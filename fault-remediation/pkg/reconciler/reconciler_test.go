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
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	fqcommon "github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	fqannotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MockK8sClient is a mock implementation of K8sClient interface
type MockK8sClient struct {
	createMaintenanceResourceFn func(ctx context.Context, healthEventData *events.HealthEventData,
		groupConfig *common.EquivalenceGroupConfig) (string, error)
	runLogCollectorJobFn      func(ctx context.Context, nodeName string) (ctrl.Result, error)
	annotationManagerOverride annotation.NodeAnnotationManagerInterface
	mockStatusChecker         *mockStatusChecker
}

func (m *MockK8sClient) CreateMaintenanceResource(ctx context.Context, healthEventData *events.HealthEventData, groupConfig *common.EquivalenceGroupConfig) (string, error) {
	return m.createMaintenanceResourceFn(ctx, healthEventData, groupConfig)
}

func (m *MockK8sClient) RunLogCollectorJob(ctx context.Context, nodeName string, eventId string) (ctrl.Result, error) {
	return m.runLogCollectorJobFn(ctx, nodeName)
}

func (m *MockK8sClient) GetAnnotationManager() annotation.NodeAnnotationManagerInterface {
	return m.annotationManagerOverride
}

func (m *MockK8sClient) GetStatusChecker() crstatus.CRStatusCheckerInterface {
	return m.mockStatusChecker
}

type mockStatusChecker struct {
	shouldSkip []bool
	states     []crstatus.CRState
	stateByCR  map[string]crstatus.CRState
	callCount  int
}

func (statusChecker *mockStatusChecker) ShouldSkipCRCreation(context.Context, string, string) bool {
	return statusChecker.GetCRState(context.Background(), "", "") != crstatus.CRStateFailed
}

func (statusChecker *mockStatusChecker) GetCRState(_ context.Context, _ string, crName string) crstatus.CRState {
	if statusChecker.stateByCR != nil {
		return statusChecker.stateByCR[crName]
	}

	if statusChecker.states != nil {
		state := statusChecker.states[statusChecker.callCount]
		if statusChecker.callCount < len(statusChecker.states)-1 {
			statusChecker.callCount++
		}

		return state
	}

	shouldSkip := statusChecker.shouldSkip[statusChecker.callCount]
	if statusChecker.callCount < len(statusChecker.shouldSkip)-1 {
		statusChecker.callCount++
	}
	if shouldSkip {
		return crstatus.CRStateInProgress
	}

	return crstatus.CRStateFailed
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
	existingCRs            map[string]string
	existingCRCreated      time.Time
	createdByGroup         map[string]time.Time
	nodeAnnotations        map[string]string
	nodeLabels             map[string]string
	getRemediationStateErr error
}

func (m *MockNodeAnnotationManager) node() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: m.nodeAnnotations,
			Labels:      m.nodeLabels,
		},
	}
}

func (m *MockNodeAnnotationManager) GetRemediationState(ctx context.Context, nodeName string) (*annotation.RemediationStateAnnotation, *corev1.Node, error) {
	if m.getRemediationStateErr != nil {
		return nil, nil, m.getRemediationStateErr
	}

	if m.existingCRs == nil {
		return &annotation.RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]annotation.EquivalenceGroupState),
		}, m.node(), nil
	}

	annotationState := &annotation.RemediationStateAnnotation{
		EquivalenceGroups: make(map[string]annotation.EquivalenceGroupState),
	}
	createdAt := m.existingCRCreated
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	for groupName, crName := range m.existingCRs {
		groupCreatedAt := createdAt
		if createdAtForGroup, ok := m.createdByGroup[groupName]; ok {
			groupCreatedAt = createdAtForGroup
		}
		annotationState.EquivalenceGroups[groupName] = annotation.EquivalenceGroupState{
			MaintenanceCR: crName,
			CreatedAt:     groupCreatedAt,
		}
	}
	return annotationState, m.node(), nil
}

func (m *MockNodeAnnotationManager) UpdateRemediationState(ctx context.Context, nodeName string,
	group string, crName string, actionName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	return nil
}

