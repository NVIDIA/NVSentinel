// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	sppb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	structpb "google.golang.org/protobuf/types/known/structpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// mockNormalizer allows faking normalization behavior.
// Duplicated from previous attempts as it's needed again.

const (
	ZONE                                      = "topology.kubernetes.io/zone"
	UPCOMING_MAINTENANCE                      = "compute.instances.upcomingMaintenance"
	POLL_SUCCESSFUL_ERROR_MESSAGE             = "pollSuccessful expected true, got false"
	NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE  = "Normalizer.Normalize called %d times, expected 1"
	RECEIVED_EVENT_NODE_NAME_ERROR_MESSAGE    = "Received event NodeName %s, expected %s"
	RECEIVED_EVENT_CLUSTER_NAME_ERROR_MESSAGE = "Received event ClusterName %s, expected %s"
	UNEXPECTED_ERROR_MESSAGE                  = "unexpected error: %v"
)

type mockNormalizer struct {
	NormalizeFunc func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error)
	calls         []struct {
		RawEvent       interface{}
		AdditionalInfo []interface{}
	}
}

func (m *mockNormalizer) Normalize(
	rawEvent interface{},
	additionalInfo ...interface{},
) (*model.MaintenanceEvent, error) {
	m.calls = append(m.calls, struct {
		RawEvent       interface{}
		AdditionalInfo []interface{}
	}{rawEvent, additionalInfo})
	if m.NormalizeFunc != nil {
		return m.NormalizeFunc(rawEvent, additionalInfo...)
	}
	entry, ok := rawEvent.(*logging.Entry)
	if !ok {
		return nil, fmt.Errorf("mockNormalizer: unexpected rawEvent type %T", rawEvent)
	}
	var nodeName, clusterName string
	if len(additionalInfo) > 0 {
		nodeName, _ = additionalInfo[0].(string)
	}
	if len(additionalInfo) > 1 {
		clusterName, _ = additionalInfo[1].(string)
	}
	// Default mock behavior: if nodeName or clusterName is empty (as per real normalizer's new contract), return error.
	if nodeName == "" {
		return nil, fmt.Errorf("mockNormalizer default: nodeName cannot be empty")
	}
	if clusterName == "" {
		return nil, fmt.Errorf("mockNormalizer default: clusterName cannot be empty")
	}
	return &model.MaintenanceEvent{
		EventID:     "norm-" + entry.InsertID,
		NodeName:    nodeName,
		ClusterName: clusterName,
		CSP:         model.CSPGCP,
	}, nil
}

func (m *mockNormalizer) ResetCalls() {
	m.calls = nil
}

// mockEntryIterator is a mock implementation of the EntryIterator interface.
type mockEntryIterator struct {
	entries  []*logging.Entry
	idx      int
	fetchErr error // To simulate an error from Next() itself, not iterator.Done
}

func (m *mockEntryIterator) Next() (*logging.Entry, error) {
	if m.fetchErr != nil {
		return nil, m.fetchErr // Simulate a hard error during iteration
	}
	if m.idx >= len(m.entries) {
		return nil, iterator.Done // Standard end of iteration
	}
	entry := m.entries[m.idx]
	m.idx++
	return entry, nil
}

// --- Test Suite for processLogEntries ---

const (
	testPollLogsProject      = "test-gcp-project-for-poll"
	testPollLogsZone         = "us-east1-b"
	testPollLogsInstanceName = "gke-poll-instance-1"
	testPollLogsInstanceID   = "9876543210123456789"
	testPollLogsNodeName     = "k8s-poll-node-1"
	testPollLogsLogFilter    = "resource.type=\"gce_instance\" AND protoPayload.methodName=\"compute.instances.upcomingMaintenance\""
)

