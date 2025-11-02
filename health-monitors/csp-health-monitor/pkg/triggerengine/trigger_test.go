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

package trigger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// MockDatastore is a mock implementation of the datastore.Store interface
type MockDatastore struct {
	mock.Mock
}

func (m *MockDatastore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	args := m.Called(ctx, event)
	return args.Error(0)
}

func (m *MockDatastore) FindEventsToTriggerQuarantine(
	ctx context.Context,
	triggerTimeLimit time.Duration,
) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, triggerTimeLimit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *MockDatastore) FindEventsToTriggerHealthy(
	ctx context.Context,
	healthyDelay time.Duration,
) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, healthyDelay)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *MockDatastore) UpdateEventStatus(ctx context.Context, eventID string, newStatus model.InternalStatus) error {
	args := m.Called(ctx, eventID, newStatus)
	return args.Error(0)
}

func (m *MockDatastore) GetLastProcessedEventTimestampByCSP(
	ctx context.Context,
	clusterName string,
	cspType model.CSP,
	cspNameForLog string,
) (time.Time, bool, error) {
	args := m.Called(ctx, clusterName, cspType, cspNameForLog)
	return args.Get(0).(time.Time), args.Bool(1), args.Error(2)
}

func (m *MockDatastore) FindLatestActiveEventByNodeAndType(
	ctx context.Context,
	nodeName string,
	maintenanceType model.MaintenanceType,
	statuses []model.InternalStatus,
) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName, maintenanceType, statuses)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*model.MaintenanceEvent), args.Bool(1), args.Error(2)
}

func (m *MockDatastore) FindLatestOngoingEventByNode(
	ctx context.Context,
	nodeName string,
) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*model.MaintenanceEvent), args.Bool(1), args.Error(2)
}

func (m *MockDatastore) FindActiveEventsByStatuses(ctx context.Context, csp model.CSP, statuses []string) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, csp, statuses)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

// MockUDSClient is a mock implementation of the pb.PlatformConnectorClient interface
type MockUDSClient struct {
	mock.Mock
}

func (m *MockUDSClient) HealthEventOccurredV1(
	ctx context.Context,
	in *pb.HealthEvents,
	opts ...grpc.CallOption,
) (*emptypb.Empty, error) {
	args := m.Called(ctx, in, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*emptypb.Empty), args.Error(1)
}

func newTestConfig() *config.Config {
	return &config.Config{
		MaintenanceEventPollIntervalSeconds:       60,
		TriggerQuarantineWorkflowTimeLimitMinutes: 30,
		PostMaintenanceHealthyDelayMinutes:        15,
		NodeReadinessTimeoutMinutes:               10,
		ClusterName:                               "test-cluster",
	}
}

// createMockClientWithReadyNodes returns a mocked Kubernetes clientset with the given node names marked Ready.
func createMockClientWithReadyNodes(nodeNames ...string) *k8sfake.Clientset {
	objs := []runtime.Object{}
	for _, n := range nodeNames {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: n},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:               corev1.NodeReady,
						Status:             corev1.ConditionTrue,
						LastHeartbeatTime:  metav1.Now(),
						LastTransitionTime: metav1.Now(),
					},
				},
			},
		}
		objs = append(objs, node)
	}

	return k8sfake.NewSimpleClientset(objs...)
}

// createMockClientWithNotReadyNode returns a fake clientset with a single node set to NotReady.
func createMockClientWithNotReadyNode(nodeName string) *k8sfake.Clientset {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	return k8sfake.NewSimpleClientset(node)
}

func TestNewEngine(t *testing.T) {
	cfg := newTestConfig()
	mStore := new(MockDatastore)
	mUDSClient := new(MockUDSClient)
	mockClient := createMockClientWithReadyNodes()

	engine := NewEngine(cfg, mStore, mUDSClient, mockClient)

	assert.NotNil(t, engine)
	assert.Equal(t, cfg, engine.config)
	assert.Equal(t, mStore, engine.store)
	assert.Equal(t, mUDSClient, engine.udsClient)
	assert.Equal(t, mockClient, engine.k8sClient)
	assert.Equal(t, time.Duration(cfg.MaintenanceEventPollIntervalSeconds)*time.Second, engine.pollInterval)
}

