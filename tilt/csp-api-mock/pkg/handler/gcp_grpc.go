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

package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"csp-api-mock/pkg/store"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/genproto/googleapis/cloud/audit"
	logtypepb "google.golang.org/genproto/googleapis/logging/type"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GCPLoggingServer struct {
	loggingpb.UnimplementedLoggingServiceV2Server
	store *store.EventStore
}

func NewGCPLoggingServer(eventStore *store.EventStore) *GCPLoggingServer {
	return &GCPLoggingServer{store: eventStore}
}

func (s *GCPLoggingServer) ListLogEntries(
	ctx context.Context,
	req *loggingpb.ListLogEntriesRequest,
) (*loggingpb.ListLogEntriesResponse, error) {
	// Track poll requests for test synchronization
	s.store.IncrementPollCount(store.CSPGCP)

	startTime, endTime := s.parseTimestampFilter(req.GetFilter())
	events := s.store.ListByCSP(store.CSPGCP)

	log.Printf("GCP gRPC: ListLogEntries called - filter=%q, startTime=%v, endTime=%v, totalEvents=%d",
		req.GetFilter(), startTime.Format(time.RFC3339Nano), endTime.Format(time.RFC3339Nano), len(events))

	var entries []*loggingpb.LogEntry
	for _, event := range events {
		eventTime := event.UpdatedAt
		if eventTime.IsZero() {
			eventTime = event.CreatedAt
		}

		// Apply startTime filtering (mirrors real GCP API behavior).
		if !startTime.IsZero() && !eventTime.After(startTime) {
			log.Printf("GCP gRPC: FILTERED event ID=%s (eventTime %v not after startTime %v)",
				event.ID, eventTime.Format(time.RFC3339Nano), startTime.Format(time.RFC3339Nano))
			continue
		}

		// Skip endTime filtering for test reliability.
		// In tests, events are created at approximately the same time as polls,
		// and can cause race conditions at the boundary. The real GCP API wouldn't have
		// this issue because events exist before the monitor polls.
		_ = endTime

		if entry := s.eventToLogEntry(event); entry != nil {
			log.Printf("GCP gRPC: Returning event ID=%s, instanceID=%s, status=%s",
				event.ID, event.InstanceID, event.Status)
			entries = append(entries, entry)
		}
	}

	log.Printf("GCP gRPC: ListLogEntries returning %d entries", len(entries))
	return &loggingpb.ListLogEntriesResponse{Entries: entries}, nil
}

func (s *GCPLoggingServer) parseTimestampFilter(filter string) (startTime, endTime time.Time) {
	if idx := strings.Index(filter, `timestamp > "`); idx != -1 {
		startIdx := idx + len(`timestamp > "`)
		if endIdx := strings.Index(filter[startIdx:], `"`); endIdx != -1 {
			ts := filter[startIdx : startIdx+endIdx]
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				startTime = t
			} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
				startTime = t
			}
		}
	}

	if idx := strings.Index(filter, `timestamp <= "`); idx != -1 {
		startIdx := idx + len(`timestamp <= "`)
		if endIdx := strings.Index(filter[startIdx:], `"`); endIdx != -1 {
			ts := filter[startIdx : startIdx+endIdx]
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				endTime = t
			} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
				endTime = t
			}
		}
	}

	return startTime, endTime
}

func (s *GCPLoggingServer) eventToLogEntry(event *store.MaintenanceEvent) *loggingpb.LogEntry {
	methodName := GCPMethodUpcomingMaintenance
	if event.EventTypeCode != "" {
		methodName = event.EventTypeCode
	}

	maintenanceStatus := event.Status
	if maintenanceStatus == "" {
		maintenanceStatus = "PENDING"
	}

	maintenanceType := event.MaintenanceType
	if maintenanceType == "" {
		maintenanceType = "SCHEDULED"
	}

	timestamp := event.UpdatedAt
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	instanceName := event.InstanceID
	if event.NodeName != "" {
		instanceName = event.NodeName
	}

	resourceNameFQN := fmt.Sprintf("projects/%s/zones/%s/instances/%s",
		event.ProjectID, event.Zone, instanceName)

	var statusMessage string
	if strings.ToUpper(maintenanceStatus) == "COMPLETE" || strings.ToUpper(maintenanceStatus) == "COMPLETED" {
		statusMessage = "Maintenance window has completed for this instance. All maintenance notifications on the instance have been removed."
	} else if event.Description != "" {
		statusMessage = event.Description
	} else {
		statusMessage = fmt.Sprintf("Maintenance %s for %s", maintenanceStatus, instanceName)
	}

	auditLog := &audit.AuditLog{
		ServiceName:  "compute.googleapis.com",
		MethodName:   methodName,
		ResourceName: resourceNameFQN,
		AuthenticationInfo: &audit.AuthenticationInfo{
			PrincipalEmail: "system@google.com",
		},
		Status: &spb.Status{Message: statusMessage},
	}

	isComplete := strings.ToUpper(maintenanceStatus) == "COMPLETE" || strings.ToUpper(maintenanceStatus) == "COMPLETED"

	if !isComplete {
		metadata := make(map[string]*structpb.Value)
		metadata["@type"] = structpb.NewStringValue("type.googleapis.com/google.cloud.compute.v1.UpcomingMaintenance")
		metadata["maintenanceStatus"] = structpb.NewStringValue(strings.ToUpper(maintenanceStatus))
		metadata["type"] = structpb.NewStringValue(maintenanceType)
		metadata["canReschedule"] = structpb.NewBoolValue(strings.ToUpper(maintenanceStatus) == "PENDING")

		if event.ScheduledStart != nil {
			metadata["windowStartTime"] = structpb.NewStringValue(event.ScheduledStart.Format(time.RFC3339))
		}
		if event.ScheduledEnd != nil {
			metadata["windowEndTime"] = structpb.NewStringValue(event.ScheduledEnd.Format(time.RFC3339))
		}
		if latestStart, ok := event.Metadata["latestWindowStartTime"]; ok {
			metadata["latestWindowStartTime"] = structpb.NewStringValue(latestStart)
		} else if event.ScheduledStart != nil {
			metadata["latestWindowStartTime"] = structpb.NewStringValue(event.ScheduledStart.Add(2 * time.Hour).Format(time.RFC3339))
		}

		auditLog.Metadata = &structpb.Struct{Fields: metadata}
	}

	protoPayload, err := anypb.New(auditLog)
	if err != nil {
		return nil
	}

	operationID := event.ID
	if opID, ok := event.Metadata["operationId"]; ok && opID != "" {
		operationID = opID
	}

	isPending := strings.ToUpper(maintenanceStatus) == "PENDING"

	return &loggingpb.LogEntry{
		LogName:   fmt.Sprintf("projects/%s/logs/cloudaudit.googleapis.com%%2Fsystem_event", event.ProjectID),
		InsertId:  event.ID,
		Timestamp: timestamppb.New(timestamp),
		Severity:  logtypepb.LogSeverity_NOTICE,
		Resource: &monitoredres.MonitoredResource{
			Type: "gce_instance",
			Labels: map[string]string{
				"instance_id": event.InstanceID,
				"zone":        event.Zone,
				"project_id":  event.ProjectID,
			},
		},
		Payload: &loggingpb.LogEntry_ProtoPayload{ProtoPayload: protoPayload},
		Operation: &loggingpb.LogEntryOperation{
			Id:       operationID,
			Producer: GCPMethodUpcomingMaintenance,
			First:    isPending,
			Last:     isComplete,
		},
	}
}
