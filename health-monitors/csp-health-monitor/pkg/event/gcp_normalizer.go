// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package event

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// GCP Compute Engine maintenance-related method names and messages.
const (
	GCPMethodUpcomingMaintenance        = "compute.instances.upcomingMaintenance"
	GCPMethodUpcomingInfraMaintenance   = "compute.instances.upcomingInfraMaintenance"
	GCPMethodTerminateOnHostMaintenance = "compute.instances.terminateOnHostMaintenance"
	GCPMethodMigrateOnHostMaintenance   = "compute.instances.migrateOnHostMaintenance"

	// Message snippet that indicates completion of a maintenance window.
	GCPMaintenanceCompletedMsg = "Maintenance window has completed"
	// defaultUnknown is a generic placeholder for unknown ResourceType or ResourceID
	defaultUnknown = "UNKNOWN"
)

// GCPNormalizer implements the Normalizer interface for GCP Cloud Logging entries.
type GCPNormalizer struct{}

// Ensure GCPNormalizer implements the Normalizer interface.
var _ Normalizer = (*GCPNormalizer)(nil)

func safeFormatTime(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}

	return t.Format(time.RFC3339Nano)
}

func extractInstanceNameFromFQN(resourceNameFQN string) string {
	parts := strings.Split(resourceNameFQN, "/")
	if len(parts) > 0 && len(parts)-2 >= 0 && parts[len(parts)-2] == "instances" {
		return parts[len(parts)-1]
	}

	slog.Debug("Could not parse instance name from FQN", "fqn", resourceNameFQN)

	return ""
}

// extractNodeAndCluster validates additionalInfo and returns nodeName and clusterName.
// It centralises validation to keep Normalize concise.
func extractNodeAndCluster(additionalInfo []interface{}) (string, string, error) {
	if len(additionalInfo) < 2 {
		return "", "", fmt.Errorf("missing nodeName or clusterName in additionalInfo")
	}

	nodeName, ok := additionalInfo[0].(string)
	if !ok || nodeName == "" {
		return "", "", fmt.Errorf("nodeName must be a non-empty string")
	}

	clusterName, ok := additionalInfo[1].(string)
	if !ok || clusterName == "" {
		return "", "", fmt.Errorf("clusterName must be a non-empty string")
	}

	return nodeName, clusterName, nil
}

// buildBaseEvent creates a MaintenanceEvent from the log entry after basic
// validation (resource must be gce_instance).
func buildBaseEvent(entry *logging.Entry, nodeName, clusterName string) (*model.MaintenanceEvent, error) {
	event := initializeEventFromLogEntry(entry, nodeName)
	event.ClusterName = clusterName

	if event.ResourceType != "gce_instance" {
		return nil, fmt.Errorf("unsupported resource type for GCP normalization: %s", event.ResourceType)
	}

	return event, nil
}

// decodeAuditPayload extracts the audit log payload from the entry.
func decodeAuditPayload(entry *logging.Entry) (*auditpb.AuditLog, error) {
	auditLog, ok := entry.Payload.(*auditpb.AuditLog)
	if !ok {
		return nil, fmt.Errorf("unsupported payload type %T; expected *auditpb.AuditLog", entry.Payload)
	}

	return auditLog, nil
}

// Normalize converts a GCP Cloud Logging entry into a standard MaintenanceEvent.
func (n *GCPNormalizer) Normalize(
	rawEvent interface{},
	additionalInfo ...interface{},
) (*model.MaintenanceEvent, error) {
	entry, ok := rawEvent.(*logging.Entry)
	if !ok {
		return nil, fmt.Errorf("invalid type for GCP normalization: expected *logging.Entry, got %T", rawEvent)
	}

	if entry == nil {
		return nil, fmt.Errorf("cannot normalize nil LogEntry")
	}

	nodeName, clusterName, err := extractNodeAndCluster(additionalInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to extract node and cluster from additional info: %w", err)
	}

	event, err := buildBaseEvent(entry, nodeName, clusterName)
	if err != nil {
		slog.Debug("GCP Normalizer: skipping entry due to validation", "error", err)
		return nil, fmt.Errorf("failed to build base event: %w", err)
	}

	auditLog, err := decodeAuditPayload(entry)
	if err != nil {
		slog.Error("Unexpected payload type for GCP maintenance log entry", "error", err, "insertID", entry.InsertID)
		return nil, fmt.Errorf("failed to decode audit payload: %w", err)
	}

	methodName,
		rpcStatusMessage,
		parsedInstanceName := updateEventFromAuditLog(event, auditLog, entry.InsertID)

	finalizeEventStatus(event, methodName, entry.InsertID)

	slog.Info(
		"Normalized GCP event details",
		"eventID", event.EventID,
		"resourceType", event.ResourceType,
		"resourceID", event.ResourceID,
		"parsedInstanceNameForLogging", parsedInstanceName,
		"nodeName", event.NodeName,
		"maintenanceType", event.MaintenanceType,
		"cspStatus", event.CSPStatus,
		"internalStatus", event.Status,
		"scheduledStartTime", safeFormatTime(event.ScheduledStartTime),
		"scheduledEndTime", safeFormatTime(event.ScheduledEndTime),
		"actualStartTime", safeFormatTime(event.ActualStartTime),
		"actualEndTime", safeFormatTime(event.ActualEndTime),
		"methodNameOrProducer", methodName,
		"rpcStatusMsg", rpcStatusMessage,
	)

	return event, nil
}

