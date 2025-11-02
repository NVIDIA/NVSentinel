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
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	DATE_TIME         = "2023-01-15T10:30:00.123456789Z"
	DATE_TIME_RFC3339 = "2023-01-15T10:30:00Z"
	K8S_NODE_1        = "k8s-node-1"
	TEST_CLUSTER      = "test-cluster"
	TEST_INSTANCE_ERR = "projects/p/zones/z/instances/i-err"
)

func TestSafeFormatTime(t *testing.T) {
	tests := []struct {
		name     string
		input    *time.Time
		expected string
	}{
		{
			name:     "nil time",
			input:    nil,
			expected: "<nil>",
		},
		{
			name: "valid time",
			input: func() *time.Time {
				tm, _ := time.Parse(time.RFC3339Nano, DATE_TIME)
				return &tm
			}(),
			expected: DATE_TIME,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := safeFormatTime(tc.input)
			if result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestExtractInstanceNameFromFQN(t *testing.T) {
	tests := []struct {
		name     string
		fqn      string
		expected string
	}{
		{
			name:     "valid FQN",
			fqn:      "projects/my-project/zones/us-central1-a/instances/my-instance-name",
			expected: "my-instance-name",
		},
		{
			name:     "short FQN",
			fqn:      "instances/my-instance-name",
			expected: "my-instance-name",
		},
		{
			name:     "no instances part",
			fqn:      "projects/my-project/zones/us-central1-a/disks/my-disk",
			expected: "",
		},
		{
			name:     "empty FQN",
			fqn:      "",
			expected: "",
		},
		{
			name:     "FQN with trailing slash",
			fqn:      "projects/my-project/zones/us-central1-a/instances/",
			expected: "",
		},
		{
			name:     "FQN with only instances",
			fqn:      "instances/",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractInstanceNameFromFQN(tc.fqn)
			if result != tc.expected {
				t.Errorf("For FQN %q, expected %q, got %q", tc.fqn, tc.expected, result)
			}
		})
	}
}

// Helper function to validate parseMetadataTime results and reduce cognitive complexity
func validateParseMetadataTimeResult(t *testing.T, result *time.Time, err error, expectErr bool, expected *time.Time) {
	if expectErr {
		if err == nil {
			t.Errorf("Expected error, got nil")
		}
		return
	}

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
		return
	}

	if !result.Equal(*expected) {
		t.Errorf("Expected time %v, got %v", *expected, *result)
	}
}

func TestParseMetadataTime(t *testing.T) {
	tests := []struct {
		name      string
		timeStr   string
		expectErr bool
		expected  *time.Time
	}{
		{
			name:    "RFC3339Nano",
			timeStr: DATE_TIME,
			expected: func() *time.Time {
				tm, _ := time.Parse(time.RFC3339Nano, DATE_TIME)
				return &tm
			}(),
		},
		{
			name:    "RFC3339",
			timeStr: DATE_TIME_RFC3339,
			expected: func() *time.Time {
				tm, _ := time.Parse(time.RFC3339, DATE_TIME_RFC3339)
				return &tm
			}(),
		},
		{
			name:      "invalid format",
			timeStr:   "2023/01/15 10:30:00",
			expectErr: true,
		},
		{
			name:      "empty string",
			timeStr:   "",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseMetadataTime(tc.timeStr)
			validateParseMetadataTimeResult(t, result, err, tc.expectErr, tc.expected)
		})
	}
}

// Helper function to compare time pointers and reduce cognitive complexity
func compareTimePointers(t *testing.T, fieldName string, actual, expected *time.Time) {
	if (actual == nil && expected != nil) ||
		(actual != nil && expected == nil) ||
		(actual != nil && expected != nil && !actual.Equal(*expected)) {
		t.Errorf(
			"%s: expected %v, got %v",
			fieldName,
			expected,
			actual,
		)
	}
}

// Helper function to validate maintenance event fields
func validateMaintenanceEventFields(t *testing.T, actual, expected *model.MaintenanceEvent) {
	if actual.CSPStatus != expected.CSPStatus {
		t.Errorf("CSPStatus: expected %s, got %s", expected.CSPStatus, actual.CSPStatus)
	}
	if actual.MaintenanceType != expected.MaintenanceType {
		t.Errorf(
			"MaintenanceType: expected %s, got %s",
			expected.MaintenanceType,
			actual.MaintenanceType,
		)
	}
	compareTimePointers(t, "ScheduledStartTime", actual.ScheduledStartTime, expected.ScheduledStartTime)
	compareTimePointers(t, "ScheduledEndTime", actual.ScheduledEndTime, expected.ScheduledEndTime)
}