func (m *MockNodeAnnotationManager) RemoveGroupsFromState(ctx context.Context, nodeName string, groups []string) error {
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

func (m *MockDatabaseClient) UpdateDocumentStatusFields(ctx context.Context, documentID string, fields map[string]interface{}) error {
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

func getGroupConfig(equivalenceGroup string, supersedingEquivalenceGroups []string) *common.EquivalenceGroupConfig {
	return &common.EquivalenceGroupConfig{
		EffectiveEquivalenceGroup:    equivalenceGroup,
		SupersedingEquivalenceGroups: supersedingEquivalenceGroups,
	}
}

func TestNewReconciler(t *testing.T) {
	tests := []struct {
		name             string
		nodeName         string
		crCreationResult error
		dryRun           bool
	}{
		{
			name:             "Create reconciler with dry run enabled",
			nodeName:         "node1",
			crCreationResult: nil,
			dryRun:           true,
		},
		{
			name:             "Create reconciler with dry run disabled",
			nodeName:         "node2",
			crCreationResult: fmt.Errorf("test error"),
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
					createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
						_ *common.EquivalenceGroupConfig) (string, error) {
						assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
						return "test-cr-name", tt.crCreationResult
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
		expectedError     error
	}{
		{
			name:              "Successful RESTART_VM action",
			nodeName:          "node1",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			expectedError:     nil,
		},
		{
			name:              "Failed RESTART_VM action",
			nodeName:          "node2",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			expectedError:     fmt.Errorf("test error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
					_ *common.EquivalenceGroupConfig) (string, error) {
					assert.Equal(t, tt.nodeName, healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
					assert.Equal(t, tt.recommendedAction, healthEventDoc.HealthEventWithStatus.HealthEvent.RecommendedAction)
					return "test-cr-name", tt.expectedError
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient: k8sClient,
			}

			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
			healthEventData := &events.HealthEventData{
				ID: uuid.New().String(),
				HealthEventWithStatus: model.HealthEventWithStatus{
					HealthEvent: &protos.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}
			groupConfig := getGroupConfig("restart", nil)

			_, err := r.Config.RemediationClient.CreateMaintenanceResource(ctx, healthEventData, groupConfig)
			assert.Equal(t, tt.expectedError, err)
		})
	}

	t.Run("Successful custom action", func(t *testing.T) {
		k8sClient := &MockK8sClient{
			createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
				_ *common.EquivalenceGroupConfig) (string, error) {
				assert.Equal(t, "node-custom", healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
				assert.Equal(t, protos.RecommendedAction_CUSTOM,
					healthEventDoc.HealthEventWithStatus.HealthEvent.RecommendedAction)
				assert.Equal(t, "REPLACE_DISK",
					healthEventDoc.HealthEventWithStatus.HealthEvent.CustomRecommendedAction)
				return "test-cr-custom", nil
			},
		}

		cfg := ReconcilerConfig{RemediationClient: k8sClient}
		r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
		healthEventData := &events.HealthEventData{
			ID: uuid.New().String(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:                "node-custom",
					RecommendedAction:       protos.RecommendedAction_CUSTOM,
					CustomRecommendedAction: "REPLACE_DISK",
				},
			},
		}
		groupConfig := getGroupConfig("disk-replace", nil)

		crName, err := r.Config.RemediationClient.CreateMaintenanceResource(ctx, healthEventData, groupConfig)
		assert.NoError(t, err)
		assert.Equal(t, "test-cr-custom", crName)
	})
}

func TestPerformRemediationWithUnsupportedAction(t *testing.T) {
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			groupConfig *common.EquivalenceGroupConfig) (string, error) {
			t.Errorf("CreateMaintenanceResource should not be called on an unsupported action")
			return "", fmt.Errorf("test error")
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
	healthEvent := events.HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_UNKNOWN,
			},
			HealthEventStatus: &protos.HealthEventStatus{
				NodeQuarantined:        string(model.Quarantined),
				UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusSucceeded)},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	// shouldSkipEvent should return true for UNKNOWN action
	assert.True(t, r.shouldSkipEvent(t.Context(), healthEvent.HealthEventWithStatus, nil))
}

func TestPerformRemediationWithSuccess(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr-success", nil
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
	healthEvent := events.HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: &protos.HealthEventStatus{
				NodeQuarantined:        string(model.Quarantined),
				UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusSucceeded)},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &events.HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	groupConfig := getGroupConfig("restart", nil)

	crName, err := r.performRemediation(ctx, healthEventDoc, groupConfig)
	assert.NoError(t, err)
	assert.Equal(t, "test-cr-success", crName)
}

func TestPerformRemediationWithFailure(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "", errors.New("test error")
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
	healthEvent := events.HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: &protos.HealthEventStatus{
				NodeQuarantined:        string(model.Quarantined),
				UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusSucceeded)},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &events.HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	groupConfig := getGroupConfig("restart", nil)
	crName, err := r.performRemediation(ctx, healthEventDoc, groupConfig)
	assert.Error(t, err)
	assert.Empty(t, crName)
}

func TestPerformRemediationWithUpdateNodeStateLabelFailures(t *testing.T) {
	ctx := context.Background()
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr-label-error", nil
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
	healthEvent := events.HealthEventData{
		HealthEventWithStatus: model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node1",
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: &protos.HealthEventStatus{
				NodeQuarantined:        string(model.Quarantined),
				UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusSucceeded)},
				FaultRemediated:        nil,
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	// Convert HealthEventData to HealthEventDoc
	healthEventDoc := &events.HealthEventDoc{
		ID:                    "test-id-123",
		HealthEventWithStatus: healthEvent.HealthEventWithStatus,
	}
	groupConfig := getGroupConfig("restart", nil)
	// Even with label update errors, remediation should still succeed
	_, err := r.performRemediation(ctx, healthEventDoc, groupConfig)
	assert.Error(t, err)
}