func parseMetadataTime(timeStr string) (*time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
		return &t, nil
	}

	if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
		return &t, nil
	}

	return nil, fmt.Errorf("could not parse time string '%s' with RFC3339Nano or RFC3339", timeStr)
}

func parseMaintenanceFieldsFromProtoPayloadMetadata(metadataStruct *structpb.Struct, event *model.MaintenanceEvent) {
	if metadataStruct == nil {
		return
	}

	maintFields := metadataStruct.GetFields()

	if maintStatusVal, ok := maintFields["maintenanceStatus"]; ok {
		newCSPStatus := maintStatusVal.GetStringValue()
		// Only set if not already determined (e.g. not COMPLETED by rpcStatusMessage or other prior logic)
		if event.CSPStatus == model.CSPStatusUnknown || event.CSPStatus == "" {
			event.CSPStatus = model.ProviderStatus(newCSPStatus) // This will be PENDING or ONGOING
		}
	}

	if typeVal, okType := maintFields["type"]; okType {
		event.MaintenanceType = model.MaintenanceType(typeVal.GetStringValue())
	}

	if startTimeVal, okStart := maintFields["windowStartTime"]; okStart {
		stStr := startTimeVal.GetStringValue()
		if st, err := parseMetadataTime(stStr); err == nil {
			event.ScheduledStartTime = st
		} else {
			slog.Warn("Could not parse windowStartTime from metadata",
				"timeString", stStr,
				"eventID", event.EventID,
				"error", err)
		}
	}

	if endTimeVal, okEnd := maintFields["windowEndTime"]; okEnd {
		etStr := endTimeVal.GetStringValue()
		if et, err := parseMetadataTime(etStr); err == nil {
			event.ScheduledEndTime = et
		} else {
			slog.Warn("Could not parse windowEndTime from metadata",
				"timeString", etStr,
				"eventID", event.EventID,
				"error", err)
		}
	}
}

func isGCPMaintenanceMethod(name string) bool {
	return name == GCPMethodUpcomingMaintenance ||
		name == GCPMethodUpcomingInfraMaintenance ||
		name == GCPMethodTerminateOnHostMaintenance ||
		name == GCPMethodMigrateOnHostMaintenance
}

// --- Helper functions for Normalize ---

func initializeEventFromLogEntry(entry *logging.Entry, nodeName string) *model.MaintenanceEvent {
	event := &model.MaintenanceEvent{
		EventID:                entry.InsertID,
		CSP:                    model.CSPGCP,
		ClusterName:            "", // Default, will be overwritten by Normalize func using passed-in value
		EventReceivedTimestamp: entry.Timestamp,
		LastUpdatedTimestamp:   time.Now().UTC(),
		CSPStatus:              model.CSPStatusUnknown,
		MaintenanceType:        "",
		ResourceType:           defaultUnknown,
		ResourceID:             defaultUnknown,
		NodeName:               nodeName,
		Status:                 model.StatusDetected,
		Metadata:               make(map[string]string),
		RecommendedAction:      pb.RecommendedAction_NONE.String(),
	}

	if entry.Resource == nil {
		slog.Warn("LogEntry has no MonitoredResource block",
			"logName", entry.LogName,
			"insertID", entry.InsertID)

		return event // Return partially filled event
	}

	event.ResourceType = entry.Resource.Type

	// Since Normalize ensures ResourceType is "gce_instance", this block always executes for valid events.
	if event.ResourceType == "gce_instance" {
		// event.ResourceID = entry.Resource.Labels["instance_id"]
		// If instance_id label is missing or empty, ResourceID should remain defaultUnknown.
		if instanceIDFromLabel := entry.Resource.Labels["instance_id"]; instanceIDFromLabel != "" {
			event.ResourceID = instanceIDFromLabel
		} // Otherwise, it remains its initialized value of defaultUnknown

		if zone, ok := entry.Resource.Labels["zone"]; ok {
			event.Metadata["gcp_zone"] = zone
		}

		if projectID, ok := entry.Resource.Labels["project_id"]; ok {
			event.Metadata["gcp_project_id"] = projectID
		}
	}

	return event
}

