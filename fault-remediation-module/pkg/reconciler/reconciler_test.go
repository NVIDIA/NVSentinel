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

package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/crstatus"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/statemanager"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"k8s.io/utils/ptr"
)

// MockK8sClient is a mock implementation of K8sClient interface
type MockK8sClient struct {
	createMaintenanceResourceFn func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string)
	runLogCollectorJobFn        func(ctx context.Context, nodeName string) error
	statusCheckerOverride       crstatus.CRStatusChecker       // Optional override for testing
	annotationManagerOverride   NodeAnnotationManagerInterface // Optional override for testing
}

func (m *MockK8sClient) CreateMaintenanceResource(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
	return m.createMaintenanceResourceFn(ctx, healthEventDoc)
}

func (m *MockK8sClient) RunLogCollectorJob(ctx context.Context, nodeName string) error {
	return m.runLogCollectorJobFn(ctx, nodeName)
}

func (m *MockK8sClient) GetAnnotationManager() NodeAnnotationManagerInterface {
	return m.annotationManagerOverride
}

func (m *MockK8sClient) GetStatusCheckerForAction(action platformconnector.RecommenedAction) (crstatus.CRStatusChecker, error) {
	// Use override if set
	if m.statusCheckerOverride != nil {
		return m.statusCheckerOverride, nil
	}
	// Return a default mock status checker
	return &MockCRStatusChecker{status: crstatus.CRStatusNotFound}, nil
}

// MockCollection is a mock implementation of mongo.Collection
type MockCollection struct {
	updateOneFn      func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	countDocumentsFn func(ctx context.Context, filter interface{}, opts ...*options.CountOptions) (int64, error)
	findFn           func(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error)
}

// MockCRStatusChecker is a mock implementation of CRStatusChecker
type MockCRStatusChecker struct {
	status crstatus.CRStatus
}

func (m *MockCRStatusChecker) GetCRStatus(ctx context.Context, crName string) (crstatus.CRStatus, error) {
	return m.status, nil
}

// MockNodeAnnotationManager is a mock implementation for testing
type MockNodeAnnotationManager struct {
	existingCR string
}

func (m *MockNodeAnnotationManager) GetRemediationState(ctx context.Context, nodeName string) (*RemediationStateAnnotation, error) {
	if m.existingCR == "" {
		return &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}, nil
	}

	return &RemediationStateAnnotation{
		EquivalenceGroups: map[string]EquivalenceGroupState{
			"restart": {
				MaintenanceCR: m.existingCR,
				CreatedAt:     time.Now(),
			},
		},
	}, nil
}

func (m *MockNodeAnnotationManager) UpdateRemediationState(ctx context.Context, nodeName string,
	group string, crName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) RemoveGroupFromState(ctx context.Context, nodeName string, group string) error {
	return nil
}

func (m *MockCollection) UpdateOne(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	return m.updateOneFn(ctx, filter, update, opts...)
}

func (m *MockCollection) CountDocuments(ctx context.Context, filter interface{}, opts ...*options.CountOptions) (int64, error) {
	if m.countDocumentsFn != nil {
		return m.countDocumentsFn(ctx, filter, opts...)
	}
	return 0, nil
}

func (m *MockCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	if m.findFn != nil {
		return m.findFn(ctx, filter, opts...)
	}

	return &mongo.Cursor{}, nil
}