func TestShouldSkipEvent(t *testing.T) {
	mockK8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr", nil
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
		groupConfig       *common.EquivalenceGroupConfig
		shouldSkip        bool
		description       string
	}{
		{
			name:              "Skip NONE action",
			nodeName:          "test-node-1",
			recommendedAction: protos.RecommendedAction_NONE,
			groupConfig:       nil,
			shouldSkip:        true,
			description:       "NONE actions should be skipped",
		},
		{
			name:              "Process RESTART_VM action",
			nodeName:          "test-node-2",
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			groupConfig:       getGroupConfig("restart", nil),
			shouldSkip:        false,
			description:       "RESTART_VM actions should not be skipped",
		},
		{
			name:              "Skip CONTACT_SUPPORT action",
			nodeName:          "test-node-3",
			recommendedAction: protos.RecommendedAction_CONTACT_SUPPORT,
			groupConfig:       nil,
			shouldSkip:        true,
			description:       "Unsupported CONTACT_SUPPORT action should be skipped",
		},
		{
			name:              "Skip unknown action",
			nodeName:          "test-node-4",
			recommendedAction: protos.RecommendedAction(999),
			groupConfig:       nil,
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

			result := r.shouldSkipEvent(t.Context(), healthEventWithStatus, tt.groupConfig)
			assert.Equal(t, tt.shouldSkip, result, tt.description)
		})
	}

	t.Run("Process custom action with group config", func(t *testing.T) {
		healthEvent := &protos.HealthEvent{
			NodeName:                "test-node-custom",
			RecommendedAction:       protos.RecommendedAction_CUSTOM,
			CustomRecommendedAction: "REPLACE_DISK",
		}
		healthEventWithStatus := model.HealthEventWithStatus{
			HealthEvent: healthEvent,
		}
		groupConfig := getGroupConfig("disk-replace", nil)

		result := r.shouldSkipEvent(t.Context(), healthEventWithStatus, groupConfig)
		assert.False(t, result, "Custom actions with a matching group config should not be skipped")
	})

	t.Run("Skip custom action without group config", func(t *testing.T) {
		healthEvent := &protos.HealthEvent{
			NodeName:                "test-node-custom-unconfigured",
			RecommendedAction:       protos.RecommendedAction_CUSTOM,
			CustomRecommendedAction: "UNCONFIGURED_ACTION",
		}
		healthEventWithStatus := model.HealthEventWithStatus{
			HealthEvent: healthEvent,
		}

		result := r.shouldSkipEvent(t.Context(), healthEventWithStatus, nil)
		assert.True(t, result, "Custom actions without a matching group config should be skipped")
	})
}

func TestRunLogCollectorOnNoneActionWhenEnabled(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr-name", nil
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) (ctrl.Result, error) {
			called = true
			assert.Equal(t, "test-node-none", nodeName)
			return ctrl.Result{}, nil
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
	if event.HealthEvent.RecommendedAction == protos.RecommendedAction_NONE && r.Config.EnableLogCollector {
		_, _ = r.Config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName, "")
	}
	groupConfig := getGroupConfig("restart", nil)
	assert.True(t, r.shouldSkipEvent(t.Context(), event, groupConfig))
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
		returnedResult ctrl.Result
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
			name:           "Log collector job return requeue",
			nodeName:       "test-node-fail",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job fails to complete",
			returnedResult: ctrl.Result{RequeueAfter: 5 * time.Minute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
					_ *common.EquivalenceGroupConfig) (string, error) {
					return "test-cr-name", nil
				},
				runLogCollectorJobFn: func(ctx context.Context, nodeName string) (ctrl.Result, error) {
					assert.Equal(t, tt.nodeName, nodeName)
					if tt.jobResult {
						return tt.returnedResult, nil
					}
					return ctrl.Result{}, fmt.Errorf("job failed")
				},
			}

			cfg := ReconcilerConfig{
				RemediationClient:  k8sClient,
				EnableLogCollector: true,
			}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			result, err := r.Config.RemediationClient.RunLogCollectorJob(ctx, tt.nodeName, "")
			if tt.expectedResult {
				assert.NoError(t, err, tt.description)
				assert.Equal(t, tt.returnedResult, result)
			} else {
				assert.Error(t, err, tt.description)
			}
		})
	}
}

func TestRunLogCollectorJobDryRunMode(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr-name", nil
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) (ctrl.Result, error) {
			called = true
			// In dry run mode, this should return nil without actually creating the job
			return ctrl.Result{}, nil
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient:  k8sClient,
		EnableLogCollector: true,
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, true)

	_, err := r.Config.RemediationClient.RunLogCollectorJob(ctx, "test-node-dry-run", "")
	assert.NoError(t, err, "Dry run should return no error")
	assert.True(t, called, "Function should be called even in dry run mode")
}