func TestParseMaintenanceFieldsFromProtoPayloadMetadata(t *testing.T) {
	timeStr := DATE_TIME_RFC3339
	expectedTime, _ := time.Parse(time.RFC3339, timeStr)

	tests := []struct {
		name           string
		metadataStruct *structpb.Struct
		initialEvent   *model.MaintenanceEvent
		expectedEvent  *model.MaintenanceEvent
	}{
		{
			name: "all fields present",
			metadataStruct: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"maintenanceStatus": structpb.NewStringValue("ONGOING"),
					"type":              structpb.NewStringValue("SCHEDULED_TEST"),
					"windowStartTime":   structpb.NewStringValue(timeStr),
					"windowEndTime":     structpb.NewStringValue(timeStr),
				},
			},
			initialEvent: &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, EventID: "test1"},
			expectedEvent: &model.MaintenanceEvent{
				CSPStatus:          "ONGOING",
				MaintenanceType:    "SCHEDULED_TEST",
				ScheduledStartTime: &expectedTime,
				ScheduledEndTime:   &expectedTime,
				EventID:            "test1",
			},
		},
		{
			name: "CSPStatus already set (e.g. COMPLETED), should not overwrite from metadata",
			metadataStruct: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"maintenanceStatus": structpb.NewStringValue("ONGOING"), // metadata says ONGOING
				},
			},
			initialEvent: &model.MaintenanceEvent{
				CSPStatus: model.CSPStatusCompleted,
				EventID:   "test-completed",
			}, // initial is COMPLETED
			expectedEvent: &model.MaintenanceEvent{
				CSPStatus: model.CSPStatusCompleted,
				EventID:   "test-completed",
			}, // should remain COMPLETED
		},
		{
			name: "CSPStatus is empty, should set from metadata",
			metadataStruct: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"maintenanceStatus": structpb.NewStringValue("PENDING"),
				},
			},
			initialEvent:  &model.MaintenanceEvent{CSPStatus: "", EventID: "test-empty"},
			expectedEvent: &model.MaintenanceEvent{CSPStatus: "PENDING", EventID: "test-empty"},
		},
		{
			name:           "nil metadata struct",
			metadataStruct: nil,
			initialEvent:   &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, EventID: "test2"},
			expectedEvent:  &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, EventID: "test2"},
		},
		{
			name: "partial fields - only type",
			metadataStruct: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"type": structpb.NewStringValue("PARTIAL_TYPE"),
				},
			},
			initialEvent: &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, EventID: "test3"},
			expectedEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusUnknown,
				MaintenanceType: "PARTIAL_TYPE",
				EventID:         "test3",
			},
		},
		{
			name: "invalid time format in metadata",
			metadataStruct: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"windowStartTime": structpb.NewStringValue("invalid-time"),
				},
			},
			initialEvent:  &model.MaintenanceEvent{EventID: "test4"},
			expectedEvent: &model.MaintenanceEvent{EventID: "test4"}, // time fields should remain nil
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parseMaintenanceFieldsFromProtoPayloadMetadata(tc.metadataStruct, tc.initialEvent)
			validateMaintenanceEventFields(t, tc.initialEvent, tc.expectedEvent)
		})
	}
}

// Helper to create a basic audit.AuditLog for testing
func newTestAuditLog(methodName, resourceName, rpcStatusMsg string, metadata *structpb.Struct) *audit.AuditLog {
	al := &audit.AuditLog{
		MethodName:   methodName,
		ResourceName: resourceName,
	}
	if rpcStatusMsg != "" {
		al.Status = &status.Status{Message: rpcStatusMsg}
	}
	if metadata != nil {
		al.Metadata = metadata
	}
	return al
}

// Helper to create a basic logging.Entry for testing
func newTestLogEntry(
	insertID string,
	payload interface{},
	resourceType string,
	resourceLabels map[string]string,
) *logging.Entry {
	entry := &logging.Entry{
		InsertID:  insertID,
		Timestamp: time.Now(),
		Payload:   payload,
	}
	if resourceType != "" || len(resourceLabels) > 0 {
		entry.Resource = &mrpb.MonitoredResource{
			Type:   resourceType,
			Labels: resourceLabels,
		}
	}
	return entry
}

func TestInitializeEventFromLogEntry(t *testing.T) {
	testTime := time.Now().UTC()
	tests := []struct {
		name           string
		entry          *logging.Entry
		nodeName       string
		expectedEvent  *model.MaintenanceEvent
		compareTime    bool // whether to compare time fields that are set to time.Now()
		ignoreMetadata bool // whether to ignore metadata map in comparison
	}{
		{
			name: "gce_instance with all labels and nodeName",
			entry: newTestLogEntry("insert1", nil, "gce_instance", map[string]string{
				"instance_id": "12345",
				"zone":        "us-central1-a",
				"project_id":  "my-proj",
			}),
			nodeName: K8S_NODE_1,
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert1",
				CSP:               model.CSPGCP,
				ResourceType:      "gce_instance",
				ResourceID:        "12345",
				NodeName:          K8S_NODE_1,
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
				Metadata:          map[string]string{"gcp_zone": "us-central1-a", "gcp_project_id": "my-proj"},
			},
			compareTime: true,
		},
		{
			name:     "gce_instance without nodeName",
			entry:    newTestLogEntry("insert2", nil, "gce_instance", map[string]string{"instance_id": "67890"}),
			nodeName: "",
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert2",
				CSP:               model.CSPGCP,
				ResourceType:      "gce_instance",
				ResourceID:        "67890",
				NodeName:          "",
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
				Metadata:          map[string]string{},
			},
			compareTime: true,
		},
		{
			name: "other resource type - now results in defaultUnknown ResourceID by initializeEventFromLogEntry",
			entry: newTestLogEntry(
				"insert3",
				nil,
				"gcs_bucket",
				map[string]string{"id": "bucket-id", "project_id": "proj"},
			),
			nodeName: "node2",
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert3",
				CSP:               model.CSPGCP,
				ResourceType:      "gcs_bucket",
				ResourceID:        defaultUnknown,
				NodeName:          "node2",
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
				Metadata:          map[string]string{},
			},
			compareTime: true,
		},
		{
			name:     "other resource type with name label - now results in defaultUnknown ResourceID",
			entry:    newTestLogEntry("insert4", nil, "gke_cluster", map[string]string{"name": "cluster-name"}),
			nodeName: "node3",
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert4",
				CSP:               model.CSPGCP,
				ResourceType:      "gke_cluster",
				ResourceID:        defaultUnknown,
				NodeName:          "node3",
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
				Metadata:          map[string]string{},
			},
			compareTime: true,
		},
		{
			name:     "resource type with no id or name label - ResourceID remains defaultUnknown",
			entry:    newTestLogEntry("insert5", nil, "some_resource", map[string]string{"other_label": "value"}),
			nodeName: "node4",
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert5",
				CSP:               model.CSPGCP,
				ResourceType:      "some_resource",
				ResourceID:        defaultUnknown,
				NodeName:          "node4",
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
				Metadata:          map[string]string{},
			},
			compareTime: true,
		},
		{
			name: "nil entry.Resource",
			entry: func() *logging.Entry {
				e := newTestLogEntry("insert6", nil, "", nil)
				e.Resource = nil
				return e
			}(),
			nodeName: "node5",
			expectedEvent: &model.MaintenanceEvent{
				EventID:           "insert6",
				CSP:               model.CSPGCP,
				ResourceType:      defaultUnknown,
				ResourceID:        defaultUnknown,
				NodeName:          "node5",
				CSPStatus:         model.CSPStatusUnknown,
				MaintenanceType:   "",
				Status:            model.StatusDetected,
				RecommendedAction: pb.RecommendedAction_NONE.String(),
			},
			compareTime:    true,
			ignoreMetadata: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Capture current time before calling the function for comparison
			callTime := time.Now().UTC()
			if tc.entry != nil {
				tc.entry.Timestamp = testTime // Standardize timestamp for predictable EventReceivedTimestamp
			}

			result := initializeEventFromLogEntry(tc.entry, tc.nodeName)

			// Set time fields for expectedEvent if compareTime is true
			tc.expectedEvent.EventReceivedTimestamp = testTime
			tc.expectedEvent.LastUpdatedTimestamp = callTime // Approximate, will check with tolerance

			// For nil entry.Resource case, Metadata will be non-nil empty map
			if tc.ignoreMetadata {
				if result.Metadata == nil {
					t.Errorf("Expected non-nil Metadata, got nil")
				}
				tc.expectedEvent.Metadata = result.Metadata // Assume correct if non-nil
			}

			if !reflect.DeepEqual(result.Metadata, tc.expectedEvent.Metadata) {
				t.Errorf("Metadata: expected %v, got %v", tc.expectedEvent.Metadata, result.Metadata)
			}

			// Compare time fields with tolerance
			if !result.EventReceivedTimestamp.Equal(tc.expectedEvent.EventReceivedTimestamp) {
				t.Errorf(
					"EventReceivedTimestamp: expected %v, got %v",
					tc.expectedEvent.EventReceivedTimestamp,
					result.EventReceivedTimestamp,
				)
			}
			if tc.compareTime && result.LastUpdatedTimestamp.Sub(tc.expectedEvent.LastUpdatedTimestamp) > time.Second {
				t.Errorf(
					"LastUpdatedTimestamp: expected around %v, got %v (diff too large)",
					tc.expectedEvent.LastUpdatedTimestamp,
					result.LastUpdatedTimestamp,
				)
			}

			// Nullify time fields for DeepEqual comparison of the rest of the struct
			result.EventReceivedTimestamp = time.Time{}
			tc.expectedEvent.EventReceivedTimestamp = time.Time{}
			result.LastUpdatedTimestamp = time.Time{}
			tc.expectedEvent.LastUpdatedTimestamp = time.Time{}
			// Nullify metadata for DeepEqual if already compared or ignored
			result.Metadata = nil
			tc.expectedEvent.Metadata = nil

			if !reflect.DeepEqual(result, tc.expectedEvent) {
				t.Errorf("Expected event %v, got %v", tc.expectedEvent, result)
			}
		})
	}
}