func TestMapMaintenanceEventToHealthEvent(t *testing.T) {
	cfg := newTestConfig()
	mStore := new(MockDatastore)     // Not strictly needed for this func, but engine needs it
	mUDSClient := new(MockUDSClient) // Not strictly needed for this func, but engine needs it
	engine := NewEngine(cfg, mStore, mUDSClient, nil)

	tests := []struct {
		name          string
		event         model.MaintenanceEvent
		isHealthy     bool
		isFatal       bool
		message       string
		expectError   bool
		expectedEvent *pb.HealthEvent // Only check key fields, ignore GeneratedTimestamp
	}{
		{
			name: "Valid quarantine event",
			event: model.MaintenanceEvent{
				EventID:           "event-1",
				ResourceType:      "gce_instance",
				ResourceID:        "instance-123",
				NodeName:          "node-a",
				RecommendedAction: pb.RecommendedAction_RESTART_VM.String(),
				Metadata:          map[string]string{"key": "value"},
			},
			isHealthy:   false,
			isFatal:     true,
			message:     maintenanceScheduledMessage,
			expectError: false,
			expectedEvent: &pb.HealthEvent{
				Agent:             "csp-health-monitor",
				ComponentClass:    "gce_instance",
				CheckName:         "CSPMaintenance",
				IsFatal:           true,
				IsHealthy:         false,
				Message:           maintenanceScheduledMessage,
				RecommendedAction: pb.RecommendedAction_RESTART_VM,
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "gce_instance", EntityValue: "instance-123"},
				},
				Metadata: map[string]string{"key": "value"},
				NodeName: "node-a",
			},
		},
		{
			name: "Valid healthy event",
			event: model.MaintenanceEvent{
				EventID:           "event-2",
				ResourceType:      "EC2",
				ResourceID:        "i-abcdef",
				NodeName:          "node-b",
				RecommendedAction: "NONE", // Test string version of NONE
			},
			isHealthy:   true,
			isFatal:     false,
			message:     maintenanceCompletedMessage,
			expectError: false,
			expectedEvent: &pb.HealthEvent{
				Agent:             "csp-health-monitor",
				ComponentClass:    "EC2",
				CheckName:         "CSPMaintenance",
				IsFatal:           false,
				IsHealthy:         true,
				Message:           maintenanceCompletedMessage,
				RecommendedAction: pb.RecommendedAction_NONE,
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "EC2", EntityValue: "i-abcdef"},
				},
				NodeName: "node-b",
			},
		},
		{
			name: "Missing ResourceType",
			event: model.MaintenanceEvent{
				EventID:    "event-3",
				ResourceID: "id",
				NodeName:   "node-c",
			},
			isHealthy:   false,
			isFatal:     true,
			message:     "test",
			expectError: true,
		},
		{
			name: "Missing ResourceID",
			event: model.MaintenanceEvent{
				EventID:      "event-4",
				ResourceType: "type",
				NodeName:     "node-d",
			},
			isHealthy:   false,
			isFatal:     true,
			message:     "test",
			expectError: true,
		},
		{
			name: "Missing NodeName",
			event: model.MaintenanceEvent{
				EventID:      "event-5",
				ResourceType: "type",
				ResourceID:   "id",
			},
			isHealthy:   false,
			isFatal:     true,
			message:     "test",
			expectError: true,
		},
		{
			name: "Unknown RecommendedAction",
			event: model.MaintenanceEvent{
				EventID:           "event-6",
				ResourceType:      "gce_instance",
				ResourceID:        "instance-789",
				NodeName:          "node-e",
				RecommendedAction: "INVALID_ACTION",
			},
			isHealthy:   false,
			isFatal:     true,
			message:     maintenanceScheduledMessage,
			expectError: false,
			expectedEvent: &pb.HealthEvent{
				Agent:             "csp-health-monitor",
				ComponentClass:    "gce_instance",
				CheckName:         "CSPMaintenance",
				IsFatal:           true,
				IsHealthy:         false,
				Message:           maintenanceScheduledMessage,
				RecommendedAction: pb.RecommendedAction_NONE, // Defaults to NONE
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "gce_instance", EntityValue: "instance-789"},
				},
				NodeName: "node-e",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualEvent, err := engine.mapMaintenanceEventToHealthEvent(tc.event, tc.isHealthy, tc.isFatal, tc.message)

			if tc.expectError {
				assert.Error(t, err)
				assert.Nil(t, actualEvent)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, actualEvent)
				// Compare critical fields, GeneratedTimestamp will differ, so set to expected if not nil
				if actualEvent != nil {
					tc.expectedEvent.GeneratedTimestamp = actualEvent.GeneratedTimestamp
				}
				assert.Equal(t, tc.expectedEvent, actualEvent)
			}
		})
	}
}