func TestLogCollectorDisabled(t *testing.T) {
	ctx := context.Background()

	logCollectorCalled := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
			_ *common.EquivalenceGroupConfig) (string, error) {
			return "test-cr-name", nil
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) (ctrl.Result, error) {
			logCollectorCalled = true
			return ctrl.Result{}, nil
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
	if event.HealthEvent.RecommendedAction == protos.RecommendedAction_NONE && r.Config.EnableLogCollector {
		_, _ = r.Config.RemediationClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName, "")
	}
	groupConfig := getGroupConfig("restart", nil)
	assert.True(t, r.shouldSkipEvent(t.Context(), event, groupConfig))
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
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
					_ *common.EquivalenceGroupConfig) (string, error) {
					return "test-cr", nil
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
		name                      string
		existingCRs               map[string]string
		shouldSkipCRCreation      []bool // if nil we will not provide a StatusChecker instances
		crStates                  []crstatus.CRState
		crStateByName             map[string]crstatus.CRState
		groupConfig               *common.EquivalenceGroupConfig
		eventCreatedAt            time.Time
		remediationCreatedAt      time.Time
		remediationCreatedByGroup map[string]time.Time
		expectedShouldCreateCR    bool
		expectedRemediated        bool
	}{
		{
			name:                   "NoStatusChecker_AllowRemediation",
			existingCRs:            nil,
			shouldSkipCRCreation:   nil,
			groupConfig:            getGroupConfig("restart", nil),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "NoCR_AllowRemediation",
			existingCRs:            nil,
			shouldSkipCRCreation:   []bool{true},
			groupConfig:            getGroupConfig("restart", nil),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "CRFailed_AllowRemediation",
			existingCRs:            map[string]string{"restart": "maintenance-node-123"},
			shouldSkipCRCreation:   []bool{false},
			groupConfig:            getGroupConfig("restart", nil),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "CRSucceeded_SkipRemediation",
			existingCRs:            map[string]string{"restart": "maintenance-node-123"},
			shouldSkipCRCreation:   []bool{true},
			groupConfig:            getGroupConfig("restart", nil),
			expectedShouldCreateCR: false,
		},
		{
			name:                   "MultipleCRs_SkipRemediation_MultipleEquivalenceGroups_OneInProgress",
			existingCRs:            map[string]string{"restart": "maintenance-node-123", "reset-GPU-123": "maintenance-gpu-123"},
			shouldSkipCRCreation:   []bool{false, true},
			groupConfig:            getGroupConfig("restart", []string{"reset-GPU-123"}),
			expectedShouldCreateCR: false,
		},
		{
			name:                   "MultipleCRs_AllowRemediation_MultipleEquivalenceGroups_BothCompleted",
			existingCRs:            map[string]string{"restart": "maintenance-node-123", "reset-GPU-123": "maintenance-gpu-123"},
			shouldSkipCRCreation:   []bool{false, false},
			groupConfig:            getGroupConfig("restart", []string{"reset-GPU-123"}),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "MultipleCRs_AllowRemediation_MultipleEquivalenceGroups_OneInProgressNotMatching",
			existingCRs:            map[string]string{"restart": "maintenance-node-123", "reset-GPU-123": "maintenance-gpu-123"},
			shouldSkipCRCreation:   []bool{false, true},
			groupConfig:            getGroupConfig("reset", []string{"reset-GPU-123"}),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "MultipleCRs_AllowRemediation_MultipleEquivalenceGroups_OneInProgressNotMatchingForSuperseding",
			existingCRs:            map[string]string{"restart": "maintenance-node-123", "reset-GPU-456": "maintenance-gpu-456"},
			shouldSkipCRCreation:   []bool{false, true},
			groupConfig:            getGroupConfig("restart", []string{"reset-GPU-123"}),
			expectedShouldCreateCR: true,
		},
		{
			name:                   "CRSucceeded_SameSession_MarksRemediated",
			existingCRs:            map[string]string{"restart": "maintenance-node-123"},
			crStates:               []crstatus.CRState{crstatus.CRStateSucceeded},
			groupConfig:            getGroupConfig("restart", nil),
			eventCreatedAt:         time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
			remediationCreatedAt:   time.Date(2026, 5, 14, 10, 1, 0, 0, time.UTC),
			expectedShouldCreateCR: false,
			expectedRemediated:     true,
		},
		{
			name:                   "CRSucceeded_FutureSession_AllowsRemediation",
			existingCRs:            map[string]string{"restart": "maintenance-node-123"},
			crStates:               []crstatus.CRState{crstatus.CRStateSucceeded},
			groupConfig:            getGroupConfig("restart", nil),
			eventCreatedAt:         time.Date(2026, 5, 14, 10, 10, 1, 0, time.UTC),
			remediationCreatedAt:   time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
			expectedShouldCreateCR: true,
			expectedRemediated:     false,
		},
		{
			name:        "MultipleMatchingGroups_NewestStateEvaluatedFirst",
			existingCRs: map[string]string{"restart": "maintenance-restart", "reset-GPU-123": "maintenance-reset"},
			crStateByName: map[string]crstatus.CRState{
				"maintenance-restart": crstatus.CRStateFailed,
				"maintenance-reset":   crstatus.CRStateInProgress,
			},
			groupConfig:            getGroupConfig("reset-GPU-123", []string{"restart"}),
			eventCreatedAt:         time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
			expectedShouldCreateCR: false,
			expectedRemediated:     false,
			remediationCreatedByGroup: map[string]time.Time{
				"restart":       time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
				"reset-GPU-123": time.Date(2026, 5, 14, 10, 1, 0, 0, time.UTC),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAnnotationManager := &MockNodeAnnotationManager{
				existingCRs:       tt.existingCRs,
				existingCRCreated: tt.remediationCreatedAt,
				createdByGroup:    tt.remediationCreatedByGroup,
			}

			mockK8sClient := &MockK8sClient{
				annotationManagerOverride: mockAnnotationManager,
			}

			if tt.shouldSkipCRCreation != nil {
				mockK8sClient.mockStatusChecker = &mockStatusChecker{
					shouldSkip: tt.shouldSkipCRCreation,
				}
			}
			if tt.crStates != nil {
				mockK8sClient.mockStatusChecker = &mockStatusChecker{
					states: tt.crStates,
				}
			}
			if tt.crStateByName != nil {
				mockK8sClient.mockStatusChecker = &mockStatusChecker{
					stateByCR: tt.crStateByName,
				}
			}

			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

			healthEvent := &protos.HealthEvent{
				NodeName: "test-node",
			}
			shouldCreateCR, _, remediated, err := r.checkExistingCRStatus(ctx, healthEvent, tt.eventCreatedAt, tt.groupConfig)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedShouldCreateCR, shouldCreateCR)
			assert.Equal(t, tt.expectedRemediated, remediated)
		})
	}
}