// Helper function to validate CSP status
func validateCSPStatus(t *testing.T, actual, expected model.ProviderStatus) {
	if actual != expected {
		t.Errorf("Expected CSPStatus %s, got %s", expected, actual)
	}
}

// Helper function to validate expected ActualEndTime when it should be set
func validateExpectedActualEndTime(t *testing.T, actualEndTime, originalEndTime *time.Time) {
	if actualEndTime == nil {
		t.Error("Expected ActualEndTime to be set, but it was nil")
		return
	}
	if originalEndTime == nil && actualEndTime.IsZero() {
		t.Error("ActualEndTime was set, but to a zero value unexpectedly")
	}
}

// Helper function to validate ActualEndTime when it should remain unchanged
func validateUnchangedActualEndTime(t *testing.T, actualEndTime, originalEndTime *time.Time) {
	if actualEndTime == originalEndTime {
		return // Same pointer, so unchanged
	}

	// Check if both are nil (unchanged)
	if originalEndTime == nil && actualEndTime == nil {
		return
	}

	// Check if both are non-nil and equal
	if originalEndTime != nil && actualEndTime != nil && originalEndTime.Equal(*actualEndTime) {
		return
	}

	// If we get here, the time was changed when it shouldn't have been
	t.Errorf("Expected ActualEndTime to remain unchanged (%v), but got %v", originalEndTime, actualEndTime)
}

// Helper function to validate ActualEndTime based on expectation
func validateActualEndTime(t *testing.T, initialEvent *model.MaintenanceEvent, originalEndTime *time.Time, expectActualEndTime bool) {
	if expectActualEndTime {
		validateExpectedActualEndTime(t, initialEvent.ActualEndTime, originalEndTime)
	} else {
		validateUnchangedActualEndTime(t, initialEvent.ActualEndTime, originalEndTime)
	}
}

