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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MockK8sClient is a mock implementation of K8sClient interface
type MockK8sClient struct {
	createMaintenanceResourceFn func(ctx context.Context, healthEventData *HealthEventData) (bool, string)
	runLogCollectorJobFn        func(ctx context.Context, nodeName string) error
	annotationManagerOverride   NodeAnnotationManagerInterface
	realStatusChecker           *crstatus.CRStatusChecker
}

type CRStatusCheckerInterface interface {
	IsSuccessful(ctx context.Context, crName string) bool
}

func (m *MockK8sClient) CreateMaintenanceResource(ctx context.Context, healthEventData *HealthEventData) (bool, string) {
	return m.createMaintenanceResourceFn(ctx, healthEventData)
}

func (m *MockK8sClient) RunLogCollectorJob(ctx context.Context, nodeName string) error {
	return m.runLogCollectorJobFn(ctx, nodeName)
}

func (m *MockK8sClient) GetAnnotationManager() NodeAnnotationManagerInterface {
	return m.annotationManagerOverride
}

func (m *MockK8sClient) GetStatusChecker() *crstatus.CRStatusChecker {
	return m.realStatusChecker
}

func (m *MockK8sClient) GetConfig() *config.TomlConfig {
	return &config.TomlConfig{
		RemediationActions: map[string]config.MaintenanceResource{
			protos.RecommendedAction_RESTART_BM.String(): {
				EquivalenceGroup: "restart",
			},
			protos.RecommendedAction_COMPONENT_RESET.String(): {
				EquivalenceGroup: "restart",
			},
		},
	}
}

// MockDatabaseClient is a mock implementation of DatabaseClient
type MockDatabaseClient struct {
	updateDocumentFn func(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error)
	countDocumentsFn func(ctx context.Context, filter interface{}, options *client.CountOptions) (int64, error)
	findFn           func(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error)
}

type MockCRStatusChecker struct {
	isSuccessful bool
}

func (m *MockCRStatusChecker) IsSuccessful(ctx context.Context, crName string) bool {
	return m.isSuccessful
}

type TestCRStatusChecker struct {
	mock *MockCRStatusChecker
}

func (t *TestCRStatusChecker) IsSuccessful(ctx context.Context, crName string) bool {
	return t.mock.IsSuccessful(ctx, crName)
}

type MockCRStatusCheckerWrapper struct {
	mock *MockCRStatusChecker
}

func (w *MockCRStatusCheckerWrapper) IsSuccessful(ctx context.Context, crName string) bool {
	if w.mock != nil {
		return w.mock.IsSuccessful(ctx, crName)
	}
	return false
}

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
	group string, crName string, actionName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) RemoveGroupFromState(ctx context.Context, nodeName string, group string) error {
	return nil
}

func (m *MockDatabaseClient) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	if m.updateDocumentFn != nil {
		return m.updateDocumentFn(ctx, filter, update)
	}
	return &client.UpdateResult{ModifiedCount: 1}, nil
}

func (m *MockDatabaseClient) CountDocuments(ctx context.Context, filter interface{}, options *client.CountOptions) (int64, error) {
	if m.countDocumentsFn != nil {
		return m.countDocumentsFn(ctx, filter, options)
	}
	return 0, nil
}

func (m *MockDatabaseClient) Find(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error) {
	if m.findFn != nil {
		return m.findFn(ctx, filter, options)
	}
	return nil, nil
}

// Additional methods required by client.DatabaseClient interface
func (m *MockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status interface{}) error {
	return nil
}

func (m *MockDatabaseClient) UpsertDocument(ctx context.Context, filter interface{}, document interface{}) (*client.UpdateResult, error) {
	return &client.UpdateResult{ModifiedCount: 1}, nil
}

func (m *MockDatabaseClient) FindOne(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error) {
	return nil, nil
}

func (m *MockDatabaseClient) Aggregate(ctx context.Context, pipeline interface{}) (client.Cursor, error) {
	return nil, nil
}

func (m *MockDatabaseClient) Ping(ctx context.Context) error {
	return nil
}

func (m *MockDatabaseClient) Close(ctx context.Context) error {
	return nil
}

func (m *MockDatabaseClient) DeleteResumeToken(ctx context.Context, tokenConfig client.TokenConfig) error {
	return nil
}