func TestIsRetryableGRPCError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{
			name:   "Nil error",
			err:    nil,
			expect: false,
		},
		{
			name:   "Non-gRPC error",
			err:    errors.New("some random error"),
			expect: false,
		},
		{
			name:   "gRPC Unavailable error",
			err:    status.Error(codes.Unavailable, "server unavailable"),
			expect: true,
		},
		{
			name:   "gRPC Internal error",
			err:    status.Error(codes.Internal, "internal server error"),
			expect: false,
		},
		{
			name:   "gRPC DeadlineExceeded error",
			err:    status.Error(codes.DeadlineExceeded, "deadline exceeded"),
			expect: false,
		},
		{
			name:   "gRPC NotFound error",
			err:    status.Error(codes.NotFound, "not found"),
			expect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, isRetryableGRPCError(tc.err))
		})
	}
}

func TestProcessAndSendTrigger(t *testing.T) {
	ctx := context.Background()
	cfg := newTestConfig()

	defaultEvent := model.MaintenanceEvent{
		EventID:           "event-default",
		NodeName:          "node-1",
		ResourceType:      "gce_instance",
		ResourceID:        "instance-abc",
		RecommendedAction: pb.RecommendedAction_NONE.String(),
	}

	tests := []struct {
		name                  string
		event                 model.MaintenanceEvent
		triggerType           string
		isHealthy             bool
		isFatal               bool
		message               string
		targetDBStatus        model.InternalStatus
		setupMocks            func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus)
		expectError           bool
		expectedErrorContains string
		verifyMocks           func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient)
	}{
		{
			name:           "Quarantine success",
			event:          defaultEvent,
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil).
					Once()
				mStore.On("UpdateEventStatus", ctx, currentEvent.EventID, currentTargetDBStatus).Return(nil).Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertExpectations(t)
			},
		},
		{
			name:           "Healthy success",
			event:          defaultEvent,
			triggerType:    healthyTriggerType,
			isHealthy:      true,
			isFatal:        false,
			message:        maintenanceCompletedMessage,
			targetDBStatus: model.StatusHealthyTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil).
					Once()
				mStore.On("UpdateEventStatus", ctx, currentEvent.EventID, currentTargetDBStatus).Return(nil).Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertExpectations(t)
			},
		},
		{
			name: "Missing NodeName",
			event: model.MaintenanceEvent{
				EventID:      "event-no-node",
				ResourceType: "test",
				ResourceID:   "id",
			}, // NodeName is empty
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				// No calls expected to UDS or DB
			},
			expectError:           true,
			expectedErrorContains: "missing NodeName",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything, mock.Anything)
				mStore.AssertNotCalled(t, "UpdateEventStatus", mock.Anything, mock.Anything, mock.Anything)
			},
		},
		{
			name: "Mapping error (missing ResourceType)",
			event: model.MaintenanceEvent{
				EventID:    "event-map-err",
				NodeName:   "node-map-err",
				ResourceID: "id",
			}, // ResourceType empty
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				// No calls expected to UDS or DB
			},
			expectError:           true,
			expectedErrorContains: "missing required fields",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything, mock.Anything)
				mStore.AssertNotCalled(t, "UpdateEventStatus", mock.Anything, mock.Anything, mock.Anything)
			},
		},
		{
			name:           "UDS send fails non-retryable",
			event:          defaultEvent,
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Internal, "internal UDS error")).
					Once()

				// UpdateEventStatus should not be called
			},
			expectError:           true,
			expectedErrorContains: "failed to send quarantine health event via UDS",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertNotCalled(t, "UpdateEventStatus", mock.Anything, mock.Anything, mock.Anything)
			},
		},
		{
			name:           "UDS send fails with retryable then succeeds",
			event:          defaultEvent,
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unavailable, "UDS temporarily unavailable")).
					Once()

					// First attempt fails
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil).
					Once()

					// Second attempt succeeds
				mStore.On("UpdateEventStatus", ctx, currentEvent.EventID, currentTargetDBStatus).Return(nil).Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertExpectations(t)
			},
		},
		{
			name:           "UDS send fails after all retries (timeout)",
			event:          defaultEvent,
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				// udsMaxRetries is the original const value from trigger.go (which is 5)
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unavailable, "UDS unavailable")).
					Times(udsMaxRetries)
			},
			expectError:           true,
			expectedErrorContains: fmt.Sprintf("failed to send health event after %d retries (timeout)", udsMaxRetries),
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertNotCalled(t, "UpdateEventStatus", mock.Anything, mock.Anything, mock.Anything)
			},
		},
		{
			name:           "DB update fails after successful UDS send",
			event:          defaultEvent,
			triggerType:    quarantineTriggerType,
			isHealthy:      false,
			isFatal:        true,
			message:        maintenanceScheduledMessage,
			targetDBStatus: model.StatusQuarantineTriggered,
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient, currentEvent model.MaintenanceEvent, currentTargetDBStatus model.InternalStatus) {
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil).
					Once()
				mStore.On("UpdateEventStatus", ctx, currentEvent.EventID, currentTargetDBStatus).
					Return(errors.New("DB update failed")).
					Once()
			},
			expectError:           true,
			expectedErrorContains: "failed to update event status post-quarantine-trigger",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mUDSClient.AssertExpectations(t)
				mStore.AssertExpectations(t)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mStore := new(MockDatastore)
			mUDSClient := new(MockUDSClient)
			engine := NewEngine(cfg, mStore, mUDSClient, nil)

			tc.setupMocks(mStore, mUDSClient, tc.event, tc.targetDBStatus)

			err := engine.processAndSendTrigger(
				ctx,
				tc.event,
				tc.triggerType,
				tc.isHealthy,
				tc.isFatal,
				tc.message,
				tc.targetDBStatus,
			)

			if tc.expectError {
				assert.Error(t, err)
				if tc.expectedErrorContains != "" {
					assert.Contains(t, err.Error(), tc.expectedErrorContains)
				}
			} else {
				assert.NoError(t, err)
			}

			if tc.verifyMocks != nil {
				tc.verifyMocks(t, mStore, mUDSClient)
			}
		})
	}
}