func TestHandleGcpCompletionMessage(t *testing.T) {
	tests := []struct {
		name                string
		initialEvent        *model.MaintenanceEvent
		methodName          string
		rpcStatusMessage    string
		entryInsertID       string
		expectedCSPStatus   model.ProviderStatus
		expectActualEndTime bool
	}{
		{
			name:                "completion message for upcomingMaintenance",
			initialEvent:        &model.MaintenanceEvent{CSPStatus: model.CSPStatusPending},
			methodName:          GCPMethodUpcomingMaintenance,
			rpcStatusMessage:    "Instance is active. " + GCPMaintenanceCompletedMsg + ". Some details.",
			entryInsertID:       "id1",
			expectedCSPStatus:   model.CSPStatusCompleted,
			expectActualEndTime: true,
		},
		{
			name:                "completion message for upcomingInfraMaintenance",
			initialEvent:        &model.MaintenanceEvent{CSPStatus: model.CSPStatusPending},
			methodName:          GCPMethodUpcomingInfraMaintenance,
			rpcStatusMessage:    GCPMaintenanceCompletedMsg,
			entryInsertID:       "id2",
			expectedCSPStatus:   model.CSPStatusCompleted,
			expectActualEndTime: true,
		},
		{
			name:                "no completion message",
			initialEvent:        &model.MaintenanceEvent{CSPStatus: model.CSPStatusPending},
			methodName:          GCPMethodUpcomingMaintenance,
			rpcStatusMessage:    "Instance is active.",
			entryInsertID:       "id3",
			expectedCSPStatus:   model.CSPStatusPending,
			expectActualEndTime: false,
		},
		{
			name:                "wrong method name",
			initialEvent:        &model.MaintenanceEvent{CSPStatus: model.CSPStatusActive},
			methodName:          GCPMethodMigrateOnHostMaintenance,
			rpcStatusMessage:    GCPMaintenanceCompletedMsg,
			entryInsertID:       "id4",
			expectedCSPStatus:   model.CSPStatusActive,
			expectActualEndTime: false,
		},
		{
			name: "already completed status",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:     model.CSPStatusCompleted,
				ActualEndTime: &time.Time{},
			}, // Already has an end time
			methodName:          GCPMethodUpcomingMaintenance,
			rpcStatusMessage:    GCPMaintenanceCompletedMsg,
			entryInsertID:       "id5",
			expectedCSPStatus:   model.CSPStatusCompleted,
			expectActualEndTime: true, // Should still be true as initial event had it
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originalActualEndTime := tc.initialEvent.ActualEndTime
			handleGcpCompletionMessage(tc.initialEvent, tc.methodName, tc.rpcStatusMessage, tc.entryInsertID)

			validateCSPStatus(t, tc.initialEvent.CSPStatus, tc.expectedCSPStatus)
			validateActualEndTime(t, tc.initialEvent, originalActualEndTime, tc.expectActualEndTime)
		})
	}
}

func TestRefineGcpStatusAndType(t *testing.T) {
	tests := []struct {
		name                    string
		initialEvent            *model.MaintenanceEvent
		methodName              string
		expectedCSPStatus       model.ProviderStatus
		expectedMaintenanceType model.MaintenanceType
	}{
		{
			name: "upcoming maintenance, unknown status, type initially UNSCHEDULED from metadata (simulated)",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusUnknown,
				MaintenanceType: model.TypeUnscheduled,
			},
			methodName:              GCPMethodUpcomingMaintenance,
			expectedCSPStatus:       model.CSPStatusPending,
			expectedMaintenanceType: model.TypeUnscheduled,
		},
		{
			name:                    "upcoming infra maintenance, unknown status, type initially empty",
			initialEvent:            &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, MaintenanceType: ""},
			methodName:              GCPMethodUpcomingInfraMaintenance,
			expectedCSPStatus:       model.CSPStatusPending,
			expectedMaintenanceType: "",
		},
		{
			name:                    "terminate on host maintenance, unknown status, type initially empty",
			initialEvent:            &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, MaintenanceType: ""},
			methodName:              GCPMethodTerminateOnHostMaintenance,
			expectedCSPStatus:       model.CSPStatusActive,
			expectedMaintenanceType: "",
		},
		{
			name: "migrate on host maintenance, unknown status, specific type already set from metadata",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusUnknown,
				MaintenanceType: "CUSTOM_TYPE",
			},
			methodName:              GCPMethodMigrateOnHostMaintenance,
			expectedCSPStatus:       model.CSPStatusActive,
			expectedMaintenanceType: "CUSTOM_TYPE",
		},
		{
			name: "CSPStatus already PENDING, type initially UNSCHEDULED (simulated from metadata)",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusPending,
				MaintenanceType: model.TypeUnscheduled,
			},
			methodName:              GCPMethodUpcomingMaintenance,
			expectedCSPStatus:       model.CSPStatusPending,
			expectedMaintenanceType: model.TypeUnscheduled,
		},
		{
			name: "MaintenanceType already SCHEDULED (simulated from metadata)",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusUnknown,
				MaintenanceType: model.TypeScheduled,
			},
			methodName:              GCPMethodUpcomingMaintenance,
			expectedCSPStatus:       model.CSPStatusPending,
			expectedMaintenanceType: model.TypeScheduled,
		},
		{
			name: "event already COMPLETED, should do nothing to type",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusCompleted,
				MaintenanceType: model.TypeUnscheduled,
			},
			methodName:              GCPMethodUpcomingMaintenance,
			expectedCSPStatus:       model.CSPStatusCompleted,
			expectedMaintenanceType: model.TypeUnscheduled,
		},
		{
			name: "non-maintenance method, should do nothing to type",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusUnknown,
				MaintenanceType: model.TypeUnscheduled,
			},
			methodName:              "compute.instances.get",
			expectedCSPStatus:       model.CSPStatusUnknown,
			expectedMaintenanceType: model.TypeUnscheduled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refineGcpStatusAndType(tc.initialEvent, tc.methodName)
			if tc.initialEvent.CSPStatus != tc.expectedCSPStatus {
				t.Errorf("Expected CSPStatus %s, got %s", tc.expectedCSPStatus, tc.initialEvent.CSPStatus)
			}
			if tc.initialEvent.MaintenanceType != tc.expectedMaintenanceType {
				t.Errorf(
					"Expected MaintenanceType %s, got %s",
					tc.expectedMaintenanceType,
					tc.initialEvent.MaintenanceType,
				)
			}
		})
	}
}

// Helper function to validate internal status
func validateInternalStatus(t *testing.T, actual, expected model.InternalStatus) {
	if actual != expected {
		t.Errorf("Expected internal Status %s, got %s", expected, actual)
	}
}

// Helper function to validate expected ActualStartTime when it should be set
func validateExpectedActualStartTime(t *testing.T, actualStartTime, originalStartTime *time.Time) {
	if actualStartTime == nil {
		t.Error("Expected ActualStartTime to be set, but it was nil")
		return
	}
	if originalStartTime == nil && actualStartTime.IsZero() {
		t.Error("ActualStartTime was set by finalize, but to a zero value unexpectedly")
	}
}