func TestCloseStaleEquivalentEvents(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"

	tests := []struct {
		name              string
		status            model.Status
		state             *annotation.RemediationStateAnnotation
		rawEvents         []datastore.Event
		expectedUpdates   map[string]bool
		expectTimestamp   bool
		unexpectedUpdates []string
	}{
		{
			name:   "UnQuarantined closes only equivalent restart events as remediated",
			status: model.UnQuarantined,
			// This annotation is what fault-remediation writes when it creates a maintenance CR.
			// In production this means "there is/was a restart remediation covering this node".
			// When fault-quarantine later unquarantines the node, skipped stale events in the
			// same equivalence group should be closed so cold start cannot replay them.
			state: &annotation.RemediationStateAnnotation{
				EquivalenceGroups: map[string]annotation.EquivalenceGroupState{
					"restart": {MaintenanceCR: "maintenance-test-node-event-a", ActionName: "RESTART_BM"},
				},
			},
			// event-a is the stale event: it needs RESTART_BM, which maps to the restart group,
			// so the prior restart CR covers it. event-contact-support is intentionally unrelated:
			// it has no configured remediation group and must not be swept up by node-level cleanup.
			rawEvents: []datastore.Event{
				testRawHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM),
				testRawHealthEvent("event-contact-support", nodeName, protos.RecommendedAction_CONTACT_SUPPORT),
			},
			expectedUpdates: map[string]bool{
				"event-a": true,
			},
			expectTimestamp:   true,
			unexpectedUpdates: []string{"event-contact-support"},
		},
		{
			name:   "Cancelled closes equivalent restart events as not remediated",
			status: model.Cancelled,
			// Cancelled is the external/manual cancellation path, for example someone
			// manually uncordoned the node. Unlike UnQuarantined, it does not prove the
			// fault was remediated. FR still needs a durable terminal value so cold start
			// does not replay the stale event, but that value must be faultremediated=false.
			state: &annotation.RemediationStateAnnotation{
				EquivalenceGroups: map[string]annotation.EquivalenceGroupState{
					"restart": {MaintenanceCR: "maintenance-test-node-event-a", ActionName: "RESTART_BM"},
				},
			},
			rawEvents: []datastore.Event{
				testRawHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM),
			},
			// expectedUpdates maps event ID -> expected healtheventstatus.faultremediated value.
			// false here means "terminal, but not successfully remediated".
			expectedUpdates: map[string]bool{
				"event-a": false,
			},
		},
		{
			name:   "No remediation state does not close events",
			status: model.UnQuarantined,
			// Without a remediation annotation, FR has no equivalence-group evidence that an
			// earlier maintenance CR covered these events. The cleanup must therefore do nothing.
			state: &annotation.RemediationStateAnnotation{
				EquivalenceGroups: map[string]annotation.EquivalenceGroupState{},
			},
			rawEvents: []datastore.Event{
				testRawHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM),
			},
			expectedUpdates: map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updates := make(map[string]datastore.HealthEventStatus)
			mockStore := &MockHealthEventStore{
				FindHealthEventsByQueryBatchedFn: func(_ context.Context, _ datastore.QueryBuilder, _ int,
					fn func([]datastore.HealthEventWithStatus) error) error {
					batch := make([]datastore.HealthEventWithStatus, 0, len(tt.rawEvents))
					for _, rawEvent := range tt.rawEvents {
						batch = append(batch, datastore.HealthEventWithStatus{RawEvent: rawEvent})
					}

					return fn(batch)
				},
				UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
					updates[id] = status
					return nil
				},
			}

			mockK8sClient := &MockK8sClient{}
			cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
			r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)

			err := r.closeStaleEquivalentEvents(ctx, nodeName, tt.status, tt.state, mockStore)
			assert.NoError(t, err)

			assert.Len(t, updates, len(tt.expectedUpdates))
			for id, expectedRemediated := range tt.expectedUpdates {
				status, ok := updates[id]
				assert.True(t, ok, "expected update for %s", id)
				assert.NotNil(t, status.FaultRemediated)
				assert.Equal(t, expectedRemediated, *status.FaultRemediated)
				if tt.expectTimestamp {
					assert.NotNil(t, status.LastRemediationTimestamp)
				} else {
					assert.Nil(t, status.LastRemediationTimestamp)
				}
			}

			for _, id := range tt.unexpectedUpdates {
				_, ok := updates[id]
				assert.False(t, ok, "did not expect update for %s", id)
			}
		})
	}
}