func TestCheckAndTriggerEvents(t *testing.T) {
	ctx := context.Background()
	cfg := newTestConfig()

	defaultQuarantineEvent := model.MaintenanceEvent{
		EventID: "q-event-1", NodeName: "node-q1", ResourceType: "test", ResourceID: "q-id1", RecommendedAction: pb.RecommendedAction_NONE.String(),
	}
	defaultHealthyEvent := model.MaintenanceEvent{
		EventID: "h-event-1", NodeName: "node-h1", ResourceType: "test", ResourceID: "h-id1", RecommendedAction: pb.RecommendedAction_NONE.String(),
	}

	tests := []struct {
		name                  string
		setupMocks            func(mStore *MockDatastore, mUDSClient *MockUDSClient)
		expectError           bool
		expectedErrorContains string
		verifyMocks           func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient)
	}{
		{
			name: "No events found",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.On("FindEventsToTriggerQuarantine", ctx, time.Duration(cfg.TriggerQuarantineWorkflowTimeLimitMinutes)*time.Minute).
					Return([]model.MaintenanceEvent{}, nil).
					Once()
				mStore.On("FindEventsToTriggerHealthy", ctx, time.Duration(cfg.PostMaintenanceHealthyDelayMinutes)*time.Minute).
					Return([]model.MaintenanceEvent{}, nil).
					Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
				mUDSClient.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything, mock.Anything)
			},
		},
		{
			name: "One quarantine event, success",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{defaultQuarantineEvent}, nil).
					Once()
				mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{}, nil).
					Once()
				// processAndSendTrigger mocks for defaultQuarantineEvent
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.MatchedBy(func(events *pb.HealthEvents) bool {
					return len(events.Events) == 1 && events.Events[0].NodeName == defaultQuarantineEvent.NodeName &&
						events.Events[0].IsFatal
				}), mock.Anything).Return(&emptypb.Empty{}, nil).Once()
				mStore.On("UpdateEventStatus", ctx, defaultQuarantineEvent.EventID, model.StatusQuarantineTriggered).
					Return(nil).
					Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
				mUDSClient.AssertExpectations(t)
			},
		},
		{
			name: "One healthy event, success",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{}, nil).
					Once()
				mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{defaultHealthyEvent}, nil).
					Once()
				// processAndSendTrigger mocks for defaultHealthyEvent
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.MatchedBy(func(events *pb.HealthEvents) bool {
					return len(events.Events) == 1 && events.Events[0].NodeName == defaultHealthyEvent.NodeName &&
						events.Events[0].IsHealthy
				}), mock.Anything).Return(&emptypb.Empty{}, nil).Once()
				mStore.On("UpdateEventStatus", ctx, defaultHealthyEvent.EventID, model.StatusHealthyTriggered).
					Return(nil).
					Once()
			},
			expectError: false,
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
				mUDSClient.AssertExpectations(t)
			},
		},
		{
			name: "Error finding quarantine events",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).
					Return(nil, errors.New("db find quarantine error")).
					Once()

				// FindEventsToTriggerHealthy should not be called if the first find fails and returns error
			},
			expectError:           true,
			expectedErrorContains: "failed to query for quarantine triggers",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
				mStore.AssertNotCalled(t, "FindEventsToTriggerHealthy", mock.Anything, mock.Anything)
			},
		},
		{
			name: "Error finding healthy events",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{}, nil).
					Once()
				mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).
					Return(nil, errors.New("db find healthy error")).
					Once()
			},
			expectError:           true,
			expectedErrorContains: "failed to query for healthy triggers",
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
			},
		},
		{
			name: "Error during quarantine trigger processing, healthy processing continues",
			setupMocks: func(mStore *MockDatastore, mUDSClient *MockUDSClient) {
				eventWithNoNode := model.MaintenanceEvent{
					EventID:      "q-no-node",
					ResourceType: "test",
					ResourceID:   "q-nn",
				} // quarantine trigger will fail
				mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{eventWithNoNode}, nil).
					Once()

				mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).
					Return([]model.MaintenanceEvent{defaultHealthyEvent}, nil).
					Once()
				// Mocks for successful healthy event
				mUDSClient.On("HealthEventOccurredV1", ctx, mock.MatchedBy(func(events *pb.HealthEvents) bool {
					return len(events.Events) == 1 && events.Events[0].NodeName == defaultHealthyEvent.NodeName &&
						events.Events[0].IsHealthy
				}), mock.Anything).Return(&emptypb.Empty{}, nil).Once()
				mStore.On("UpdateEventStatus", ctx, defaultHealthyEvent.EventID, model.StatusHealthyTriggered).
					Return(nil).
					Once()
			},
			expectError: false, // The checkAndTriggerEvents itself doesn't return error for individual trigger fails
			verifyMocks: func(t *testing.T, mStore *MockDatastore, mUDSClient *MockUDSClient) {
				mStore.AssertExpectations(t)
				mUDSClient.AssertExpectations(t) // UDS for healthy, not for failed quarantine
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mStore := new(MockDatastore)
			mUDSClient := new(MockUDSClient)
			mockClient := createMockClientWithReadyNodes("node-q1", "node-h1", "q-no-node")
			engine := NewEngine(cfg, mStore, mUDSClient, mockClient)

			if tc.setupMocks != nil {
				tc.setupMocks(mStore, mUDSClient)
			}

			err := engine.checkAndTriggerEvents(ctx)

			if tc.expectError {
				assert.Error(t, err)
				if tc.expectedErrorContains != "" {
					assert.Contains(t, err.Error(), tc.expectedErrorContains)
				}
			} else {
				assert.NoError(t, err)
			}

			if tc.verifyMocks != nil {
				tc.verifyMocks(t, mStore, mUDSClient)
			}
		})
	}
}