// createTestGCEAuditLogEntry is a helper to create logging.Entry for tests.
// It now accepts an optional operationProducer. If provided, entry.Operation will be set.
func createTestGCEAuditLogEntry(
	t *testing.T,
	insertID, instanceID, project, zone, instanceName, methodName, statusMsg string,
	maintMetadata *computepb.UpcomingMaintenance,
	ts time.Time,
	operationProducer ...string,
) *logging.Entry {
	t.Helper()
	resource := &mrpb.MonitoredResource{
		Type:   "gce_instance",
		Labels: map[string]string{"project_id": project, "zone": zone, "instance_id": instanceID},
	}

	var auditMetadataStruct *structpb.Struct
	if maintMetadata != nil {
		jsonData, err := protojson.Marshal(maintMetadata)
		if err != nil {
			t.Fatalf("Failed to marshal UpcomingMaintenance to JSON: %v", err)
		}
		auditMetadataStruct = &structpb.Struct{}
		if err := protojson.Unmarshal(jsonData, auditMetadataStruct); err != nil {
			t.Fatalf("Failed to unmarshal JSON to structpb.Struct: %v", err)
		}
	}

	resourceNameFQN := fmt.Sprintf("projects/%s/zones/%s/instances/%s", project, zone, instanceName)
	auditLogPayload := &auditpb.AuditLog{
		ServiceName:        "compute.googleapis.com",
		MethodName:         methodName,
		ResourceName:       resourceNameFQN,
		Status:             &sppb.Status{Message: statusMsg},
		AuthenticationInfo: &auditpb.AuthenticationInfo{PrincipalEmail: "system@google.com"},
		Metadata:           auditMetadataStruct,
	}

	entry := &logging.Entry{
		InsertID:  insertID,
		LogName:   fmt.Sprintf("projects/%s/logs/cloudaudit.googleapis.com%%2Fsystem_event", project),
		Resource:  resource,
		Timestamp: ts,
		Severity:  logging.Notice,
		Payload:   auditLogPayload,
	}

	if len(operationProducer) > 0 && operationProducer[0] != "" {
		entry.Operation = &loggingpb.LogEntryOperation{
			Producer: operationProducer[0],
		}
	}

	return entry
}

func setupProcessLogEntriesTest(
	t *testing.T,
) (context.Context, context.CancelFunc, *Client, *mockNormalizer, chan model.MaintenanceEvent, *fake.Clientset) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	mockNorm := &mockNormalizer{}
	eventChan := make(chan model.MaintenanceEvent, 10)
	fakeK8sCS := fake.NewSimpleClientset()

	// logadminClient is now an interface, so we don't mock it directly here for processLogEntries tests.
	// We will pass a mockEntryIterator to processLogEntries.
	client := &Client{
		config:       config.GCPConfig{LogFilter: testPollLogsLogFilter, APIPollingIntervalSeconds: 1, Enabled: true},
		normalizer:   mockNorm,
		k8sClientset: fakeK8sCS,
		clusterName:  "test-cluster-poll",
	}
	return ctx, cancel, client, mockNorm, eventChan, fakeK8sCS
}

func TestProcessLogEntries_NoEntries(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, _ := setupProcessLogEntriesTest(t)
	defer cancel()

	mockIter := &mockEntryIterator{entries: nil}
	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-2*time.Minute),
	)

	if !pollSuccessful {
		t.Error("pollSuccessful expected true for no entries, got false")
	}
	if len(mockNorm.calls) != 0 {
		t.Errorf("Normalizer.Normalize called %d times, expected 0", len(mockNorm.calls))
	}
	select {
	case ev := <-eventChan:
		t.Errorf("eventChan received an event unexpectedly: %+v", ev)
	default:
	}
}