func getParsedInstanceNameForLogging(event *model.MaintenanceEvent, auditLog *auditpb.AuditLog) string {
	// First, try extracting the instance name from the fully-qualified resource name.
	if name := extractInstanceNameFromFQN(auditLog.GetResourceName()); name != "" {
		return name
	}
	// Fallback to the numeric instance ID if the FQN didn't contain a parsable name.
	if event.ResourceID != defaultUnknown && event.ResourceID != "" {
		return event.ResourceID
	}
	// Final fallback: return the raw resource name from the audit log.
	return auditLog.GetResourceName()
}

func handleGcpCompletionMessage(event *model.MaintenanceEvent, methodName, rpcStatusMessage, entryInsertID string) {
	if event.CSPStatus == model.CSPStatusCompleted {
		return // Already completed, nothing to do
	}

	if (methodName == GCPMethodUpcomingMaintenance || methodName == GCPMethodUpcomingInfraMaintenance) &&
		strings.Contains(rpcStatusMessage, GCPMaintenanceCompletedMsg) {
		slog.Debug("GCP maintenance completion message found",
			"method", methodName,
			"insertID", entryInsertID,
			"rpcStatusMessage", rpcStatusMessage)

		event.CSPStatus = model.CSPStatusCompleted
		if event.ActualEndTime == nil {
			now := time.Now().UTC()
			event.ActualEndTime = &now
		}
	}
}

func refineGcpStatusAndType(event *model.MaintenanceEvent, methodName string) {
	if event.CSPStatus == model.CSPStatusCompleted {
		return
	}

	if !isGCPMaintenanceMethod(methodName) {
		return
	}

	// Set CSPStatus based on method name if still unknown
	if event.CSPStatus == model.CSPStatusUnknown {
		if methodName == GCPMethodUpcomingMaintenance || methodName == GCPMethodUpcomingInfraMaintenance {
			event.CSPStatus = model.CSPStatusPending
		} else {
			event.CSPStatus = model.CSPStatusActive
		}
	}
}

func updateEventFromAuditLog(
	event *model.MaintenanceEvent,
	auditLog *auditpb.AuditLog,
	entryInsertID string,
) (methodName, rpcStatusMessage, parsedInstanceNameForLogging string) {
	methodName = auditLog.GetMethodName()
	parsedInstanceNameForLogging = getParsedInstanceNameForLogging(event, auditLog)

	if auditLog.GetStatus() != nil {
		rpcStatusMessage = auditLog.GetStatus().GetMessage()
	}

	if md := auditLog.GetMetadata(); md != nil {
		parseMaintenanceFieldsFromProtoPayloadMetadata(md, event)
	}

	slog.Debug("Parsed AuditLog payload", "insertID", entryInsertID)

	refineGcpStatusAndType(event, methodName)
	handleGcpCompletionMessage(event, methodName, rpcStatusMessage, entryInsertID)

	return
}

//nolint:cyclop // single switch over CSP status; splitting would reduce clarity
func finalizeEventStatus(event *model.MaintenanceEvent, methodName string, entryInsertID string) {
	switch event.CSPStatus {
	case model.CSPStatusPending:
		event.Status = model.StatusDetected
	case model.CSPStatusOngoing, model.CSPStatusActive:
		event.Status = model.StatusMaintenanceOngoing
		if event.ActualStartTime == nil { // If ActualStartTime is not set yet
			now := time.Now().UTC()
			event.ActualStartTime = &now // Set to current time when ONGOING/ACTIVE is first observed
		}
	case model.CSPStatusCompleted:
		event.Status = model.StatusMaintenanceComplete
		if event.ActualEndTime == nil {
			now := time.Now().UTC()
			event.ActualEndTime = &now
		}
	case model.CSPStatusCancelled:
		event.Status = model.StatusCancelled
	case model.CSPStatusUnknown:
		if event.Status == "" {
			event.Status = model.StatusDetected
		}

		if isGCPMaintenanceMethod(methodName) {
			slog.Debug("CSPStatus unknown for known maintenance method; using default internal status",
				"cspStatus", event.CSPStatus,
				"method", methodName,
				"insertID", entryInsertID,
				"defaultStatus", event.Status)
		}
	default: // Handles any other unexpected CSPStatus values
		slog.Warn("Unexpected CSPStatus encountered; using default internal status",
			"cspStatus", event.CSPStatus,
			"method", methodName,
			"insertID", entryInsertID,
			"defaultStatus", model.StatusDetected)

		if event.Status == "" { // Ensure internal status is at least Detected
			event.Status = model.StatusDetected
		}
	}
}