// Helper function to validate ActualStartTime when it should remain unchanged
func validateUnchangedActualStartTime(t *testing.T, actualStartTime, originalStartTime *time.Time) {
	// If both are the same pointer, it's unchanged
	if actualStartTime == originalStartTime {
		return
	}

	// If original was not nil and current is not nil, check if they're equal
	if originalStartTime != nil && actualStartTime != nil {
		if !originalStartTime.Equal(*actualStartTime) {
			t.Errorf("ActualStartTime was already set and should not have changed, expected %v, got %v", originalStartTime, actualStartTime)
		}
		return
	}

	// If original was nil but current is not nil, it was set when it shouldn't have been
	if originalStartTime == nil && actualStartTime != nil {
		t.Errorf("ActualStartTime was not expected to be set by finalize, but it was. Got %v", actualStartTime)
	}
}

// Helper function to validate ActualStartTime based on expectation
func validateActualStartTime(t *testing.T, initialEvent *model.MaintenanceEvent, originalStartTime *time.Time, expectActualStartTime bool) {
	if expectActualStartTime {
		validateExpectedActualStartTime(t, initialEvent.ActualStartTime, originalStartTime)
	} else {
		validateUnchangedActualStartTime(t, initialEvent.ActualStartTime, originalStartTime)
	}
}

// Helper function to validate expected ActualEndTime when it should be set
func validateExpectedActualEndTimeForFinalize(t *testing.T, actualEndTime, originalEndTime *time.Time) {
	if actualEndTime == nil {
		t.Error("Expected ActualEndTime to be set, but it was nil")
		return
	}
	if originalEndTime == nil && actualEndTime.IsZero() {
		t.Error("ActualEndTime was set by finalize, but to a zero value unexpectedly")
	}
}

// Helper function to validate ActualEndTime when it should remain unchanged
func validateUnchangedActualEndTimeForFinalize(t *testing.T, actualEndTime, originalEndTime *time.Time) {
	// If both are the same pointer, it's unchanged
	if actualEndTime == originalEndTime {
		return
	}

	// If original was not nil and current is not nil, check if they're equal
	if originalEndTime != nil && actualEndTime != nil {
		if !originalEndTime.Equal(*actualEndTime) {
			t.Errorf("ActualEndTime was already set and should not have changed, expected %v, got %v", originalEndTime, actualEndTime)
		}
		return
	}

	// If original was nil but current is not nil, it was set when it shouldn't have been
	if originalEndTime == nil && actualEndTime != nil {
		t.Errorf("ActualEndTime was not expected to be set by finalize, but it was. Got %v", actualEndTime)
	}
}

// Helper function to validate ActualEndTime based on expectation for finalize
func validateActualEndTimeForFinalize(t *testing.T, initialEvent *model.MaintenanceEvent, originalEndTime *time.Time, expectActualEndTime bool) {
	if expectActualEndTime {
		validateExpectedActualEndTimeForFinalize(t, initialEvent.ActualEndTime, originalEndTime)
	} else {
		validateUnchangedActualEndTimeForFinalize(t, initialEvent.ActualEndTime, originalEndTime)
	}
}

func TestFinalizeEventStatus(t *testing.T) {
	tests := []struct {
		name                   string
		initialEvent           *model.MaintenanceEvent
		methodName             string // For logging if unmapped status for known method
		entryInsertID          string
		expectedInternalStatus model.InternalStatus
		expectActualStartTime  bool
		expectActualEndTime    bool
	}{
		{
			name:                   "CSPStatus PENDING",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: model.CSPStatusPending, Status: ""},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusDetected,
		},
		{
			name:                   "CSPStatus ONGOING, no actualStartTime yet",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: model.CSPStatusOngoing, Status: ""},
			methodName:             GCPMethodMigrateOnHostMaintenance,
			expectedInternalStatus: model.StatusMaintenanceOngoing,
			expectActualStartTime:  true,
		},
		{
			name: "CSPStatus ACTIVE, actualStartTime already set",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:       model.CSPStatusActive,
				Status:          "",
				ActualStartTime: func() *time.Time { t := time.Now().Add(-time.Hour); return &t }(),
			},
			methodName:             GCPMethodTerminateOnHostMaintenance,
			expectedInternalStatus: model.StatusMaintenanceOngoing,
			expectActualStartTime:  true, // will be true as it was already set
		},
		{
			name:                   "CSPStatus COMPLETED, no actualEndTime yet",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: model.CSPStatusCompleted, Status: ""},
			methodName:             GCPMethodUpcomingMaintenance, // from message
			expectedInternalStatus: model.StatusMaintenanceComplete,
			expectActualEndTime:    true,
		},
		{
			name: "CSPStatus COMPLETED, actualEndTime already set",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus:     model.CSPStatusCompleted,
				Status:        "",
				ActualEndTime: func() *time.Time { t := time.Now().Add(-time.Minute); return &t }(),
			},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusMaintenanceComplete,
			expectActualEndTime:    true, // will be true as it was already set
		},
		{
			name:                   "CSPStatus CANCELLED",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: model.CSPStatusCancelled, Status: ""},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusCancelled,
		},
		{
			name:                   "CSPStatus UNKNOWN, internal status empty, known method",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: model.CSPStatusUnknown, Status: ""},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusDetected, // Defaults to Detected if internal status is empty
		},
		{
			name: "CSPStatus UNKNOWN, internal status already set",
			initialEvent: &model.MaintenanceEvent{
				CSPStatus: model.CSPStatusUnknown,
				Status:    model.StatusDetected,
			},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusDetected, // Should not change if already set
		},
		{
			name:                   "Unmapped CSPStatus for known maintenance method",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: "SOME_NEW_STATUS", Status: ""},
			methodName:             GCPMethodUpcomingMaintenance,
			expectedInternalStatus: model.StatusDetected, // Defaults to Detected
		},
		{
			name:                   "Unmapped CSPStatus for non-maintenance method",
			initialEvent:           &model.MaintenanceEvent{CSPStatus: "SOME_OTHER_STATUS", Status: ""},
			methodName:             "compute.instances.list",
			expectedInternalStatus: model.StatusDetected, // Defaults to Detected
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originalActualStartTime := tc.initialEvent.ActualStartTime
			originalActualEndTime := tc.initialEvent.ActualEndTime

			finalizeEventStatus(tc.initialEvent, tc.methodName, tc.entryInsertID)

			validateInternalStatus(t, tc.initialEvent.Status, tc.expectedInternalStatus)
			validateActualStartTime(t, tc.initialEvent, originalActualStartTime, tc.expectActualStartTime)
			validateActualEndTimeForFinalize(t, tc.initialEvent, originalActualEndTime, tc.expectActualEndTime)
		})
	}
}