func TestNewReconciler(t *testing.T) {
	tests := []struct {
		name             string
		nodeName         string
		crCreationResult bool
		dryRun           bool
	}{
		{
			name:             "Create reconciler with dry run enabled",
			nodeName:         "node1",
			crCreationResult: true,
			dryRun:           true,
		},
		{
			name:             "Create reconciler with dry run disabled",
			nodeName:         "node2",
			crCreationResult: false,
			dryRun:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ReconcilerConfig{
				MongoConfig: storewatcher.MongoDBConfig{
					URI:      "mongodb://localhost:27017",
					Database: "test",
				},
				RemediationClient: &MockK8sClient{
					createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
						assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
						return tt.crCreationResult, "test-cr-name"
					},
				},
			}

			r := NewReconciler(cfg, tt.dryRun)
			assert.NotNil(t, r)
			assert.Equal(t, tt.dryRun, r.DryRun)
		})
	}
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction platformconnector.RecommenedAction
		shouldSucceed     bool
	}{
		{
			name:              "Successful RESTART_VM action",
			nodeName:          "node1",
			recommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			shouldSucceed:     true,
		},
		{
			name:              "Failed RESTART_VM action",
			nodeName:          "node2",
			recommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			shouldSucceed:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
					assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
					assert.Equal(t, tt.recommendedAction, healthEventDoc.HealthEventWithStatus.HealthEvent.RecommendedAction)
					return tt.shouldSucceed, "test-cr-name"
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient: k8sClient,
			}

			r := NewReconciler(cfg, false)
			healthEventDoc := &HealthEventDoc{
				ID: primitive.NewObjectID(),
				HealthEventWithStatus: storeconnector.HealthEventWithStatus{
					HealthEvent: &platformconnector.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}
			result, _ := r.Config.RemediationClient.CreateMaintenanceResource(ctx, healthEventDoc)
			assert.Equal(t, tt.shouldSucceed, result)
		})
	}
}

func TestPerformRemediationWithUnsupportedAction(t *testing.T) {
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			t.Errorf("CreateMaintenanceResource should not be called on an unsupported action")
			return false, ""
		},
	}
	count := 0
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.RemediationFailedLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}
	cfg := ReconcilerConfig{
		RemediationClient: k8sClient,
		StateManager:      stateManager,
		UpdateMaxRetries:  2,
		UpdateRetryDelay:  1 * time.Microsecond,
	}
	healthEvent := HealthEventDoc{
		HealthEventWithStatus: storeconnector.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platformconnector.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: platformconnector.RecommenedAction_UNKNOWN,
			},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewReconciler(cfg, false)

	// shouldSkipEvent should return true for UNKNOWN action
	assert.True(t, r.shouldSkipEvent(t.Context(), healthEvent.HealthEventWithStatus))
}

func TestPerformRemediationWithSuccess(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr-success"
		},
	}
	count := 0
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.RemediatingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.RemediationSucceededLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}
	cfg := ReconcilerConfig{
		RemediationClient: k8sClient,
		StateManager:      stateManager,
		UpdateMaxRetries:  2,
		UpdateRetryDelay:  1 * time.Microsecond,
	}
	healthEvent := HealthEventDoc{
		HealthEventWithStatus: storeconnector.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platformconnector.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewReconciler(cfg, false)
	success, crName := r.performRemediation(ctx, &healthEvent)
	assert.True(t, success)
	assert.Equal(t, "test-cr-success", crName)
}

func TestPerformRemediationWithFailure(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return false, ""
		},
	}
	count := 0
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.RemediatingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.RemediationFailedLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}
	cfg := ReconcilerConfig{
		RemediationClient: k8sClient,
		StateManager:      stateManager,
		UpdateMaxRetries:  2,
		UpdateRetryDelay:  1 * time.Microsecond,
	}
	healthEvent := HealthEventDoc{
		HealthEventWithStatus: storeconnector.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platformconnector.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewReconciler(cfg, false)
	success, crName := r.performRemediation(ctx, &healthEvent)
	assert.False(t, success)
	assert.Empty(t, crName)
}

func TestPerformRemediationWithUpdateNodeStateLabelFailures(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr-label-error"
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			// Simulate error but allow the function to continue
			return true, fmt.Errorf("got an error calling UpdateNVSentinelStateNodeLabel")
		},
	}
	cfg := ReconcilerConfig{
		RemediationClient: k8sClient,
		StateManager:      stateManager,
		UpdateMaxRetries:  2,
		UpdateRetryDelay:  1 * time.Microsecond,
	}
	healthEvent := HealthEventDoc{
		HealthEventWithStatus: storeconnector.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platformconnector.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewReconciler(cfg, false)
	// Even with label update errors, remediation should still succeed
	success, crName := r.performRemediation(ctx, &healthEvent)
	assert.True(t, success)
	assert.Equal(t, "test-cr-label-error", crName)
}