func TestUnsupportedActionSkipMarksEventTerminal(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	eventID := "unsupported-event"
	updated := false

	// CONTACT_SUPPORT is not configured as a maintenance action for FR. That is an
	// intentional skip, but it still needs a durable terminal status; otherwise cold
	// start will keep replaying the same unsupported event because faultremediated is nil.
	mockK8sClient := &MockK8sClient{}
	cfg := ReconcilerConfig{
		RemediationClient: mockK8sClient,
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(context.Context, string,
				statemanager.NVSentinelStateLabelValue, bool) (bool, error) {
				return true, nil
			},
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	mockStore := &MockHealthEventStore{
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, eventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.False(t, *status.FaultRemediated)
			updated = true

			return nil
		},
	}
	healthEventDoc := &events.HealthEventDoc{
		ID: eventID,
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_CONTACT_SUPPORT,
			},
			HealthEventStatus: &protos.HealthEventStatus{},
		},
	}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(eventID, nodeName, protos.RecommendedAction_CONTACT_SUPPORT),
		ResumeToken: []byte("resume-token"),
	}

	_, err, done := r.trySkipEvent(ctx, healthEventDoc, nil, eventWithToken, mockWatcher, mockStore, nodeName)
	assert.True(t, done)
	assert.NoError(t, err)
	assert.True(t, updated)
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoveryRecomputesLabelToRemediationSucceeded(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	activeEventID := "event-a"
	recoveryEventID := "event-c"
	targetLabels := make([]statemanager.NVSentinelStateLabelValue, 0)
	completionUpdated := false

	activeEvent := testAnnotationHealthEvent(activeEventID, nodeName, protos.RecommendedAction_RESTART_BM, "GPU-a")
	mockAnnotationManager := &MockNodeAnnotationManager{
		existingCRs: map[string]string{
			"restart": "maintenance-test-node-event-a",
		},
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationFailedLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{annotationManagerOverride: mockAnnotationManager},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(_ context.Context, nodeName string,
				newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
				assert.Equal(t, "test-node", nodeName)
				assert.False(t, removeStateLabel)
				targetLabels = append(targetLabels, newStateLabelValue)

				// Moving away from remediation-failed is an intentional recompute and
				// may be reported as an unexpected transition after the label is updated.
				return true, fmt.Errorf("unexpected state transition")
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			remediated := true

			return []datastore.HealthEventWithStatus{
				{
					HealthEventStatus: datastore.HealthEventStatus{
						FaultRemediated: &remediated,
						UserPodsEvictionStatus: datastore.OperationStatus{
							Status: datastore.StatusSucceeded,
						},
					},
				},
			}, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)
			completionUpdated = true

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.Equal(t, []statemanager.NVSentinelStateLabelValue{
		statemanager.RemediationSucceededLabelValue,
	}, targetLabels)
	assert.True(t, completionUpdated)
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoveryKeepsRemediationFailedWhenUnsupportedEventRemains(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	activeEventID := "event-contact-support"
	recoveryEventID := "event-c"
	targetLabels := make([]statemanager.NVSentinelStateLabelValue, 0)

	activeEvent := testAnnotationHealthEvent(activeEventID, nodeName, protos.RecommendedAction_CONTACT_SUPPORT, "GPU-b")
	mockAnnotationManager := &MockNodeAnnotationManager{
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationFailedLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{annotationManagerOverride: mockAnnotationManager},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(_ context.Context, nodeName string,
				newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
				assert.Equal(t, "test-node", nodeName)
				assert.False(t, removeStateLabel)
				targetLabels = append(targetLabels, newStateLabelValue)

				return false, nil
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			return []datastore.HealthEventWithStatus{
				{
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{
							Status: datastore.StatusSucceeded,
						},
					},
				},
			}, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.Equal(t, []statemanager.NVSentinelStateLabelValue{
		statemanager.RemediationFailedLabelValue,
	}, targetLabels)
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoveryLeavesLabelWhenNoActiveCoveringCR(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	recoveryEventID := "event-c"
	labelUpdateCalled := false
	completionUpdated := false

	// A supported failure is still active with FaultRemediated nil. The remediation annotation
	// lists its equivalence group, but the covering CR is no longer active (Failed). Annotation
	// membership alone must not be treated as success, and the event was not skipped behind an
	// active CR, so the normal remediation flow will retry it. Leave the current label untouched.
	activeEvent := testAnnotationHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM, "GPU-a")
	mockAnnotationManager := &MockNodeAnnotationManager{
		existingCRs: map[string]string{
			"restart": "maintenance-test-node-event-a",
		},
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationFailedLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{
			annotationManagerOverride: mockAnnotationManager,
			mockStatusChecker: &mockStatusChecker{
				stateByCR: map[string]crstatus.CRState{
					"maintenance-test-node-event-a": crstatus.CRStateFailed,
				},
			},
		},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(context.Context, string,
				statemanager.NVSentinelStateLabelValue, bool) (bool, error) {
				labelUpdateCalled = true

				return true, nil
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			// Drained but remediation still pending (FaultRemediated nil).
			return []datastore.HealthEventWithStatus{
				{
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{
							Status: datastore.StatusSucceeded,
						},
					},
				},
			}, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)
			completionUpdated = true

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.False(t, labelUpdateCalled,
		"must not mark remediation-succeeded when no in-progress/succeeded covering CR exists")
	assert.True(t, completionUpdated, "recovery event should still be finalized")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoveryRecomputesSucceededWhenCoveringCRInProgress(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	recoveryEventID := "event-c"
	targetLabels := make([]statemanager.NVSentinelStateLabelValue, 0)

	// A supported failure is still active with FaultRemediated nil because it was skipped behind an
	// equivalent maintenance CR (handleExistingCRSkip never wrote FaultRemediated and did not
	// requeue). The covering CR is in progress, so remediation is underway for the node's remaining
	// failure; the stale remediation-failed label must be recomputed to remediation-succeeded,
	// matching the normal flow that sets the label on CR creation.
	activeEvent := testAnnotationHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM, "GPU-a")
	mockAnnotationManager := &MockNodeAnnotationManager{
		existingCRs: map[string]string{
			"restart": "maintenance-test-node-event-a",
		},
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationFailedLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{
			annotationManagerOverride: mockAnnotationManager,
			mockStatusChecker: &mockStatusChecker{
				stateByCR: map[string]crstatus.CRState{
					"maintenance-test-node-event-a": crstatus.CRStateInProgress,
				},
			},
		},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(_ context.Context, nodeName string,
				newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
				assert.Equal(t, "test-node", nodeName)
				assert.False(t, removeStateLabel)
				targetLabels = append(targetLabels, newStateLabelValue)

				return true, nil
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			return []datastore.HealthEventWithStatus{
				{
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{
							Status: datastore.StatusSucceeded,
						},
					},
				},
			}, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.Equal(t, []statemanager.NVSentinelStateLabelValue{
		statemanager.RemediationSucceededLabelValue,
	}, targetLabels)
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoveryDowngradesStaleRemediationSucceeded(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	activeEventID := "event-contact-support"
	recoveryEventID := "event-c"
	targetLabels := make([]statemanager.NVSentinelStateLabelValue, 0)

	// The node is currently (incorrectly) remediation-succeeded, but an unsupported
	// CONTACT_SUPPORT failure is still active. Recovering a different event must recompute
	// the label down to remediation-failed.
	activeEvent := testAnnotationHealthEvent(activeEventID, nodeName, protos.RecommendedAction_CONTACT_SUPPORT, "GPU-b")
	mockAnnotationManager := &MockNodeAnnotationManager{
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationSucceededLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{annotationManagerOverride: mockAnnotationManager},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(_ context.Context, nodeName string,
				newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
				assert.Equal(t, "test-node", nodeName)
				assert.False(t, removeStateLabel)
				targetLabels = append(targetLabels, newStateLabelValue)

				return true, nil
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			return []datastore.HealthEventWithStatus{
				{
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{
							Status: datastore.StatusSucceeded,
						},
					},
				},
			}, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.Equal(t, []statemanager.NVSentinelStateLabelValue{
		statemanager.RemediationFailedLabelValue,
	}, targetLabels)
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func TestPartialRecoverySkipsRecomputeWhenLabelNotTerminal(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	recoveryEventID := "event-c"
	labelUpdateCalled := false
	findQueryCalled := false
	completionUpdated := false

	// remediating is non-terminal: the in-progress remediation flow owns the label, so a
	// partial recovery must not recompute it.
	activeEvent := testAnnotationHealthEvent("event-a", nodeName, protos.RecommendedAction_RESTART_BM, "GPU-a")
	mockAnnotationManager := &MockNodeAnnotationManager{
		nodeAnnotations: quarantineAnnotationForTest(t, activeEvent),
		nodeLabels: map[string]string{
			statemanager.NVSentinelStateLabelKey: string(statemanager.RemediatingLabelValue),
		},
	}

	cfg := ReconcilerConfig{
		RemediationClient: &MockK8sClient{annotationManagerOverride: mockAnnotationManager},
		StateManager: &statemanager.MockStateManager{
			UpdateNVSentinelStateNodeLabelFn: func(context.Context, string,
				statemanager.NVSentinelStateLabelValue, bool) (bool, error) {
				labelUpdateCalled = true

				return true, nil
			},
		},
	}

	mockStore := &MockHealthEventStore{
		FindHealthEventsByQueryFn: func(context.Context, datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			findQueryCalled = true

			return nil, nil
		},
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, recoveryEventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)
			completionUpdated = true

			return nil
		},
	}

	r := NewFaultRemediationReconciler(nil, nil, mockStore, cfg, false)
	mockWatcher := &MockChangeStreamWatcher{}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(recoveryEventID, nodeName, protos.RecommendedAction_NONE),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handlePartialRecoveryEvent(ctx, nodeName, mockWatcher, eventWithToken, mockStore)

	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	// The node is not in a terminal remediation state, so no recompute work should happen.
	assert.False(t, findQueryCalled, "should not scan active events when label is not terminal")
	assert.False(t, labelUpdateCalled, "should not update the label when it is not terminal")
	// The recovery event must still be finalized so cold start does not replay it.
	assert.True(t, completionUpdated, "recovery event should be marked terminal")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount)
}

func quarantineAnnotationForTest(t *testing.T, events ...*protos.HealthEvent) map[string]string {
	t.Helper()

	healthEventsMap := fqannotation.NewHealthEventsAnnotationMap()
	for _, event := range events {
		healthEventsMap.AddOrUpdateEvent(event)
	}

	annotationBytes, err := json.Marshal(healthEventsMap)
	assert.NoError(t, err)

	return map[string]string{
		fqcommon.QuarantineHealthEventAnnotationKey: string(annotationBytes),
	}
}

func testAnnotationHealthEvent(
	id string,
	nodeName string,
	action protos.RecommendedAction,
	entityValue string,
) *protos.HealthEvent {
	return &protos.HealthEvent{
		Id:                id,
		Version:           1,
		Agent:             "test-agent",
		ComponentClass:    "GPU",
		CheckName:         "test-check",
		IsFatal:           true,
		IsHealthy:         false,
		RecommendedAction: action,
		NodeName:          nodeName,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU_UUID", EntityValue: entityValue},
		},
	}
}

func testRawHealthEvent(id, nodeName string, action protos.RecommendedAction) datastore.Event {
	return datastore.Event{
		"_id": id,
		"healthevent": map[string]interface{}{
			"version":           1,
			"agent":             "test-agent",
			"componentclass":    "GPU",
			"checkname":         "test-check",
			"isfatal":           true,
			"ishealthy":         false,
			"message":           "test event",
			"recommendedaction": int32(action),
			"errorcode":         []interface{}{"REPRO"},
			"entitiesimpacted": []interface{}{
				map[string]interface{}{
					"entitytype":  "GPU_UUID",
					"entityvalue": "GPU-test",
				},
			},
			"nodename": nodeName,
		},
		"healtheventstatus": map[string]interface{}{
			"nodequarantined": string(model.AlreadyQuarantined),
			"userpodsevictionstatus": map[string]interface{}{
				"status": string(model.AlreadyDrained),
			},
		},
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
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *events.HealthEventData,
					_ *common.EquivalenceGroupConfig) (string, error) {
					return "test-cr", nil
				},
				runLogCollectorJobFn: func(ctx context.Context, nodeName string) (ctrl.Result, error) {
					logCollectorCalled = true
					return ctrl.Result{}, nil
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
				r.runLogCollector(ctx, healthEvent, "")
			}

			assert.Equal(t, tt.expectLogCollectorCall, logCollectorCalled, tt.description)
		})
	}
}

