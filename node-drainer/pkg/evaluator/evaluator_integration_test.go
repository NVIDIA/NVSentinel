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

package evaluator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	annotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type mockDataStore struct {
	queue.DataStore
}

func newMockDataStore() *mockDataStore {
	return &mockDataStore{}
}

type mockHealthEventStore struct {
	datastore.HealthEventStore
	healthEvents [][]datastore.HealthEventWithStatus
	callCount    int
	err          error
}

func newMockHealthEventStore(healthEvents [][]datastore.HealthEventWithStatus, err error) *mockHealthEventStore {
	return &mockHealthEventStore{
		callCount:    0,
		healthEvents: healthEvents,
		err:          err,
	}
}

func (mockStore *mockHealthEventStore) FindHealthEventsByQuery(_ context.Context,
	_ datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	if mockStore.err != nil {
		return nil, mockStore.err
	}
	healthEventsForCall := mockStore.healthEvents[mockStore.callCount]
	if mockStore.callCount < len(mockStore.healthEvents)-1 {
		mockStore.callCount++
	}
	return healthEventsForCall, nil
}

type testSetup struct {
	ctx               context.Context
	client            kubernetes.Interface
	dynamicClient     dynamic.Interface
	mockDB            *mockDataStore
	healthEventStore  *mockHealthEventStore
	evaluator         *NodeDrainEvaluator
	informersInstance *informers.Informers
}

func setupDirectTest(t *testing.T, userNamespaces []config.UserNamespace, dryRun, partialDrainEnabled bool) *testSetup {
	t.Helper()
	ctx := t.Context()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = testEnv.Stop() })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	tomlConfig := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
		SystemNamespaces:          "kube-*",
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    2,
		UserNamespaces:            userNamespaces,
		PartialDrainEnabled:       partialDrainEnabled,
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), dryRun)
	require.NoError(t, err)
	go func() { _ = informersInstance.Run(ctx) }()
	require.Eventually(t, informersInstance.HasSynced, 30*time.Second, 1*time.Second)

	mockDB := newMockDataStore()
	healthEventStore := newMockHealthEventStore(nil, nil)

	evaluator := &NodeDrainEvaluator{
		config:    tomlConfig,
		informers: informersInstance,
	}
	return &testSetup{
		ctx:               ctx,
		client:            client,
		evaluator:         evaluator,
		mockDB:            mockDB,
		healthEventStore:  healthEventStore,
		informersInstance: informersInstance,
	}
}