func TestProcessLogEntries_OneValidMappableEntry(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, fakeK8sCS := setupProcessLogEntriesTest(t)
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testPollLogsNodeName,
			Annotations: map[string]string{gcpInstanceIDAnnotation: testPollLogsInstanceID},
			Labels:      map[string]string{ZONE: testPollLogsZone},
		},
	}
	_ = fakeK8sCS.Tracker().Add(node)

	entryTime := time.Now().UTC().Add(-5 * time.Minute)
	pendingStatus := "PENDING"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &pendingStatus,
		Type:              &scheduledType,
	}
	logEntry := createTestGCEAuditLogEntry(
		t,
		"insert-map-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"pending",
		maintUpcoming,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error(POLL_SUCCESSFUL_ERROR_MESSAGE)
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}
	call := mockNorm.calls[0]
	if call.RawEvent.(*logging.Entry).InsertID != "insert-map-1" {
		t.Errorf("Normalizer called with wrong event. Got ID %s", call.RawEvent.(*logging.Entry).InsertID)
	}
	if len(call.AdditionalInfo) < 2 {
		t.Fatalf("Normalizer not called with enough additionalInfo. Got: %v", call.AdditionalInfo)
	}
	if call.AdditionalInfo[0].(string) != testPollLogsNodeName {
		t.Errorf(
			"Normalizer not called with correct nodeName. Got: %v, Expected: %s",
			call.AdditionalInfo[0],
			testPollLogsNodeName,
		)
	}
	if call.AdditionalInfo[1].(string) != client.clusterName {
		t.Errorf(
			"Normalizer not called with correct clusterName. Got: %v, Expected: %s",
			call.AdditionalInfo[1],
			client.clusterName,
		)
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-insert-map-1" {
			t.Errorf("Received event ID %s, expected norm-insert-map-1", ev.EventID)
		}
		if ev.NodeName != testPollLogsNodeName {
			t.Errorf("Received event with NodeName %s, expected %s", ev.NodeName, testPollLogsNodeName)
		}
		if ev.ClusterName != client.clusterName {
			t.Errorf("Received event with ClusterName %s, expected %s", ev.ClusterName, client.clusterName)
		}
	default:
		t.Fatal("Expected an event on eventChan, but got none")
	}
}

func TestProcessLogEntries_IteratorError(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, _ := setupProcessLogEntriesTest(t)
	defer cancel()

	mockIter := &mockEntryIterator{fetchErr: errors.New("gcp log fetch failed")}
	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-2*time.Minute),
	)

	if pollSuccessful {
		t.Error("pollSuccessful expected false due to iterator error, got true")
	}
	if len(mockNorm.calls) != 0 {
		t.Errorf("Normalizer.Normalize called %d times, expected 0", len(mockNorm.calls))
	}
}

func TestProcessLogEntries_NormalizationError(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, _ := setupProcessLogEntriesTest(t)
	defer cancel()

	entryTime := time.Now().UTC().Add(-5 * time.Minute)
	logEntry := createTestGCEAuditLogEntry(
		t,
		"norm-err-entry-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"pending",
		nil,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		return nil, errors.New("normalization failed badly")
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error("pollSuccessful expected true even with normalization error, got false")
	}
	if len(mockNorm.calls) != 1 {
		t.Errorf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}
	select {
	case ev := <-eventChan:
		t.Errorf("eventChan received an event (%+v) despite normalization error", ev)
	default:
	}
}

func TestProcessLogEntries_ContextCancelledDuringProcessing(t *testing.T) {
	ctx, cancelFunc, client, mockNorm, eventChan, _ := setupProcessLogEntriesTest(t)
	// Don't defer cancelFunc, we call it manually

	entryTime1 := time.Now().UTC().Add(-10 * time.Minute)
	entryTime2 := time.Now().UTC().Add(-5 * time.Minute)
	log1 := createTestGCEAuditLogEntry(
		t,
		"ctx-proc-1",
		"id1",
		testPollLogsProject,
		testPollLogsZone,
		"name1",
		UPCOMING_MAINTENANCE,
		"p1",
		nil,
		entryTime1,
	)
	log2 := createTestGCEAuditLogEntry(
		t,
		"ctx-proc-2",
		"id2",
		testPollLogsProject,
		testPollLogsZone,
		"name2",
		UPCOMING_MAINTENANCE,
		"p2",
		nil,
		entryTime2,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{log1, log2}}

	processedOne := make(chan struct{})
	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		if entry.InsertID == "ctx-proc-1" {
			close(processedOne)               // Signal that the first one is being processed
			time.Sleep(10 * time.Millisecond) // Give a moment for cancellation to potentially be processed
			// Now, check context if it was cancelled *during* this normalization.
			if ctx.Err() != nil {
				return nil, ctx.Err() // Propagate cancellation error
			}
		}
		return &model.MaintenanceEvent{EventID: "norm-" + entry.InsertID, CSP: model.CSPGCP}, nil
	}

	go func() {
		<-processedOne // Wait until the first event starts processing
		cancelFunc()   // Then cancel the context
	}()

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if pollSuccessful { // pollSuccessful should be false if context is cancelled mid-processing loop
		t.Error("pollSuccessful expected false due to context cancellation, got true")
	}

	if len(mockNorm.calls) != 1 {
		t.Logf(
			"Normalizer.Normalize was called %d times. Expected 1 due to context cancellation after the first began processing.",
			len(mockNorm.calls),
		)
	}

	evCount := 0
	select {
	case <-eventChan:
		evCount++
	case <-time.After(50 * time.Millisecond): // Timeout for checking channel
	}
	if evCount > 1 {
		t.Errorf("eventChan received %d events, expected at most 1 due to context cancellation", evCount)
	}
	cancelFunc() // Ensure cleanup
}