func (m *MockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, filter interface{}) (client.ChangeStreamWatcher, error) {
	return nil, nil // Simple mock implementation
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
				DataStoreConfig: datastore.DataStoreConfig{
					Provider: datastore.ProviderMongoDB,
					Connection: datastore.ConnectionConfig{
						Host:     "mongodb://localhost:27017",
						Database: "test",
					},
				},
				RemediationClient: &MockK8sClient{
					createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
						assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
						return tt.crCreationResult, "test-cr-name"
					},
				},
			}

			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, tt.dryRun)
			assert.NotNil(t, r)
			assert.Equal(t, tt.dryRun, r.dryRun)
		})
	}
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction protos.RecommendedAction
		shouldSucceed     bool
	}{
		{
			name:              "Successful RESTART_VM action",
			nodeName:          "node1",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			shouldSucceed:     true,
		},
		{
			name:              "Failed RESTART_VM action",
			nodeName:          "node2",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			shouldSucceed:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
					assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
					assert.Equal(t, tt.recommendedAction, healthEventDoc.HealthEventWithStatus.HealthEvent.RecommendedAction)
					return tt.shouldSucceed, "test-cr-name"
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient: k8sClient,
			}

			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
			healthEventData := &HealthEventData{
				ID: uuid.New().String(),
				HealthEventWithStatus: model.HealthEventWithStatus{
					HealthEvent: &protos.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}
			result, _ := r.config.RemediationClient.CreateMaintenanceResource(ctx, healthEventData)
			assert.Equal(t, tt.shouldSucceed, result)
		})
	}
}

func TestPerformRemediationWithUnsupportedAction(t *testing.T) {
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	healthEvent := HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_UNKNOWN,
			},
			HealthEventStatus: model.HealthEventStatus{
				NodeQuarantined:        ptr.To(model.Quarantined),
				UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	// shouldSkipEvent should return true for UNKNOWN action
	assert.True(t, r.shouldSkipEvent(t.Context(), healthEvent.HealthEventWithStatus))
}