func TestAdaptEvents_DoneChannelClosesOnInputClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan datastore.EventWithToken)
	_, done := AdaptEvents(ctx, in)

	// Close the input channel to simulate change stream death
	close(in)

	select {
	case <-done:
		// done channel closed as expected
	case <-time.After(2 * time.Second):
		t.Fatal("done channel was not closed after input channel closed")
	}
}

func TestAdaptEvents_DoneChannelClosesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	in := make(chan datastore.EventWithToken)
	_, done := AdaptEvents(ctx, in)

	cancel()

	select {
	case <-done:
		// done channel closed as expected
	case <-time.After(2 * time.Second):
		t.Fatal("done channel was not closed after context cancellation")
	}
}

func TestAdaptEvents_ForwardsEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan datastore.EventWithToken, 1)
	out, _ := AdaptEvents(ctx, in)

	testEvent := datastore.EventWithToken{}
	in <- testEvent

	select {
	case <-out:
		// Event forwarded successfully
	case <-time.After(2 * time.Second):
		t.Fatal("event was not forwarded through AdaptEvents")
	}
}

func nodeNotFoundErr(nodeName string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "nodes"}, nodeName)
}

func TestDeletedNodeRemediationEventMarkedTerminal(t *testing.T) {
	ctx := context.Background()
	nodeName := "deleted-node"
	eventID := "stale-event-id"
	updated := false

	mockAnnotation := &MockNodeAnnotationManager{
		getRemediationStateErr: nodeNotFoundErr(nodeName),
	}
	mockK8sClient := &MockK8sClient{
		annotationManagerOverride: mockAnnotation,
	}
	cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	mockWatcher := &MockChangeStreamWatcher{}
	mockStore := &MockHealthEventStore{
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, eventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.False(t, *status.FaultRemediated)
			updated = true
			return nil
		},
	}

	healthEventDoc := &events.HealthEventDoc{
		ID: eventID,
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			HealthEventStatus: &protos.HealthEventStatus{},
		},
	}
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(eventID, nodeName, protos.RecommendedAction_RESTART_BM),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handleRemediationEvent(ctx, healthEventDoc, eventWithToken, mockWatcher, mockStore)
	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.True(t, updated, "expected faultRemediated=false to be written")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount, "expected resume token to be marked processed")
}