// TestProcessLogEntries_WithOperationProducer tests processing of an entry that includes Operation.Producer.
func TestProcessLogEntries_WithOperationProducer(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, fakeK8sCS := setupProcessLogEntriesTest(t)
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testPollLogsNodeName,
			Annotations: map[string]string{gcpInstanceIDAnnotation: testPollLogsInstanceID},
			Labels:      map[string]string{ZONE: testPollLogsZone},
		},
	}
	_ = fakeK8sCS.Tracker().Add(node)

	entryTime := time.Now().UTC().Add(-5 * time.Minute)
	pendingStatusString := "PENDING"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &pendingStatusString,
		Type:              &scheduledType,
	}
	logEntry := createTestGCEAuditLogEntry(
		t,
		"op-producer-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"pending",
		maintUpcoming,
		entryTime,
		UPCOMING_MAINTENANCE, /* operationProducer */
	) // Key addition
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	// Setup mock normalizer to return a specific CSPStatus for verification
	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		nodeName, _ := additionalInfo[0].(string)
		clusterName, _ := additionalInfo[1].(string)
		if nodeName == "" {
			return nil, fmt.Errorf("nodeName cannot be empty for op-producer test")
		}
		if clusterName == "" {
			return nil, fmt.Errorf("clusterName cannot be empty for op-producer test")
		}
		return &model.MaintenanceEvent{
			EventID:     "norm-" + entry.InsertID,
			NodeName:    nodeName,
			ClusterName: clusterName,
			CSP:         model.CSPGCP,
			CSPStatus:   "PENDING",
		}, nil
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error(POLL_SUCCESSFUL_ERROR_MESSAGE)
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}
	call := mockNorm.calls[0]
	if call.RawEvent.(*logging.Entry).InsertID != "op-producer-1" {
		t.Errorf("Normalizer called with wrong event. Got ID %s", call.RawEvent.(*logging.Entry).InsertID)
	}
	rawEntry := call.RawEvent.(*logging.Entry)
	if rawEntry.Operation == nil || rawEntry.Operation.Producer != UPCOMING_MAINTENANCE {
		t.Errorf("Normalizer called with event missing expected Operation.Producer. Got: %v", rawEntry.Operation)
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-op-producer-1" {
			t.Errorf("Received event ID %s, expected norm-op-producer-1", ev.EventID)
		}
		if ev.NodeName != testPollLogsNodeName {
			t.Errorf("Received event with NodeName %s, expected %s", ev.NodeName, testPollLogsNodeName)
		}
		if ev.ClusterName != client.clusterName {
			t.Errorf("Received event with ClusterName %s, expected %s", ev.ClusterName, client.clusterName)
		}
		if ev.CSPStatus != "PENDING" {
			t.Errorf("Received event with CSPStatus %s, expected %s", ev.CSPStatus, "PENDING")
		}
	default:
		t.Fatal("Expected an event on eventChan, but got none")
	}
}