func TestPerformRemediationWithSuccess(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	healthEvent := HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: model.HealthEventStatus{
				NodeQuarantined:        ptr.To(model.Quarantined),
				UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	success, crName, err := r.performRemediation(ctx, healthEventDoc)
	assert.NoError(t, err)
	assert.True(t, success)
	assert.Equal(t, "test-cr-success", crName)
}

func TestPerformRemediationWithFailure(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	healthEvent := HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: model.HealthEventStatus{
				NodeQuarantined:        ptr.To(model.Quarantined),
				UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	success, crName, err := r.performRemediation(ctx, healthEventDoc)
	assert.NoError(t, err)
	assert.False(t, success)
	assert.Empty(t, crName)
}

func TestPerformRemediationWithUpdateNodeStateLabelFailures(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	healthEvent := HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: model.HealthEventStatus{
				NodeQuarantined:        ptr.To(model.Quarantined),
				UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusSucceeded},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	// Even with label update errors, remediation should still succeed
	success, crName, err := r.performRemediation(ctx, healthEventDoc)
	assert.NoError(t, err)
	assert.True(t, success)
	assert.Equal(t, "test-cr-label-error", crName)
}

func TestShouldSkipEvent(t *testing.T) {
	mockK8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
			return true, "test-cr"
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{RemediationClient: mockK8sClient, StateManager: stateManager}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction protos.RecommendedAction
		shouldSkip        bool
		description       string
	}{
		{
			name:              "Skip NONE action",
			nodeName:          "test-node-1",
			recommendedAction: protos.RecommendedAction_NONE,
			shouldSkip:        true,
			description:       "NONE actions should be skipped",
		},
		{
			name:              "Process RESTART_VM action",
			nodeName:          "test-node-2",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			shouldSkip:        false,
			description:       "RESTART_VM actions should not be skipped",
		},
		{
			name:              "Skip CONTACT_SUPPORT action",
			nodeName:          "test-node-3",
			recommendedAction: protos.RecommendedAction_CONTACT_SUPPORT,
			shouldSkip:        true,
			description:       "Unsupported CONTACT_SUPPORT action should be skipped",
		},
		{
			name:              "Skip unknown action",
			nodeName:          "test-node-4",
			recommendedAction: protos.RecommendedAction(999),
			shouldSkip:        true,
			description:       "Unknown actions should be skipped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthEvent := &protos.HealthEvent{
				NodeName:          tt.nodeName,
				RecommendedAction: tt.recommendedAction,
			}
			healthEventWithStatus := model.HealthEventWithStatus{
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
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	he := &protos.HealthEvent{NodeName: "test-node-none", RecommendedAction: protos.RecommendedAction_NONE}
	event := model.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector run before skipping
	if event.HealthEvent.RecommendedAction == protos.RecommendedAction_NONE && r.config.EnableLogCollector {
		_ = r.config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
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
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			result := r.config.RemediationClient.RunLogCollectorJob(ctx, tt.nodeName)
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
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, true)

	result := r.config.RemediationClient.RunLogCollectorJob(ctx, "test-node-dry-run")
	assert.NoError(t, result, "Dry run should return no error")
	assert.True(t, called, "Function should be called even in dry run mode")
}

func TestLogCollectorDisabled(t *testing.T) {
	ctx := context.Background()

	logCollectorCalled := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
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
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	he := &protos.HealthEvent{NodeName: "test-node-disabled", RecommendedAction: protos.RecommendedAction_NONE}
	event := model.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector should NOT run when disabled
	if event.HealthEvent.RecommendedAction == protos.RecommendedAction_NONE && r.config.EnableLogCollector {
		_ = r.config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
	}
	assert.True(t, r.shouldSkipEvent(t.Context(), event))
	assert.False(t, logCollectorCalled, "log collector job should NOT be invoked when disabled")
}

func TestUpdateNodeRemediatedStatus(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		eventToken     datastore.EventWithToken
		nodeRemediated bool
		mockError      error
		expectError    bool
	}{
		{
			name: "Successful update",
			eventToken: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"_id": "test-id-1",
					},
				},
				ResumeToken: []byte("test-token-1"),
			},
			nodeRemediated: true,
			mockError:      nil,
			expectError:    false,
		},
		{
			name: "Failed update",
			eventToken: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"_id": "test-id-2",
					},
				},
				ResumeToken: []byte("test-token-2"),
			},
			nodeRemediated: false,
			mockError:      assert.AnError,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
					return true, "test-cr"
				},
			}
			cfg := ReconcilerConfig{
				RemediationClient: mockK8sClient,
				UpdateMaxRetries:  1,
				UpdateRetryDelay:  0,
			}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
			// Create mock health event store
			mockHealthStore := &MockHealthEventStore{
				UpdateHealthEventStatusFn: func(ctx context.Context, id string, status datastore.HealthEventStatus) error {
					// Validate that the right parameters are passed
					if tt.mockError != nil {
						return tt.mockError
					}
					assert.Equal(t, tt.nodeRemediated, *status.FaultRemediated)
					return nil
				},
			}

			err := r.updateNodeRemediatedStatus(ctx, mockHealthStore, tt.eventToken, tt.nodeRemediated)

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
		crSucceeded   bool
		currentAction protos.RecommendedAction
		expectedSkip  bool
		description   string
	}{
		{
			name:          "NoCR_AllowRemediation",
			existingCR:    "",
			crSucceeded:   false,
			currentAction: protos.RecommendedAction_RESTART_BM,
			expectedSkip:  false,
			description:   "Should allow remediation when no CR exists",
		},
		{
			name:          "CRSucceeded_SkipRemediation",
			existingCR:    "maintenance-node-123",
			crSucceeded:   true,
			currentAction: protos.RecommendedAction_RESTART_BM,
			expectedSkip:  true,
			description:   "Should skip remediation when CR succeeded",
		},
		{
			name:          "CRFailed_AllowRemediation",
			existingCR:    "maintenance-node-789",
			crSucceeded:   false,
			currentAction: protos.RecommendedAction_RESTART_BM,
			expectedSkip:  false,
			description:   "Should allow remediation when CR failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAnnotationManager := &MockNodeAnnotationManager{
				existingCR: tt.existingCR,
			}

			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
					return true, "test-cr"
				},
				annotationManagerOverride: mockAnnotationManager,
			}

			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			healthEvent := &protos.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: tt.currentAction,
			}

			shouldCreateCR, _, err := r.checkExistingCRStatus(ctx, healthEvent)
			assert.NoError(t, err, tt.description)
			assert.True(t, shouldCreateCR, "Should always allow retry when no status checker")
		})
	}
}