func TestShouldSkipEvent(t *testing.T) {
	mockK8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr"
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	r := NewReconciler(ReconcilerConfig{RemediationClient: mockK8sClient, StateManager: stateManager}, false)

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction platformconnector.RecommenedAction
		shouldSkip        bool
		description       string
	}{
		{
			name:              "Skip NONE action",
			nodeName:          "test-node-1",
			recommendedAction: platformconnector.RecommenedAction_NONE,
			shouldSkip:        true,
			description:       "NONE actions should be skipped",
		},
		{
			name:              "Process RESTART_VM action",
			nodeName:          "test-node-2",
			recommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			shouldSkip:        false,
			description:       "RESTART_VM actions should not be skipped",
		},
		{
			name:              "Skip CONTACT_SUPPORT action",
			nodeName:          "test-node-3",
			recommendedAction: platformconnector.RecommenedAction_CONTACT_SUPPORT,
			shouldSkip:        true,
			description:       "Unsupported CONTACT_SUPPORT action should be skipped",
		},
		{
			name:              "Skip unknown action",
			nodeName:          "test-node-4",
			recommendedAction: platformconnector.RecommenedAction(999),
			shouldSkip:        true,
			description:       "Unknown actions should be skipped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthEvent := &platformconnector.HealthEvent{
				NodeName:          tt.nodeName,
				RecommendedAction: tt.recommendedAction,
			}
			healthEventWithStatus := storeconnector.HealthEventWithStatus{
				HealthEvent: healthEvent,
			}

			result := r.shouldSkipEvent(t.Context(), healthEventWithStatus)
			assert.Equal(t, tt.shouldSkip, result, tt.description)
		})
	}
}

func TestRunLogCollectorOnNoneActionWhenEnabled(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr-name"
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			called = true
			assert.Equal(t, "test-node-none", nodeName)
			return nil
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient:  k8sClient,
		EnableLogCollector: true,
		StateManager:       stateManager,
	}
	r := NewReconciler(cfg, false)

	he := &platformconnector.HealthEvent{NodeName: "test-node-none", RecommendedAction: platformconnector.RecommenedAction_NONE}
	event := storeconnector.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector run before skipping
	if event.HealthEvent.RecommendedAction == platformconnector.RecommenedAction_NONE && r.Config.EnableLogCollector {
		_ = r.Config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
	}
	assert.True(t, r.shouldSkipEvent(t.Context(), event))
	assert.True(t, called, "log collector job should be invoked when enabled for NONE action")
}

func TestRunLogCollectorJobErrorScenarios(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		nodeName       string
		jobResult      bool
		expectedResult bool
		description    string
	}{
		{
			name:           "Log collector job succeeds",
			nodeName:       "test-node-success",
			jobResult:      true,
			expectedResult: true,
			description:    "Happy path - job completes successfully",
		},
		{
			name:           "Log collector job fails",
			nodeName:       "test-node-fail",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job fails to complete",
		},
		{
			name:           "Log collector job with api error",
			nodeName:       "test-node-api-error",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - kubernetes API error during job creation",
		},
		{
			name:           "Log collector job with creation error",
			nodeName:       "test-node-create-error",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job creation fails",
		},
		{
			name:           "Log collector job timeout",
			nodeName:       "test-node-timeout",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job times out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
					return true, "test-cr-name"
				},
				runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
					assert.Equal(t, tt.nodeName, nodeName)
					if tt.jobResult {
						return nil
					}
					return fmt.Errorf("job failed")
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient:  k8sClient,
				EnableLogCollector: true,
			}
			r := NewReconciler(cfg, false)

			result := r.Config.RemediationClient.RunLogCollectorJob(ctx, tt.nodeName)
			if tt.expectedResult {
				assert.NoError(t, result, tt.description)
			} else {
				assert.Error(t, result, tt.description)
			}
		})
	}
}

func TestRunLogCollectorJobDryRunMode(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr-name"
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			called = true
			// In dry run mode, this should return nil without actually creating the job
			return nil
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient:  k8sClient,
		EnableLogCollector: true,
	}
	r := NewReconciler(cfg, true) // Enable dry run

	result := r.Config.RemediationClient.RunLogCollectorJob(ctx, "test-node-dry-run")
	assert.NoError(t, result, "Dry run should return no error")
	assert.True(t, called, "Function should be called even in dry run mode")
}