// TestProcessLogEntries_UnmappableInstance tests processing an entry for an instance not found in K8s.
func TestProcessLogEntries_UnmappableInstance(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, _ := setupProcessLogEntriesTest(
		t,
	) // Deliberately not using fakeK8sCS to add node
	defer cancel()

	entryTime := time.Now().UTC().Add(-5 * time.Minute)
	pendingStatus := "PENDING"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &pendingStatus,
		Type:              &scheduledType,
	}
	// Using a distinct instance ID that won't be in the empty fakeK8sCS
	logEntry := createTestGCEAuditLogEntry(
		t,
		"unmap-1",
		"unmappable-instance-id",
		testPollLogsProject,
		testPollLogsZone,
		"unmappable-instance-name",
		UPCOMING_MAINTENANCE,
		"pending",
		maintUpcoming,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	// Normalizer should be called with an empty node name
	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		var nodeName string
		if len(additionalInfo) > 0 {
			nodeName, _ = additionalInfo[0].(string)
		}
		if nodeName != "" {
			t.Errorf("Expected Normalize to be called with empty nodeName for unmappable instance, got '%s'", nodeName)
		}
		return &model.MaintenanceEvent{
			EventID:   "norm-" + entry.InsertID,
			NodeName:  nodeName, // Should be empty
			CSP:       model.CSPGCP,
			CSPStatus: "PENDING",
		}, nil
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error("pollSuccessful expected true for unmappable instance, got false")
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-unmap-1" {
			t.Errorf("Received event ID %s, expected norm-unmap-1", ev.EventID)
		}
		if ev.NodeName != "" {
			t.Errorf("Received event with NodeName '%s', expected empty for unmappable instance", ev.NodeName)
		}
		if ev.CSPStatus != "PENDING" {
			t.Errorf("Received event CSPStatus %s, expected PENDING", ev.CSPStatus)
		}
	default:
		t.Fatal("Expected an event on eventChan for unmappable instance, but got none")
	}
}

// TestProcessLogEntries_OngoingStatus tests processing of an ONGOING maintenance event.
func TestProcessLogEntries_OngoingStatus(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, fakeK8sCS := setupProcessLogEntriesTest(t)
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testPollLogsNodeName,
			Annotations: map[string]string{gcpInstanceIDAnnotation: testPollLogsInstanceID},
			Labels:      map[string]string{ZONE: testPollLogsZone},
		},
	}
	_ = fakeK8sCS.Tracker().Add(node)

	entryTime := time.Now().UTC().Add(-5 * time.Minute)
	ongoingStatus := "ONGOING"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &ongoingStatus,
		Type:              &scheduledType,
	}
	logEntry := createTestGCEAuditLogEntry(
		t,
		"ongoing-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"ongoing",
		maintUpcoming,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		nodeName, _ := additionalInfo[0].(string)
		clusterName, _ := additionalInfo[1].(string)
		if nodeName == "" {
			return nil, fmt.Errorf("nodeName cannot be empty for ongoing status test")
		}
		if clusterName == "" {
			return nil, fmt.Errorf("clusterName cannot be empty for ongoing status test")
		}
		return &model.MaintenanceEvent{
			EventID:     "norm-" + entry.InsertID,
			NodeName:    nodeName,
			ClusterName: clusterName,
			CSP:         model.CSPGCP,
			CSPStatus:   "ONGOING", // Expected output status
		}, nil
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error(POLL_SUCCESSFUL_ERROR_MESSAGE)
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-ongoing-1" {
			t.Errorf("Received event ID %s, expected norm-ongoing-1", ev.EventID)
		}
		if ev.NodeName != testPollLogsNodeName {
			t.Errorf(RECEIVED_EVENT_NODE_NAME_ERROR_MESSAGE, ev.NodeName, testPollLogsNodeName)
		}
		if ev.ClusterName != client.clusterName {
			t.Errorf(RECEIVED_EVENT_CLUSTER_NAME_ERROR_MESSAGE, ev.ClusterName, client.clusterName)
		}
		if ev.CSPStatus != "ONGOING" {
			t.Errorf("Received event CSPStatus %s, expected ONGOING", ev.CSPStatus)
		}
	default:
		t.Fatal("Expected an ONGOING event on eventChan, but got none")
	}
}