// Helper function to validate basic event fields
func validateGCPNormalizerEventFields(t *testing.T, result, expected *model.MaintenanceEvent) {
	if result.EventID != expected.EventID {
		t.Errorf("EventID: expected %q, got %q", expected.EventID, result.EventID)
	}
	if result.CSP != expected.CSP {
		t.Errorf("CSP: expected %q, got %q", expected.CSP, result.CSP)
	}
	if result.NodeName != expected.NodeName {
		t.Errorf("NodeName: expected %q, got %q", expected.NodeName, result.NodeName)
	}
	if result.ResourceType != expected.ResourceType {
		t.Errorf("ResourceType: expected %q, got %q", expected.ResourceType, result.ResourceType)
	}
	if result.ResourceID != expected.ResourceID {
		t.Errorf("ResourceID: expected %q, got %q", expected.ResourceID, result.ResourceID)
	}
	if result.MaintenanceType != expected.MaintenanceType {
		t.Errorf(
			"MaintenanceType: expected %q, got %q",
			expected.MaintenanceType,
			result.MaintenanceType,
		)
	}
	if result.CSPStatus != expected.CSPStatus {
		t.Errorf("CSPStatus: expected %q, got %q", expected.CSPStatus, result.CSPStatus)
	}
	if result.Status != expected.Status {
		t.Errorf("Status: expected %q, got %q", expected.Status, result.Status)
	}
	if result.ClusterName != expected.ClusterName {
		t.Errorf("ClusterName: expected %q, got %q", expected.ClusterName, result.ClusterName)
	}
}

// Helper function to validate scheduled times
func validateScheduledTimes(t *testing.T, result, expected *model.MaintenanceEvent) {
	compareTimePointers(t, "ScheduledStartTime", result.ScheduledStartTime, expected.ScheduledStartTime)
	compareTimePointers(t, "ScheduledEndTime", result.ScheduledEndTime, expected.ScheduledEndTime)
}

// Helper function to validate actual times based on status
func validateActualTimes(t *testing.T, result, expected *model.MaintenanceEvent) {
	if expected.Status == model.StatusMaintenanceOngoing && result.ActualStartTime == nil {
		t.Errorf("Expected ActualStartTime to be set for ONGOING status, but was nil")
	}
	if expected.Status == model.StatusMaintenanceComplete && result.ActualEndTime == nil {
		t.Errorf("Expected ActualEndTime to be set for COMPLETED status, but was nil")
	}
	// If status is not ongoing/complete, actual times should be nil unless already set from a previous state
	if expected.Status != model.StatusMaintenanceOngoing &&
		expected.Status != model.StatusMaintenanceComplete {
		if result.ActualStartTime != nil && expected.ActualStartTime == nil {
			t.Errorf(
				"ActualStartTime was set (%v) but not expected for status %s",
				result.ActualStartTime,
				expected.Status,
			)
		}
		if result.ActualEndTime != nil && expected.ActualEndTime == nil {
			t.Errorf(
				"ActualEndTime was set (%v) but not expected for status %s",
				result.ActualEndTime,
				expected.Status,
			)
		}
	}
}

// Helper function to validate timestamps
func validateTimestamps(t *testing.T, result *model.MaintenanceEvent, rawEvent interface{}) {
	// Check EventReceivedTimestamp is set (should be close to baseTime used in entry creation)
	if entry, ok := rawEvent.(*logging.Entry); ok && entry != nil {
		if result.EventReceivedTimestamp.Sub(entry.Timestamp) > time.Millisecond {
			t.Errorf(
				"EventReceivedTimestamp %v is not close to entry.Timestamp %v",
				result.EventReceivedTimestamp,
				entry.Timestamp,
			)
		}
	}
	// Check LastUpdatedTimestamp is recent
	if time.Since(result.LastUpdatedTimestamp) > 5*time.Second {
		t.Errorf("LastUpdatedTimestamp %v is too old", result.LastUpdatedTimestamp)
	}
}

// Helper function to handle error cases
func handleErrorCase(t *testing.T, err error, expectErr bool) bool {
	if expectErr {
		if err == nil {
			t.Errorf("Expected error, got nil")
		}
		return true // Skip further validation
	}
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
		return true // Skip further validation
	}
	return false // Continue with validation
}

// Test case struct for GCP Normalizer tests
type gcpNormalizerTestCase struct {
	name                string
	rawEvent            interface{}
	additionalInfo      []interface{}
	expectedEvent       *model.MaintenanceEvent
	expectErr           bool
	checkActualTimes    bool
	checkScheduledTimes bool
}

// Helper function to create scheduled maintenance metadata
func createScheduledMaintenanceMetadata(baseTime time.Time, maintenanceType, status string) *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"type":              structpb.NewStringValue(maintenanceType),
			"windowStartTime":   structpb.NewStringValue(baseTime.Format(time.RFC3339Nano)),
			"windowEndTime":     structpb.NewStringValue(baseTime.Add(time.Hour).Format(time.RFC3339Nano)),
			"maintenanceStatus": structpb.NewStringValue(status),
		},
	}
}