func TestLogCollectorDisabled(t *testing.T) {
	ctx := context.Background()

	logCollectorCalled := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
			return true, "test-cr-name"
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			logCollectorCalled = true
			return nil
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient:  k8sClient,
		EnableLogCollector: false, // Disabled
		StateManager:       stateManager,
	}
	r := NewReconciler(cfg, false)

	he := &platformconnector.HealthEvent{NodeName: "test-node-disabled", RecommendedAction: platformconnector.RecommenedAction_NONE}
	event := storeconnector.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector should NOT run when disabled
	if event.HealthEvent.RecommendedAction == platformconnector.RecommenedAction_NONE && r.Config.EnableLogCollector {
		_ = r.Config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
	}
	assert.True(t, r.shouldSkipEvent(t.Context(), event))
	assert.False(t, logCollectorCalled, "log collector job should NOT be invoked when disabled")
}

func TestUpdateNodeRemediatedStatus(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		event          bson.M
		nodeRemediated bool
		mockError      error
		expectError    bool
	}{
		{
			name: "Successful update",
			event: bson.M{
				"fullDocument": bson.M{
					"_id": "test-id-1",
				},
			},
			nodeRemediated: true,
			mockError:      nil,
			expectError:    false,
		},
		{
			name: "Failed update",
			event: bson.M{
				"fullDocument": bson.M{
					"_id": "test-id-2",
				},
			},
			nodeRemediated: false,
			mockError:      assert.AnError,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockColl := &MockCollection{
				updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
					filterDoc := filter.(bson.M)
					updateDoc := update.(bson.M)

					assert.Equal(t, tt.event["fullDocument"].(bson.M)["_id"], filterDoc["_id"])
					assert.Equal(t, tt.nodeRemediated, updateDoc["$set"].(bson.M)["healtheventstatus.faultremediated"])

					if tt.mockError != nil {
						return nil, tt.mockError
					}
					return &mongo.UpdateResult{ModifiedCount: 1}, nil
				},
			}

			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
					return true, "test-cr"
				},
			}

			r := NewReconciler(ReconcilerConfig{
				RemediationClient: mockK8sClient,
				UpdateMaxRetries:  1,
				UpdateRetryDelay:  0,
			}, false)
			err := r.updateNodeRemediatedStatus(ctx, mockColl, tt.event, tt.nodeRemediated)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCRBasedDeduplication(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		existingCR    string
		crStatus      crstatus.CRStatus
		currentAction platformconnector.RecommenedAction
		expectedSkip  bool
		description   string
	}{
		{
			name:          "NoCR_AllowRemediation",
			existingCR:    "",
			crStatus:      crstatus.CRStatusNotFound,
			currentAction: platformconnector.RecommenedAction_RESTART_BM,
			expectedSkip:  false,
			description:   "Should allow remediation when no CR exists",
		},
		{
			name:          "CRSucceeded_SkipRemediation",
			existingCR:    "maintenance-node-123",
			crStatus:      crstatus.CRStatusSucceeded,
			currentAction: platformconnector.RecommenedAction_RESTART_BM,
			expectedSkip:  true,
			description:   "Should skip remediation when CR succeeded",
		},
		{
			name:          "CRInProgress_SkipRemediation",
			existingCR:    "maintenance-node-456",
			crStatus:      crstatus.CRStatusInProgress,
			currentAction: platformconnector.RecommenedAction_RESTART_BM,
			expectedSkip:  true,
			description:   "Should skip remediation when CR is in progress",
		},
		{
			name:          "CRFailed_AllowRemediation",
			existingCR:    "maintenance-node-789",
			crStatus:      crstatus.CRStatusFailed,
			currentAction: platformconnector.RecommenedAction_RESTART_BM,
			expectedSkip:  false,
			description:   "Should allow remediation when CR failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock status checker
			mockStatusChecker := &MockCRStatusChecker{
				status: tt.crStatus,
			}

			// Create mock annotation manager
			mockAnnotationManager := &MockNodeAnnotationManager{
				existingCR: tt.existingCR,
			}

			// Create mock K8s client with overrides
			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
					return true, "test-cr"
				},
				statusCheckerOverride:     mockStatusChecker,
				annotationManagerOverride: mockAnnotationManager,
			}

			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewReconciler(cfg, false)

			healthEvent := &platformconnector.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: tt.currentAction,
			}

			shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent)
			assert.NoError(t, err, tt.description)
			assert.Equal(t, !tt.expectedSkip, shouldCreateCR, tt.description)
			if tt.existingCR != "" && (tt.crStatus == crstatus.CRStatusSucceeded || tt.crStatus == crstatus.CRStatusInProgress) {
				assert.Equal(t, tt.existingCR, existingCR, "Should return existing CR name")
			}
		})
	}
}