// TestProcessLogEntries_CompletedStatus tests processing of a COMPLETED maintenance event.
func TestProcessLogEntries_CompletedStatus(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, fakeK8sCS := setupProcessLogEntriesTest(t)
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testPollLogsNodeName,
			Annotations: map[string]string{gcpInstanceIDAnnotation: testPollLogsInstanceID},
			Labels:      map[string]string{ZONE: testPollLogsZone},
		},
	}
	_ = fakeK8sCS.Tracker().Add(node)

	entryTime := time.Now().UTC().Add(-2 * time.Hour) // Event happened in the past
	completedStatus := "COMPLETED"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &completedStatus,
		Type:              &scheduledType,
	}
	logEntry := createTestGCEAuditLogEntry(
		t,
		"completed-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"completed",
		maintUpcoming,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		nodeName, _ := additionalInfo[0].(string)
		clusterName, _ := additionalInfo[1].(string)
		if nodeName == "" {
			return nil, fmt.Errorf("nodeName cannot be empty for completed status test")
		}
		if clusterName == "" {
			return nil, fmt.Errorf("clusterName cannot be empty for completed status test")
		}
		return &model.MaintenanceEvent{
			EventID:     "norm-" + entry.InsertID,
			NodeName:    nodeName,
			ClusterName: clusterName,
			CSP:         model.CSPGCP,
			CSPStatus:   "COMPLETED", // Expected output status
		}, nil
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error(POLL_SUCCESSFUL_ERROR_MESSAGE)
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-completed-1" {
			t.Errorf("Received event ID %s, expected norm-completed-1", ev.EventID)
		}
		if ev.NodeName != testPollLogsNodeName {
			t.Errorf(RECEIVED_EVENT_NODE_NAME_ERROR_MESSAGE, ev.NodeName, testPollLogsNodeName)
		}
		if ev.ClusterName != client.clusterName {
			t.Errorf(RECEIVED_EVENT_CLUSTER_NAME_ERROR_MESSAGE, ev.ClusterName, client.clusterName)
		}
		if ev.CSPStatus != "COMPLETED" {
			t.Errorf("Received event CSPStatus %s, expected COMPLETED", ev.CSPStatus)
		}
	default:
		t.Fatal("Expected a COMPLETED event on eventChan, but got none")
	}
}

// TestProcessLogEntries_CancelledStatus tests processing of a CANCELLED maintenance event.
func TestProcessLogEntries_CancelledStatus(t *testing.T) {
	ctx, cancel, client, mockNorm, eventChan, fakeK8sCS := setupProcessLogEntriesTest(t)
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testPollLogsNodeName,
			Annotations: map[string]string{gcpInstanceIDAnnotation: testPollLogsInstanceID},
			Labels:      map[string]string{ZONE: testPollLogsZone},
		},
	}
	_ = fakeK8sCS.Tracker().Add(node)

	entryTime := time.Now().UTC().Add(-30 * time.Minute)
	cancelledStatus := "CANCELLED"
	scheduledType := "SCHEDULED"
	maintUpcoming := &computepb.UpcomingMaintenance{
		MaintenanceStatus: &cancelledStatus,
		Type:              &scheduledType,
	}
	logEntry := createTestGCEAuditLogEntry(
		t,
		"cancelled-1",
		testPollLogsInstanceID,
		testPollLogsProject,
		testPollLogsZone,
		testPollLogsInstanceName,
		UPCOMING_MAINTENANCE,
		"cancelled",
		maintUpcoming,
		entryTime,
	)
	mockIter := &mockEntryIterator{entries: []*logging.Entry{logEntry}}

	mockNorm.NormalizeFunc = func(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
		entry := rawEvent.(*logging.Entry)
		nodeName, _ := additionalInfo[0].(string)
		clusterName, _ := additionalInfo[1].(string)
		if nodeName == "" {
			return nil, fmt.Errorf("nodeName cannot be empty for cancelled status test")
		}
		if clusterName == "" {
			return nil, fmt.Errorf("clusterName cannot be empty for cancelled status test")
		}
		return &model.MaintenanceEvent{
			EventID:     "norm-" + entry.InsertID,
			NodeName:    nodeName,
			ClusterName: clusterName,
			CSP:         model.CSPGCP,
			CSPStatus:   "CANCELLED", // Expected output status
		}, nil
	}

	now := time.Now().UTC()
	pollSuccessful := client.processLogEntries(
		ctx,
		mockIter,
		now,
		eventChan,
		now.Add(-time.Hour-time.Minute),
	)

	if !pollSuccessful {
		t.Error(POLL_SUCCESSFUL_ERROR_MESSAGE)
	}
	if len(mockNorm.calls) != 1 {
		t.Fatalf(NORMALIZER_NORMALIZE_CALLS_ERROR_MESSAGE, len(mockNorm.calls))
	}

	select {
	case ev := <-eventChan:
		if ev.EventID != "norm-cancelled-1" {
			t.Errorf("Received event ID %s, expected norm-cancelled-1", ev.EventID)
		}
		if ev.NodeName != testPollLogsNodeName {
			t.Errorf(RECEIVED_EVENT_NODE_NAME_ERROR_MESSAGE, ev.NodeName, testPollLogsNodeName)
		}
		if ev.ClusterName != client.clusterName {
			t.Errorf(RECEIVED_EVENT_CLUSTER_NAME_ERROR_MESSAGE, ev.ClusterName, client.clusterName)
		}
		if ev.CSPStatus != "CANCELLED" {
			t.Errorf("Received event CSPStatus %s, expected CANCELLED", ev.CSPStatus)
		}
	default:
		t.Fatal("Expected a CANCELLED event on eventChan, but got none")
	}
}