func TestEvaluator_IsNodeAlreadyDrained(t *testing.T) {
	tests := []struct {
		name                     string
		nodeName                 string
		annotationHealthEvents   []*protos.HealthEvent
		nodeAnnotations          map[string]string
		healthEventsFromStore    [][]datastore.HealthEventWithStatus
		healthEventStoreError    error
		partialDrainEntity       *protos.Entity
		partialDrainEnabled      bool
		currentEventId           string
		expectError              bool
		expectedErrMessage       string
		expectedIsAlreadyDrained bool
	}{
		{
			name:     "Missing quarantineHealthEvent annotation",
			nodeName: "test-node",
			nodeAnnotations: map[string]string{
				"key1": "value1",
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Unmarshal quarantineHealthEvent annotation failure",
			nodeName: "test-node",
			nodeAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "value1",
			},
			partialDrainEnabled:      true,
			expectError:              true,
			expectedErrMessage:       "failed to unmarshal quarantine annotation for node",
			expectedIsAlreadyDrained: false,
		},
		{
			name:                     "Node not drained: quarantineHealthEvent contains no events",
			nodeName:                 "test-node",
			annotationHealthEvents:   nil,
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "quarantineHealthEvent is missing Id",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					NodeName: "test-node",
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "FindHealthEventsByQuery returns error",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			healthEventStoreError:    errors.New("fake error"),
			partialDrainEnabled:      true,
			expectError:              true,
			expectedErrMessage:       "failed to query health events for node",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Multiple events returned from FindHealthEventsByQuery",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
					{
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              true,
			expectedErrMessage:       "unexpected number of events for node",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Full drain in progress",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusInProgress,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Full drain completed, returning already drained for current full drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: true,
		},
		{
			name:     "Full drain completed, returning already drained for current partial drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "123",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: true,
		},
		{
			name:     "Partial drain in progress for matching entity",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "123",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusInProgress,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
							EntitiesImpacted: []*protos.Entity{
								{
									EntityType:  "GPU_UUID",
									EntityValue: "123",
								},
							},
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Partial drain in completed for matching entity, returning already drained for current partial drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "123",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
							EntitiesImpacted: []*protos.Entity{
								{
									EntityType:  "GPU_UUID",
									EntityValue: "123",
								},
							},
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: true,
		},
		{
			name:     "Partial drain in completed for non-matching entity, returning not drained for current partial drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "456",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
							EntitiesImpacted: []*protos.Entity{
								{
									EntityType:  "GPU_UUID",
									EntityValue: "123",
								},
							},
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Partial drain in completed for non-matching entity, feature not enabled so considering it as full drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "456",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
							EntitiesImpacted: []*protos.Entity{
								{
									EntityType:  "GPU_UUID",
									EntityValue: "123",
								},
							},
						},
					},
				},
			},
			partialDrainEnabled:      false,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: true,
		},
		{
			name:     "Multiple events: full drain and non-matching partial drain, returning drained for current partial drain",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id:    "68ba1f82edf24b925c99cce8",
					Agent: "0",
				},
				{
					Id:    "68bd4fc9ffa9f5eca91c340c",
					Agent: "1",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "456",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
							EntitiesImpacted: []*protos.Entity{
								{
									EntityType:  "GPU_UUID",
									EntityValue: "123",
								},
							},
						},
					},
				},
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_RESTART_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: true,
		},
		{
			name:     "Invalid partialDrainEntity",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			currentEventId: "68ba1f82edf24b925c99cce8",
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "456",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_REPLACE_VM,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              false,
			expectedErrMessage:       "",
			expectedIsAlreadyDrained: false,
		},
		{
			name:     "Matching event Id is skipped",
			nodeName: "test-node",
			annotationHealthEvents: []*protos.HealthEvent{
				{
					Id: "68ba1f82edf24b925c99cce8",
				},
			},
			partialDrainEntity: &protos.Entity{
				EntityType:  "GPU_UUID",
				EntityValue: "456",
			},
			healthEventsFromStore: [][]datastore.HealthEventWithStatus{
				{
					{
						HealthEventStatus: datastore.HealthEventStatus{
							NodeQuarantined: ptr.To(datastore.Quarantined),
							UserPodsEvictionStatus: datastore.OperationStatus{
								Status: datastore.StatusSucceeded,
							},
						},
						HealthEvent: &protos.HealthEvent{
							RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
						},
					},
				},
			},
			partialDrainEnabled:      true,
			expectError:              true,
			expectedErrMessage:       "no supported entities for a partial drain found",
			expectedIsAlreadyDrained: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := setupDirectTest(t, []config.UserNamespace{
				{Name: "test-*", Mode: config.ModeImmediateEvict},
			}, false, tt.partialDrainEnabled)

			annotationsForNode := tt.nodeAnnotations
			if annotationsForNode == nil {
				annotationValue, err := createHealthEventAnnotationsMap(tt.annotationHealthEvents)
				require.NoError(t, err)
				annotationsForNode = map[string]string{common.QuarantineHealthEventAnnotationKey: annotationValue}
			}

			createNodeWithLabelsAndAnnotations(setup.ctx, t, setup.client, tt.nodeName, map[string]string{"test": "true"},
				annotationsForNode)

			require.Eventually(t, func() bool {
				_, err := setup.informersInstance.GetNode(tt.nodeName)
				return err == nil

			}, 30*time.Second, 1*time.Second)

			setup.healthEventStore.healthEvents = tt.healthEventsFromStore
			setup.healthEventStore.err = tt.healthEventStoreError

			isDrained, err := setup.evaluator.isNodeAlreadyDrained(setup.ctx, tt.currentEventId, tt.partialDrainEntity,
				tt.nodeName, setup.healthEventStore)

			if tt.expectError {
				require.False(t, isDrained)
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectedErrMessage)
			} else {
				require.Equal(t, isDrained, tt.expectedIsAlreadyDrained)
				require.NoError(t, err)
			}
		})
	}
}

func createNodeWithLabelsAndAnnotations(ctx context.Context, t *testing.T, client kubernetes.Interface,
	nodeName string, labels map[string]string, annotations map[string]string) {
	t.Helper()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: labels, Annotations: annotations},
		Status:     v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}},
	}
	_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createHealthEventAnnotationsMap(healthEvents []*protos.HealthEvent) (string, error) {
	healthEventsAnnotationsMap := &annotation.HealthEventsAnnotationMap{
		Events: make(map[annotation.HealthEventKey]*protos.HealthEvent),
	}
	for _, event := range healthEvents {
		healthEventsAnnotationsMap.Events[annotation.CreateEventKeyForEntity(event, nil)] = event
	}
	annotationsMapBytes, err := healthEventsAnnotationsMap.MarshalJSON()
	if err != nil {
		return "", err
	}
	return string(annotationsMapBytes), nil
}