// Helper function to create basic maintenance metadata
func createBasicMaintenanceMetadata(status string) *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"maintenanceStatus": structpb.NewStringValue(status),
		},
	}
}

// Helper function to create metadata with type and status
func createMaintenanceMetadataWithType(maintenanceType, status string) *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"maintenanceStatus": structpb.NewStringValue(status),
			"type":              structpb.NewStringValue(maintenanceType),
		},
	}
}

// Helper function to create expected maintenance event
func createExpectedMaintenanceEvent(eventID, nodeName, resourceID string, maintenanceType model.MaintenanceType, cspStatus model.ProviderStatus, status model.InternalStatus, clusterName string, scheduledStart, scheduledEnd *time.Time) *model.MaintenanceEvent {
	return &model.MaintenanceEvent{
		EventID:            eventID,
		CSP:                model.CSPGCP,
		NodeName:           nodeName,
		ResourceType:       "gce_instance",
		ResourceID:         resourceID,
		MaintenanceType:    maintenanceType,
		CSPStatus:          cspStatus,
		Status:             status,
		ScheduledStartTime: scheduledStart,
		ScheduledEndTime:   scheduledEnd,
		ClusterName:        clusterName,
	}
}

// Helper function to run a single test case
func runGCPNormalizerTestCase(t *testing.T, normalizer *GCPNormalizer, baseTime time.Time, tc gcpNormalizerTestCase) {
	t.Run(tc.name, func(t *testing.T) {
		// For tests that set specific timestamps in payload, ensure tc.entry.Timestamp is consistent
		if entry, ok := tc.rawEvent.(*logging.Entry); ok && entry != nil {
			entry.Timestamp = baseTime // Standardize for EventReceivedTimestamp field
		}

		result, err := normalizer.Normalize(tc.rawEvent, tc.additionalInfo...)

		if handleErrorCase(t, err, tc.expectErr) {
			return
		}

		if result == nil {
			t.Errorf("Expected non-nil event, got nil")
			return
		}

		validateGCPNormalizerEventFields(t, result, tc.expectedEvent)

		if tc.checkScheduledTimes {
			validateScheduledTimes(t, result, tc.expectedEvent)
		}

		if tc.checkActualTimes {
			validateActualTimes(t, result, tc.expectedEvent)
		}

		validateTimestamps(t, result, tc.rawEvent)
	})
}

func TestGCPNormalizer_Normalize_ErrorCases(t *testing.T) {
	normalizer := &GCPNormalizer{}
	baseTime := time.Date(2023, 1, 15, 10, 0, 0, 0, time.UTC)

	errorTests := []gcpNormalizerTestCase{
		{
			name:      "nil rawEvent",
			rawEvent:  nil,
			expectErr: true,
		},
		{
			name:      "invalid rawEvent type",
			rawEvent:  "not a log entry",
			expectErr: true,
		},
		{
			name: "unsupported payload type",
			rawEvent: newTestLogEntry("errpayload1",
				"not an audit log payload",
				"gce_instance", map[string]string{"instance_id": "id-err"}),
			additionalInfo: []interface{}{"k8s-node-err", "test-cluster-errpayload"},
			expectErr:      true,
		},
		{
			name: "error - missing nodeName in additionalInfo",
			rawEvent: newTestLogEntry("err_no_nodename",
				newTestAuditLog(GCPMethodUpcomingMaintenance, TEST_INSTANCE_ERR, "", nil),
				"gce_instance", map[string]string{"instance_id": "id-err-nn"}),
			additionalInfo: []interface{}{},
			expectErr:      true,
		},
		{
			name: "error - empty nodeName in additionalInfo",
			rawEvent: newTestLogEntry("err_empty_nodename",
				newTestAuditLog(GCPMethodUpcomingMaintenance, TEST_INSTANCE_ERR, "", nil),
				"gce_instance", map[string]string{"instance_id": "id-err-en"}),
			additionalInfo: []interface{}{"", TEST_CLUSTER},
			expectErr:      true,
		},
		{
			name: "error - missing clusterName in additionalInfo",
			rawEvent: newTestLogEntry("err_no_clustername",
				newTestAuditLog(GCPMethodUpcomingMaintenance, TEST_INSTANCE_ERR, "", nil),
				"gce_instance", map[string]string{"instance_id": "id-err-cn"}),
			additionalInfo: []interface{}{"k8s-node-valid"},
			expectErr:      true,
		},
		{
			name: "error - empty clusterName in additionalInfo",
			rawEvent: newTestLogEntry("err_empty_clustername",
				newTestAuditLog(GCPMethodUpcomingMaintenance, TEST_INSTANCE_ERR, "", nil),
				"gce_instance", map[string]string{"instance_id": "id-err-ecn"}),
			additionalInfo: []interface{}{"k8s-node-valid", ""},
			expectErr:      true,
		},
		{
			name: "error - unsupported resource type (not gce_instance)",
			rawEvent: newTestLogEntry("err_wrong_type",
				newTestAuditLog(GCPMethodUpcomingMaintenance, "projects/p/zones/z/disks/d-1", "", nil),
				"gcs_disk", map[string]string{"disk_id": "id-disk"}),
			additionalInfo: []interface{}{"k8s-node-disk", "test-cluster-disk"},
			expectErr:      true,
		},
	}

	for _, tc := range errorTests {
		runGCPNormalizerTestCase(t, normalizer, baseTime, tc)
	}
}