func TestCrossActionRemediationWithEquivalenceGroups(t *testing.T) {
	ctx := context.Background()

	// Test that different actions in the same equivalence group are handled correctly
	tests := []struct {
		name           string
		existingAction platformconnector.RecommenedAction
		newEventAction platformconnector.RecommenedAction
		crStatus       crstatus.CRStatus
		shouldCreateCR bool
		description    string
	}{
		{
			name:           "ComponentReset_vs_NodeReboot_SameGroup_InProgress",
			existingAction: platformconnector.RecommenedAction_COMPONENT_RESET,
			newEventAction: platformconnector.RecommenedAction_RESTART_BM,
			crStatus:       crstatus.CRStatusInProgress,
			shouldCreateCR: false,
			description:    "Should skip RESTART_VM when COMPONENT_RESET CR is in progress (same group)",
		},
		{
			name:           "NodeReboot_vs_RestartVM_SameGroup_Succeeded",
			existingAction: platformconnector.RecommenedAction_RESTART_BM,
			newEventAction: platformconnector.RecommenedAction_RESTART_VM,
			crStatus:       crstatus.CRStatusSucceeded,
			shouldCreateCR: false,
			description:    "Should skip RESTART_VM when RESTART_VM CR succeeded (same group)",
		},
		{
			name:           "ResetGPU_vs_RestartBM_SameGroup_Failed",
			existingAction: platformconnector.RecommenedAction_COMPONENT_RESET,
			newEventAction: platformconnector.RecommenedAction_RESTART_BM,
			crStatus:       crstatus.CRStatusFailed,
			shouldCreateCR: true,
			description:    "Should allow RESTART_BM when COMPONENT_RESET CR failed (same group)",
		},
		{
			name:           "ComponentReset_vs_NONE_NotInGroup",
			existingAction: platformconnector.RecommenedAction_COMPONENT_RESET,
			newEventAction: platformconnector.RecommenedAction_NONE,
			crStatus:       crstatus.CRStatusSucceeded,
			shouldCreateCR: false, // NONE actions are skipped in shouldSkipEvent
			description:    "NONE action handling is independent of CR status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock status checker
			mockStatusChecker := &MockCRStatusChecker{
				status: tt.crStatus,
			}

			// Create mock annotation manager with existing CR
			mockAnnotationManager := &MockNodeAnnotationManager{
				existingCR: "maintenance-node-existing",
			}

			// Create mock K8s client with overrides
			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) (bool, string) {
					return true, "test-cr"
				},
				statusCheckerOverride:     mockStatusChecker,
				annotationManagerOverride: mockAnnotationManager,
			}

			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewReconciler(cfg, false)

			healthEvent := &platformconnector.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: tt.newEventAction,
			}

			shouldCreateCR, _, err := r.checkExistingCRStatus(ctx, healthEvent)
			assert.NoError(t, err, tt.description)

			// For actions in the same group
			if common.GetRemediationGroupForAction(tt.newEventAction) != "" &&
				common.GetRemediationGroupForAction(tt.existingAction) == common.GetRemediationGroupForAction(tt.newEventAction) {
				assert.Equal(t, tt.shouldCreateCR, shouldCreateCR, tt.description)
			}
		})
	}
}