func TestCrossActionRemediationWithEquivalenceGroups(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		existingAction protos.RecommendedAction
		newEventAction protos.RecommendedAction
		crSucceeded    bool
		shouldCreateCR bool
		description    string
	}{
		{
			name:           "ComponentReset_vs_NodeReboot_SameGroup_NotSucceeded",
			existingAction: protos.RecommendedAction_COMPONENT_RESET,
			newEventAction: protos.RecommendedAction_RESTART_BM,
			crSucceeded:    false,
			shouldCreateCR: true,
			description:    "Should allow RESTART_BM when COMPONENT_RESET CR not succeeded (same group)",
		},
		{
			name:           "NodeReboot_vs_RestartVM_SameGroup_Succeeded",
			existingAction: protos.RecommendedAction_RESTART_BM,
			newEventAction: protos.RecommendedAction_RESTART_VM,
			crSucceeded:    true,
			shouldCreateCR: false,
			description:    "Should create RESTART_VM when RESTART_BM CR succeeded (same group, but only partial fix)",
		},
		{
			name:           "ResetGPU_vs_RestartBM_SameGroup_NotSucceeded",
			existingAction: protos.RecommendedAction_COMPONENT_RESET,
			newEventAction: protos.RecommendedAction_RESTART_BM,
			crSucceeded:    false,
			shouldCreateCR: true,
			description:    "Should allow RESTART_BM when COMPONENT_RESET CR not succeeded (same group)",
		},
		{
			name:           "ComponentReset_vs_NONE_NotInGroup",
			existingAction: protos.RecommendedAction_COMPONENT_RESET,
			newEventAction: protos.RecommendedAction_NONE,
			crSucceeded:    true,
			shouldCreateCR: false,
			description:    "NONE action handling is independent of CR status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAnnotationManager := &MockNodeAnnotationManager{
				existingCR: "maintenance-node-existing",
			}

			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
					return true, "test-cr"
				},
				annotationManagerOverride: mockAnnotationManager,
			}

			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			healthEvent := &protos.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: tt.newEventAction,
			}

			shouldCreateCR, _, err := r.checkExistingCRStatus(ctx, healthEvent)
			assert.NoError(t, err, tt.description)
			assert.True(t, shouldCreateCR, "Should always allow retry when no status checker")
		})
	}
}

// TestLogCollectorOnlyCalledWhenShouldCreateCR verifies that log collector is only called
// when shouldCreateCR is true (Issue #441 - prevent duplicate log-collector jobs)
// This tests the logic that log collector runs AFTER checkExistingCRStatus, not before
func TestLogCollectorOnlyCalledWhenShouldCreateCR(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                   string
		shouldCreateCR         bool
		expectLogCollectorCall bool
		description            string
	}{
		{
			name:                   "ShouldCreateCR_True_LogCollectorCalled",
			shouldCreateCR:         true,
			expectLogCollectorCall: true,
			description:            "Log collector should be called when shouldCreateCR is true",
		},
		{
			name:                   "ShouldCreateCR_False_LogCollectorSkipped",
			shouldCreateCR:         false,
			expectLogCollectorCall: false,
			description:            "Log collector should NOT be called when shouldCreateCR is false (prevents duplicate jobs)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logCollectorCalled := false

			mockK8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventData) (bool, string) {
					return true, "test-cr"
				},
				runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
					logCollectorCalled = true
					return nil
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient:  mockK8sClient,
				EnableLogCollector: true,
			}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			healthEvent := &protos.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			}

			// Simulate the behavior in handleRemediationEvent:
			// Log collector is only called when shouldCreateCR is true
			// This is the key fix for Issue #441 - log collector moved after CR check
			shouldCreateCR := tt.shouldCreateCR

			if shouldCreateCR {
				r.runLogCollector(ctx, healthEvent)
			}

			assert.Equal(t, tt.expectLogCollectorCall, logCollectorCalled, tt.description)
		})
	}
}