// TestHealthyTriggerWaitsForNodeReady verifies that a healthy trigger is skipped when the node is NotReady and
// is successfully sent once the node transitions to Ready in a subsequent poll cycle.
func TestHealthyTriggerWaitsForNodeReady(t *testing.T) {
	ctx := context.Background()
	cfg := newTestConfig()

	healthyEvent := model.MaintenanceEvent{
		EventID: "h-event-deferred", NodeName: "node-deferred", ResourceType: "test", ResourceID: "h-id", RecommendedAction: pb.RecommendedAction_NONE.String(),
	}

	mockClient := createMockClientWithNotReadyNode(healthyEvent.NodeName)

	mStore := new(MockDatastore)
	mUDSClient := new(MockUDSClient)

	mStore.On("FindEventsToTriggerQuarantine", ctx, mock.AnythingOfType("time.Duration")).Return([]model.MaintenanceEvent{}, nil).Twice()
	mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).Return([]model.MaintenanceEvent{healthyEvent}, nil).Once()
	mStore.On("FindEventsToTriggerHealthy", ctx, mock.AnythingOfType("time.Duration")).Return([]model.MaintenanceEvent{}, nil).Once()

	mUDSClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything, mock.Anything).Return(&emptypb.Empty{}, nil).Once()
	mStore.On("UpdateEventStatus", mock.AnythingOfType("*context.timerCtx"), healthyEvent.EventID, model.StatusHealthyTriggered).Return(nil).Once()

	engine := NewEngine(cfg, mStore, mUDSClient, mockClient)
	engine.monitorInterval = 3 * time.Second

	err := engine.checkAndTriggerEvents(ctx)
	assert.NoError(t, err)
	mUDSClient.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything, mock.Anything)
	mStore.AssertNotCalled(t, "UpdateEventStatus", mock.Anything, mock.Anything, mock.Anything)

	node, _ := mockClient.CoreV1().Nodes().Get(ctx, healthyEvent.NodeName, metav1.GetOptions{})
	for i, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			node.Status.Conditions[i].Status = corev1.ConditionTrue
		}
	}
	_, _ = mockClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

	time.Sleep(5 * time.Second)
	err = engine.checkAndTriggerEvents(ctx)
	assert.NoError(t, err)

	mUDSClient.AssertExpectations(t)
	mStore.AssertExpectations(t)
}