// Original tests from before for mapGCPInstanceToNodeName and GetName:
// (These should remain unchanged and are assumed to be here from previous steps)
func TestMapGCPInstanceToNodeName(t *testing.T) {
	const (
		instanceID = "1234567890123456789"
		zone       = "us-central1-a"
		nodeName   = "gpu-node-1"
	)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				gcpInstanceIDAnnotation: instanceID,
			},
			Labels: map[string]string{
				ZONE: zone,
			},
		},
	}
	fakeCS := fake.NewSimpleClientset(node)

	c := &Client{k8sClientset: fakeCS}
	got, err := c.mapGCPInstanceToNodeName(context.Background(), instanceID, map[string]string{"gcp_zone": zone})
	if err != nil {
		t.Fatalf(UNEXPECTED_ERROR_MESSAGE, err)
	}
	if got != nodeName {
		t.Fatalf("expected node %s, got %s", nodeName, got)
	}
}

func TestMapGCPInstanceToNodeNameNotFound(t *testing.T) {
	fakeCS := fake.NewSimpleClientset()
	c := &Client{k8sClientset: fakeCS}
	got, err := c.mapGCPInstanceToNodeName(context.Background(), "nonexistent", map[string]string{})
	if err != nil {
		t.Fatalf(UNEXPECTED_ERROR_MESSAGE, err)
	}
	if got != "" {
		t.Fatalf("expected empty result, got %s", got)
	}
}

func TestMapGCPInstanceToNodeNameNoZoneLabel(t *testing.T) {
	const (
		instanceID = "9876543210987654321"
		nodeName   = "node-no-zone"
	)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				gcpInstanceIDAnnotation: instanceID,
			},
		},
	}
	fakeCS := fake.NewSimpleClientset(node)
	c := &Client{k8sClientset: fakeCS}

	got, err := c.mapGCPInstanceToNodeName(context.Background(), instanceID, map[string]string{})
	if err != nil {
		t.Fatalf(UNEXPECTED_ERROR_MESSAGE, err)
	}
	if got != nodeName {
		t.Fatalf("expected %s, got %s", nodeName, got)
	}
}

func TestMapGCPInstanceToNodeNameMultipleNodes(t *testing.T) {
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unrelated-node",
			Annotations: map[string]string{
				gcpInstanceIDAnnotation: "1111111111111111111",
			},
		},
	}
	const (
		instanceID = "2222222222222222222"
		nodeName   = "target-node"
	)
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				gcpInstanceIDAnnotation: instanceID,
			},
			Labels: map[string]string{ZONE: "europe-west4-b"},
		},
	}
	fakeCS := fake.NewSimpleClientset(node1, node2)
	c := &Client{k8sClientset: fakeCS}

	got, err := c.mapGCPInstanceToNodeName(
		context.Background(),
		instanceID,
		map[string]string{"gcp_zone": "europe-west4-b"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nodeName {
		t.Fatalf("expected %s, got %s", nodeName, got)
	}
}

func TestGetName(t *testing.T) {
	c := &Client{}
	if c.GetName() != model.CSPGCP {
		t.Fatalf("GetName() expected %s, got %s", model.CSPGCP, c.GetName())
	}
}