func TestDeletedNodeCancellationEventMarkedTerminal(t *testing.T) {
	ctx := context.Background()
	nodeName := "deleted-node"
	eventID := "stale-cancel-event-id"
	updated := false

	mockAnnotation := &MockNodeAnnotationManager{
		getRemediationStateErr: nodeNotFoundErr(nodeName),
	}
	mockK8sClient := &MockK8sClient{
		annotationManagerOverride: mockAnnotation,
	}
	cfg := ReconcilerConfig{RemediationClient: mockK8sClient}
	r := NewFaultRemediationReconciler(nil, nil, nil, cfg, false)

	mockWatcher := &MockChangeStreamWatcher{}
	mockStore := &MockHealthEventStore{
		UpdateHealthEventStatusFn: func(_ context.Context, id string, status datastore.HealthEventStatus) error {
			assert.Equal(t, eventID, id)
			assert.NotNil(t, status.FaultRemediated)
			assert.True(t, *status.FaultRemediated)
			updated = true
			return nil
		},
	}

	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent(eventID, nodeName, protos.RecommendedAction_RESTART_BM),
		ResumeToken: []byte("resume-token"),
	}

	result, err := r.handleCancellationEvent(ctx, nodeName, model.Cancelled, mockWatcher, eventWithToken, mockStore)
	assert.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.True(t, updated, "expected faultRemediated=true to be written")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount, "expected resume token to be marked processed")
}

func TestParseHealthEventShouldNotMarkColdStartEventProcessedIfDocumentIDExtractionFails(t *testing.T) {
	ctx := context.Background()
	nodeName := "cold-start-node"
	event := testRawHealthEvent("cold-start-event", nodeName, protos.RecommendedAction_RESTART_BM)
	delete(event, "_id")

	eventWithToken := datastore.EventWithToken{Event: event}
	mockWatcher := &MockChangeStreamWatcher{}
	reconciler := &FaultRemediationReconciler{}

	_, err := reconciler.parseHealthEvent(ctx, eventWithToken, mockWatcher)

	assert.ErrorContains(t, err, "error extracting document ID")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 0, markProcessedCount, "expected cold-start event without resume token to not be marked processed")
}