func TestGCPNormalizer_Normalize_ScheduledMaintenance(t *testing.T) {
	normalizer := &GCPNormalizer{}
	baseTime := time.Date(2023, 1, 15, 10, 0, 0, 0, time.UTC)
	endTime := baseTime.Add(time.Hour)

	scheduledTests := []gcpNormalizerTestCase{
		{
			name: "upcoming maintenance - PENDING - Type from Metadata",
			rawEvent: newTestLogEntry("up1",
				newTestAuditLog(GCPMethodUpcomingMaintenance, "projects/p/zones/z/instances/i-1", "",
					createScheduledMaintenanceMetadata(baseTime, string(model.TypeScheduled), string(model.CSPStatusPending))),
				"gce_instance", map[string]string{"instance_id": "id-123"}),
			additionalInfo:      []interface{}{K8S_NODE_1, TEST_CLUSTER},
			expectedEvent:       createExpectedMaintenanceEvent("up1", K8S_NODE_1, "id-123", model.TypeScheduled, model.CSPStatusPending, model.StatusDetected, TEST_CLUSTER, &baseTime, &endTime),
			checkScheduledTimes: true,
		},
		{
			name: "upcoming maintenance - PENDING - Type UNSCHEDULED from Metadata",
			rawEvent: newTestLogEntry("up_unsched",
				newTestAuditLog(GCPMethodUpcomingMaintenance, "projects/p/zones/z/instances/i-unsched", "",
					createScheduledMaintenanceMetadata(baseTime, string(model.TypeUnscheduled), string(model.CSPStatusPending))),
				"gce_instance", map[string]string{"instance_id": "id-up-unsched"}),
			additionalInfo:      []interface{}{"k8s-node-up-unsched", "test-cluster-unsched"},
			expectedEvent:       createExpectedMaintenanceEvent("up_unsched", "k8s-node-up-unsched", "id-up-unsched", model.TypeUnscheduled, model.CSPStatusPending, model.StatusDetected, "test-cluster-unsched", &baseTime, &endTime),
			checkScheduledTimes: true,
		},
	}

	for _, tc := range scheduledTests {
		runGCPNormalizerTestCase(t, normalizer, baseTime, tc)
	}
}

func TestGCPNormalizer_Normalize_OngoingMaintenance(t *testing.T) {
	normalizer := &GCPNormalizer{}
	baseTime := time.Date(2023, 1, 15, 10, 0, 0, 0, time.UTC)

	ongoingTests := []gcpNormalizerTestCase{
		{
			name: "migrate on host maintenance - ONGOING - No Type in Metadata",
			rawEvent: newTestLogEntry("mig1",
				newTestAuditLog(GCPMethodMigrateOnHostMaintenance, "projects/p/zones/z/instances/i-2", "",
					createBasicMaintenanceMetadata(string(model.CSPStatusActive))),
				"gce_instance", map[string]string{"instance_id": "id-456"}),
			additionalInfo:   []interface{}{"k8s-node-2", "test-cluster-mig"},
			expectedEvent:    createExpectedMaintenanceEvent("mig1", "k8s-node-2", "id-456", "", model.CSPStatusActive, model.StatusMaintenanceOngoing, "test-cluster-mig", nil, nil),
			checkActualTimes: true,
		},
		{
			name: "ONGOING from metadata - Type from metadata",
			rawEvent: newTestLogEntry("meta_ongoing",
				newTestAuditLog("some.other.Method", "projects/p/zones/z/instances/i-meta", "",
					createMaintenanceMetadataWithType(string(model.TypeScheduled), string(model.CSPStatusOngoing))),
				"gce_instance", map[string]string{"instance_id": "id-meta-ongoing"}),
			additionalInfo:   []interface{}{"k8s-node-meta-ongoing", "test-cluster-meta"},
			expectedEvent:    createExpectedMaintenanceEvent("meta_ongoing", "k8s-node-meta-ongoing", "id-meta-ongoing", model.TypeScheduled, model.CSPStatusOngoing, model.StatusMaintenanceOngoing, "test-cluster-meta", nil, nil),
			checkActualTimes: true,
		},
	}

	for _, tc := range ongoingTests {
		runGCPNormalizerTestCase(t, normalizer, baseTime, tc)
	}
}

func TestGCPNormalizer_Normalize_CompletedMaintenance(t *testing.T) {
	normalizer := &GCPNormalizer{}
	baseTime := time.Date(2023, 1, 15, 10, 0, 0, 0, time.UTC)

	completedTests := []gcpNormalizerTestCase{
		{
			name: "maintenance completion message - COMPLETED - No Type in Metadata",
			rawEvent: newTestLogEntry(
				"comp1",
				newTestAuditLog(
					GCPMethodUpcomingMaintenance,
					"projects/p/zones/z/instances/i-3",
					GCPMaintenanceCompletedMsg,
					nil,
				),
				"gce_instance",
				map[string]string{"instance_id": "id-789"},
			),
			additionalInfo:   []interface{}{"k8s-node-3", "test-cluster-comp"},
			expectedEvent:    createExpectedMaintenanceEvent("comp1", "k8s-node-3", "id-789", "", model.CSPStatusCompleted, model.StatusMaintenanceComplete, "test-cluster-comp", nil, nil),
			checkActualTimes: true,
		},
	}

	for _, tc := range completedTests {
		runGCPNormalizerTestCase(t, normalizer, baseTime, tc)
	}
}

func TestGCPNormalizer_Normalize_SpecialCases(t *testing.T) {
	normalizer := &GCPNormalizer{}
	baseTime := time.Date(2023, 1, 15, 10, 0, 0, 0, time.UTC)

	specialTests := []gcpNormalizerTestCase{
		{
			name: "gce_instance - resource name parsing - No Type in Metadata - ResourceID becomes defaultUnknown",
			rawEvent: newTestLogEntry(
				"fqn_test",
				newTestAuditLog(
					GCPMethodUpcomingMaintenance,
					"projects/test-proj/zones/us-west1-b/instances/instance-from-fqn",
					"",
					nil,
				),
				"gce_instance",
				map[string]string{},
			),
			additionalInfo: []interface{}{"k8s-node-fqn", "test-cluster-fqn"},
			expectedEvent:  createExpectedMaintenanceEvent("fqn_test", "k8s-node-fqn", defaultUnknown, "", model.CSPStatusPending, model.StatusDetected, "test-cluster-fqn", nil, nil),
		},
	}

	for _, tc := range specialTests {
		runGCPNormalizerTestCase(t, normalizer, baseTime, tc)
	}
}
