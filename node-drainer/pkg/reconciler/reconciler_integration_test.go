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

package reconciler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	annotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/customdrain"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/reconciler"
	sdkclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
	sdkconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

// mockDatabaseConfig is a simple mock implementation for testing
type mockDatabaseConfig struct {
	connectionURI  string
	databaseName   string
	collectionName string
}

// mockDataStore is a mock implementation of queue.DataStore for testing
type mockDataStore struct {
	documents map[string]map[string]any
	mu        sync.RWMutex
}

func newMockDataStore() *mockDataStore {
	return &mockDataStore{
		documents: make(map[string]map[string]any),
	}
}

func (m *mockDataStore) storeEvent(nodeName string, event map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[nodeName] = event
}

func (m *mockDataStore) UpdateDocument(ctx context.Context, filter, update interface{}) (*sdkclient.UpdateResult, error) {
	// Mock implementation - just return success
	return &sdkclient.UpdateResult{
		MatchedCount:  1,
		ModifiedCount: 1,
	}, nil
}

func (m *mockDataStore) FindDocument(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	filterMap, ok := filter.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid filter type")
	}

	nodeName, ok := filterMap["healthevent.nodename"].(string)
	if !ok {
		return nil, fmt.Errorf("nodename not found in filter")
	}

	event, exists := m.documents[nodeName]
	if !exists {
		return nil, fmt.Errorf("document not found for node: %s", nodeName)
	}

	return &mockSingleResult{document: event}, nil
}

func (m *mockDataStore) FindDocuments(ctx context.Context, filter interface{}, options *sdkclient.FindOptions) (sdkclient.Cursor, error) {
	// Mock implementation - return nil for now
	return nil, nil
}

func newMockHealthEventStore(healthEvents []datastore.HealthEventWithStatus, err error) *mockHealthEventStore {
	return &mockHealthEventStore{
		healthEvents: healthEvents,
		err:          err,
	}
}

type mockHealthEventStore struct {
	datastore.HealthEventStore
	healthEvents []datastore.HealthEventWithStatus
	err          error
}

func (mockStore *mockHealthEventStore) FindHealthEventsByQuery(_ context.Context,
	_ datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	return mockStore.healthEvents, mockStore.err
}

func (m *mockDatabaseConfig) GetConnectionURI() string {
	return m.connectionURI
}

func (m *mockDatabaseConfig) GetDatabaseName() string {
	return m.databaseName
}

func (m *mockDatabaseConfig) GetCollectionName() string {
	return m.collectionName
}

func (m *mockDatabaseConfig) GetCertConfig() sdkconfig.CertificateConfig {
	return &mockCertConfig{}
}

func (m *mockDatabaseConfig) GetTimeoutConfig() sdkconfig.TimeoutConfig {
	return &mockTimeoutConfig{}
}

func (m *mockDatabaseConfig) GetAppName() string {
	return "node-drainer"
}

// mockCertConfig is a simple mock implementation for testing
type mockCertConfig struct{}

func (m *mockCertConfig) GetCertPath() string {
	return "/tmp/test.crt"
}

func (m *mockCertConfig) GetKeyPath() string {
	return "/tmp/test.key"
}

func (m *mockCertConfig) GetCACertPath() string {
	return "/tmp/ca.crt"
}

// mockTimeoutConfig is a simple mock implementation for testing
type mockTimeoutConfig struct{}

func (m *mockTimeoutConfig) GetPingTimeoutSeconds() int {
	return 300
}

func (m *mockTimeoutConfig) GetPingIntervalSeconds() int {
	return 5
}

func (m *mockTimeoutConfig) GetCACertTimeoutSeconds() int {
	return 360
}

func (m *mockTimeoutConfig) GetCACertIntervalSeconds() int {
	return 5
}

func (m *mockTimeoutConfig) GetChangeStreamRetryDeadlineSeconds() int {
	return 60
}

func (m *mockTimeoutConfig) GetChangeStreamRetryIntervalSeconds() int {
	return 3
}

// go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
// source <(setup-envtest use -p env)
//
// TestReconciler_ProcessEvent tests the reconciler's ProcessEvent method directly (without queue).
// Each test case validates a specific draining scenario: immediate eviction, force draining,
// unquarantined events, allow completion mode, terminal states, already drained nodes, daemonsets, etc.
func TestReconciler_ProcessEvent(t *testing.T) {
	tests := []struct {
		name                            string
		nodeName                        string
		namespaces                      []string
		pods                            []*v1.Pod
		nodeQuarantined                 model.Status
		drainForce                      bool
		recommendedAction               protos.RecommendedAction
		entitiesImpacted                []*protos.Entity
		existingNodeLabels              map[string]string
		findHealthEventsByQueryResponse []datastore.HealthEventWithStatus
		expectError                     bool
		expectedNodeLabel               *string
		numReconciles                   int
		validateFunc                    func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error)
	}{
		{
			name:            "ImmediateEviction mode evicts pods",
			nodeName:        "test-node",
			namespaces:      []string{"immediate-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "immediate-test"},
					Spec:       v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")
				require.Eventually(t, func() bool {
					pods, _ := client.CoreV1().Pods("immediate-test").List(ctx, metav1.ListOptions{})
					activePodsCount := 0
					for _, pod := range pods.Items {
						if pod.DeletionTimestamp == nil {
							activePodsCount++
						}
					}
					return activePodsCount == 0
				}, 30*time.Second, 1*time.Second, "pods should be evicted")
			},
		},
		{
			name:            "DrainOverrides.Force overrides all namespace modes",
			nodeName:        "force-node",
			namespaces:      []string{"completion-test", "timeout-test"},
			nodeQuarantined: model.Quarantined,
			drainForce:      true,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "force-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "timeout-test"},
					Spec:       v1.PodSpec{NodeName: "force-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")
				// Both namespaces should have pods evicted due to force override
				require.Eventually(t, func() bool {
					pods1, _ := client.CoreV1().Pods("completion-test").List(ctx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
					})
					pods2, _ := client.CoreV1().Pods("timeout-test").List(ctx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
					})
					activePods1 := 0
					for _, pod := range pods1.Items {
						if pod.DeletionTimestamp == nil {
							activePods1++
						}
					}
					activePods2 := 0
					for _, pod := range pods2.Items {
						if pod.DeletionTimestamp == nil {
							activePods2++
						}
					}
					return activePods1 == 0 && activePods2 == 0
				}, 30*time.Second, 1*time.Second, "all pods should be evicted due to force override")
			},
		},
		{
			name:            "UnQuarantined event removes draining label",
			nodeName:        "healthy-node",
			namespaces:      []string{"test-ns"},
			nodeQuarantined: model.UnQuarantined,
			pods:            []*v1.Pod{},
			existingNodeLabels: map[string]string{
				statemanager.NVSentinelStateLabelKey: string(statemanager.DrainingLabelValue),
			},
			expectError:       false,
			expectedNodeLabel: nil,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)

				require.Eventually(t, func() bool {
					node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					require.NoError(t, err)
					_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
					return !exists
				}, 30*time.Second, 1*time.Second, "draining label should be removed")
			},
		},
		{
			name:            "Partial drain for AllowCompletion mode waits only for pods leveraging given GPU UUID",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-1", Namespace: "completion-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-123\"]}}",
					}},
					Spec: v1.PodSpec{
						NodeName: "completion-node",
						Containers: []v1.Container{{
							Name:  "c",
							Image: "nginx",
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
							},
						}},
						InitContainers: []v1.Container{{
							Name:  "c",
							Image: "nginx",
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
							},
						}},
					},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-2", Namespace: "completion-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-456\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-3", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			entitiesImpacted: []*protos.Entity{
				{
					EntityType:  "GPU_UUID",
					EntityValue: "GPU-123",
				},
				{
					EntityType:  "PCI",
					EntityValue: "PCI:0000:00:08",
				},
			},
			recommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			numReconciles:     5,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "waiting for pods to complete")

				nodeEvents, err := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName)})
				require.NoError(t, err)
				require.Len(t, nodeEvents.Items, 1, "only one event should be created despite multiple reconciliations")
				require.Equal(t, nodeEvents.Items[0].Reason, "AwaitingPodCompletion")
				expectedMessage := "Waiting for following pods to finish: [completion-test/running-pod-1]"
				require.Equal(t, expectedMessage, nodeEvents.Items[0].Message, "only expected pods should be drained")
			},
		},
		{
			name:            "Partial drain for ImmediateEviction mode only evicts for pods leveraging given GPU UUID",
			nodeName:        "test-node",
			namespaces:      []string{"immediate-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "immediate-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-123\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "immediate-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-456\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Namespace: "immediate-test"},
					Spec:       v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			entitiesImpacted: []*protos.Entity{
				{
					EntityType:  "GPU_UUID",
					EntityValue: "GPU-123",
				},
				{
					EntityType:  "PCI",
					EntityValue: "PCI:0000:00:08",
				},
			},
			recommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")

				// confirm that pod-1 was deleted but not pod-2 and pod-3
				expectDeletedForPods := map[string]bool{
					"pod-1": true,
					"pod-2": false,
					"pod-3": false,
				}
				for podName, expectDeleted := range expectDeletedForPods {
					pod, err := client.CoreV1().Pods("immediate-test").Get(ctx, podName, metav1.GetOptions{})
					require.NoError(t, err)
					assert.True(t, pod.DeletionTimestamp != nil == expectDeleted)
				}
			},
		},
		{
			name:            "Partial drain for DeleteAfterTimeout mode only evicts for pods leveraging given GPU UUID",
			nodeName:        "test-node",
			namespaces:      []string{"timeout-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "timeout-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-123\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "timeout-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-456\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Namespace: "timeout-test"},
					Spec:       v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			entitiesImpacted: []*protos.Entity{
				{
					EntityType:  "GPU_UUID",
					EntityValue: "GPU-123",
				},
				{
					EntityType:  "PCI",
					EntityValue: "PCI:0000:00:08",
				},
			},
			recommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "failed timeout eviction for node test-node: waiting for 1 pods to complete or timeout")

				nodeEvents, err := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName)})
				require.NoError(t, err)
				require.Len(t, nodeEvents.Items, 1, "only one event should be created despite multiple reconciliations")
				require.Equal(t, nodeEvents.Items[0].Reason, "WaitingBeforeForceDelete")
				expectedMessage := "Waiting for following pods to finish: [pod-1] in namespace: [timeout-test] or they will be force deleted on:"
				require.Contains(t, nodeEvents.Items[0].Message, expectedMessage, "only expected pods should be drained")
			},
		},
		{
			name:            "Partial drain failure if impacted entity is missing GPU UUID",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-1", Namespace: "completion-test", Annotations: map[string]string{
						model.PodDeviceAnnotationName: "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-123\"]}}",
					}},
					Spec:   v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			entitiesImpacted: []*protos.Entity{
				{
					EntityType:  "PCI",
					EntityValue: "PCI:0000:00:08",
				},
			},
			recommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			expectedNodeLabel: ptr.To(string(statemanager.DrainFailedLabelValue)),
			expectError:       false,
			numReconciles:     1,
		},
		{
			name:            "Partial drain failure if GPU pod is missing device annotation",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-1", Namespace: "completion-test"},
					Spec: v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{
						Name:  "c",
						Image: "nginx",
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
							},
						},
					}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			entitiesImpacted: []*protos.Entity{
				{
					EntityType:  "GPU_UUID",
					EntityValue: "GPU-123",
				},
				{
					EntityType:  "PCI",
					EntityValue: "PCI:0000:00:08",
				},
			},
			recommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			expectError:       true,
			numReconciles:     1,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "failed to filter pods using entity: pod running-pod-1 is requesting devices but is missing device annotation")
			},
		},
		{
			name:            "Terminal state events are skipped",
			nodeName:        "terminal-node",
			namespaces:      []string{"test-ns"},
			nodeQuarantined: model.Quarantined,
			pods:            []*v1.Pod{},
			expectError:     false,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:            "AlreadyQuarantined with already drained node",
			nodeName:        "already-drained-node",
			namespaces:      []string{"test-ns"},
			nodeQuarantined: model.AlreadyQuarantined,
			pods:            []*v1.Pod{},
			findHealthEventsByQueryResponse: []datastore.HealthEventWithStatus{
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
			expectError: false,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:            "DaemonSet pods are ignored during eviction",
			nodeName:        "daemonset-node",
			namespaces:      []string{"immediate-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "daemonset-pod",
						Namespace: "immediate-test",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "DaemonSet", Name: "test-ds", APIVersion: "apps/v1", UID: "test-ds-uid"},
						},
					},
					Spec:   v1.PodSpec{NodeName: "daemonset-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")
				require.Eventually(t, func() bool {
					pods, _ := client.CoreV1().Pods("immediate-test").List(ctx, metav1.ListOptions{})
					t.Logf("Pods remaining after eviction: %d", len(pods.Items))
					return len(pods.Items) == 1
				}, 30*time.Second, 1*time.Second)
			},
		},
		{
			name:            "Immediate eviction completes in single pass",
			nodeName:        "single-pass-node",
			namespaces:      []string{"immediate-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "immediate-test"},
					Spec:       v1.PodSpec{NodeName: "single-pass-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "immediate-test"},
					Spec:       v1.PodSpec{NodeName: "single-pass-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")
				require.Eventually(t, func() bool {
					pods, _ := client.CoreV1().Pods("immediate-test").List(ctx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
					})
					activePodsCount := 0
					for _, pod := range pods.Items {
						if pod.DeletionTimestamp == nil {
							activePodsCount++
						}
					}
					t.Logf("Active pods remaining after eviction: %d (total: %d)", activePodsCount, len(pods.Items))
					return activePodsCount == 0
				}, 30*time.Second, 1*time.Second, "all pods should be evicted in single pass")
			},
		},
		{
			name:            "AllowCompletion mode waits for pods to complete and verifies consistent pod ordering",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-3", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-1", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod-2", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			numReconciles:     5,
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "waiting for pods to complete")

				nodeEvents, err := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName)})
				require.NoError(t, err)
				require.Len(t, nodeEvents.Items, 1, "only one event should be created despite multiple reconciliations")
				require.Equal(t, nodeEvents.Items[0].Reason, "AwaitingPodCompletion")
				expectedMessage := "Waiting for following pods to finish: [completion-test/running-pod-1 completion-test/running-pod-2 completion-test/running-pod-3]"
				require.Equal(t, expectedMessage, nodeEvents.Items[0].Message, "pod list should be in sorted order")

				for _, podName := range []string{"running-pod-1", "running-pod-2", "running-pod-3"} {
					pod, err := client.CoreV1().Pods("completion-test").Get(ctx, podName, metav1.GetOptions{})
					require.NoError(t, err)
					pod.Status.Phase = v1.PodSucceeded
					_, err = client.CoreV1().Pods("completion-test").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
					require.NoError(t, err)
				}

				require.Eventually(t, func() bool {
					for _, podName := range []string{"running-pod-1", "running-pod-2", "running-pod-3"} {
						updatedPod, err := client.CoreV1().Pods("completion-test").Get(ctx, podName, metav1.GetOptions{})
						if err != nil || updatedPod.Status.Phase != v1.PodSucceeded {
							return false
						}
					}
					return true
				}, 30*time.Second, 1*time.Second, "all pod statuses should be updated to succeeded")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := setupDirectTest(t, []config.UserNamespace{
				{Name: "immediate-*", Mode: config.ModeImmediateEvict},
				{Name: "completion-*", Mode: config.ModeAllowCompletion},
				{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
			}, false)

			nodeLabels := tt.existingNodeLabels
			if nodeLabels == nil {
				nodeLabels = map[string]string{"test-label": "test-value"}
			}
			createNodeWithLabelsAndAnnotations(setup.ctx, t, setup.client, tt.nodeName, nodeLabels, nil)

			for _, ns := range tt.namespaces {
				createNamespace(setup.ctx, t, setup.client, ns)
			}

			for _, pod := range tt.pods {
				createPod(setup.ctx, t, setup.client, pod.Namespace, pod.Name, tt.nodeName, pod.Status.Phase, pod.Annotations, pod.Spec.Containers[0].Resources.Limits)
			}

			if tt.findHealthEventsByQueryResponse != nil {
				setup.healthEventStore.healthEvents = tt.findHealthEventsByQueryResponse
			}

			beforeReceived := getCounterValue(t, metrics.TotalEventsReceived)
			beforeDuration := getHistogramCount(t, metrics.EventHandlingDuration)

			numReconciles := tt.numReconciles
			if numReconciles == 0 {
				numReconciles = 1
			}

			var err error
			for i := 0; i < numReconciles; i++ {
				err = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore,
					healthEventOptions{
						nodeName:          tt.nodeName,
						nodeQuarantined:   tt.nodeQuarantined,
						drainForce:        tt.drainForce,
						recommendedAction: tt.recommendedAction,
						entitiesImpacted:  tt.entitiesImpacted,
					})
			}

			afterReceived := getCounterValue(t, metrics.TotalEventsReceived)
			afterDuration := getHistogramCount(t, metrics.EventHandlingDuration)
			assert.GreaterOrEqual(t, afterReceived, beforeReceived+1, "TotalEventsReceived should increment for test case: %s", tt.name)
			assert.GreaterOrEqual(t, afterDuration, beforeDuration+1, "EventHandlingDuration should record observation for test case: %s", tt.name)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectedNodeLabel != nil {
				require.Eventually(t, func() bool {
					node, err := setup.client.CoreV1().Nodes().Get(setup.ctx, tt.nodeName, metav1.GetOptions{})
					require.NoError(t, err)

					label, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
					if !exists && *tt.expectedNodeLabel == "" {
						return true
					}
					return exists && label == *tt.expectedNodeLabel
				}, 60*time.Second, 1*time.Second, "node label should be %s", *tt.expectedNodeLabel)
			}

			if tt.validateFunc != nil {
				tt.validateFunc(t, setup.client, setup.ctx, tt.nodeName, err)
			}
		})
	}
}

// TestReconciler_DryRunMode validates that dry-run mode doesn't actually evict pods,
// only simulates the eviction and logs what would happen.
func TestReconciler_DryRunMode(t *testing.T) {
	setup := setupDirectTest(t, []config.UserNamespace{
		{Name: "immediate-*", Mode: config.ModeImmediateEvict},
	}, true)

	nodeName := "dry-run-node"
	createNode(setup.ctx, t, setup.client, nodeName)
	createNamespace(setup.ctx, t, setup.client, "immediate-test")
	createPod(setup.ctx, t, setup.client, "immediate-test", "dry-pod", nodeName, v1.PodRunning, nil, nil)

	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore,
		healthEventOptions{
			nodeName:        nodeName,
			nodeQuarantined: model.Quarantined,
		})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "immediate eviction completed, requeuing for status verification")

	require.Eventually(t, func() bool {
		pods, err := setup.client.CoreV1().Pods("immediate-test").List(setup.ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		return len(pods.Items) == 1
	}, 30*time.Second, 1*time.Second)
}

// TestReconciler_RequeueMechanism validates that the queue requeues events for multi-step workflows.
// Tests immediate eviction triggers requeue, pods get evicted, and node transitions to drain-succeeded.
func TestReconciler_RequeueMechanism(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "immediate-*", Mode: config.ModeImmediateEvict},
	})

	nodeName := "requeue-node"
	createNode(setup.ctx, t, setup.client, nodeName)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "immediate-test"}}
	_, err := setup.client.CoreV1().Namespaces().Create(setup.ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)

	initialDepth := getGaugeValue(t, metrics.QueueDepth)

	createPod(setup.ctx, t, setup.client, "immediate-test", "test-pod", nodeName, v1.PodRunning, nil, nil)
	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, setup.healthEventStore, nodeName)

	require.Eventually(t, func() bool {
		currentDepth := getGaugeValue(t, metrics.QueueDepth)
		return currentDepth > initialDepth || currentDepth == 0
	}, 5*time.Second, 50*time.Millisecond, "Queue depth should change")

	assertPodsEvicted(t, setup.client, setup.ctx, "immediate-test")

	require.Eventually(t, func() bool {
		currentDepth := getGaugeValue(t, metrics.QueueDepth)
		return currentDepth == 0
	}, 10*time.Second, 100*time.Millisecond, "Queue should eventually be empty")
	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
}

// TestReconciler_AllowCompletionRequeue validates allow-completion mode with queue-based requeuing.
// Node enters draining state, waits for pods to complete, then transitions to drain-succeeded after pod completion.
func TestReconciler_AllowCompletionRequeue(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "completion-*", Mode: config.ModeAllowCompletion},
	})

	nodeName := "completion-requeue-node"
	createNode(setup.ctx, t, setup.client, nodeName)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "completion-test"}}
	_, err := setup.client.CoreV1().Namespaces().Create(setup.ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)

	createPod(setup.ctx, t, setup.client, "completion-test", "running-pod", nodeName, v1.PodRunning, nil, nil)
	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, setup.healthEventStore, nodeName)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	time.Sleep(5 * time.Second)

	updatedPod, err := setup.client.CoreV1().Pods("completion-test").Get(setup.ctx, "running-pod", metav1.GetOptions{})
	require.NoError(t, err)
	updatedPod.Status.Phase = v1.PodSucceeded
	_, err = setup.client.CoreV1().Pods("completion-test").UpdateStatus(setup.ctx, updatedPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		pod, err := setup.client.CoreV1().Pods("completion-test").Get(setup.ctx, "running-pod", metav1.GetOptions{})
		if err != nil {
			return true
		}
		if pod.Status.Phase == v1.PodSucceeded {
			pod.Finalizers = nil
			_, _ = setup.client.CoreV1().Pods("completion-test").Update(setup.ctx, pod, metav1.UpdateOptions{})
			_ = setup.client.CoreV1().Pods("completion-test").Delete(setup.ctx, "running-pod", metav1.DeleteOptions{
				GracePeriodSeconds: ptr.To(int64(0)),
			})
		}
		pods, _ := setup.client.CoreV1().Pods("completion-test").List(setup.ctx, metav1.ListOptions{})
		return len(pods.Items) == 0
	}, 10*time.Second, 500*time.Millisecond)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
}

// TestReconciler_MultipleNodesRequeue validates concurrent draining of multiple nodes through the queue.
// Ensures the queue handles multiple nodes independently without interference.
func TestReconciler_MultipleNodesRequeue(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "immediate-*", Mode: config.ModeImmediateEvict},
	})

	nodeNames := []string{"node-1", "node-2", "node-3"}
	for _, nodeName := range nodeNames {
		createNode(setup.ctx, t, setup.client, nodeName)
	}

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "immediate-test"}}
	_, err := setup.client.CoreV1().Namespaces().Create(setup.ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)

	beforeReceived := getCounterValue(t, metrics.TotalEventsReceived)

	for _, nodeName := range nodeNames {
		createPod(setup.ctx, t, setup.client, "immediate-test", fmt.Sprintf("pod-%s", nodeName), nodeName, v1.PodRunning, nil, nil)
		enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, setup.healthEventStore, nodeName)
	}

	assertPodsEvicted(t, setup.client, setup.ctx, "immediate-test")

	for _, nodeName := range nodeNames {
		assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
	}

	afterReceived := getCounterValue(t, metrics.TotalEventsReceived)
	assert.GreaterOrEqual(t, afterReceived, beforeReceived+float64(len(nodeNames)), "Should receive events for all nodes")
}

// TestReconciler_DeleteAfterTimeoutThenAllowCompletion validates that AllowCompletion mode
// is automatically processed after DeleteAfterTimeout pods are deleted.
func TestReconciler_DeleteAfterTimeoutThenAllowCompletion(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
		{Name: "completion-*", Mode: config.ModeAllowCompletion},
	})

	nodeName := "mixed-mode-requeue-node"
	createNode(setup.ctx, t, setup.client, nodeName)

	createNamespace(setup.ctx, t, setup.client, "timeout-test")
	createNamespace(setup.ctx, t, setup.client, "completion-test")

	createPod(setup.ctx, t, setup.client, "timeout-test", "timeout-pod", nodeName, v1.PodRunning, nil, nil)
	createPod(setup.ctx, t, setup.client, "completion-test", "completion-pod", nodeName, v1.PodRunning, nil, nil)

	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, setup.healthEventStore, nodeName)

	// Node should enter draining state
	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	// Simulate DeleteAfterTimeout completion by deleting the timeout pod
	t.Log("Simulating DeleteAfterTimeout completion - deleting timeout-pod")
	err := setup.client.CoreV1().Pods("timeout-test").Delete(setup.ctx, "timeout-pod", metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	})
	require.NoError(t, err)

	// Wait for timeout-pod to be fully deleted
	require.Eventually(t, func() bool {
		_, err := setup.client.CoreV1().Pods("timeout-test").Get(setup.ctx, "timeout-pod", metav1.GetOptions{})
		return errors.IsNotFound(err)
	}, 10*time.Second, 500*time.Millisecond, "timeout-pod should be deleted")

	// Verify completion-pod still exists (AllowCompletion mode waits, doesn't delete)
	completionPod, err := setup.client.CoreV1().Pods("completion-test").Get(setup.ctx, "completion-pod", metav1.GetOptions{})
	require.NoError(t, err, "completion-pod should still exist")
	assert.Nil(t, completionPod.DeletionTimestamp, "completion-pod should not be marked for deletion")

	// Verify AllowCompletion mode kicks in by checking for AwaitingPodCompletion event which will be created after
	// DeleteAfterTimeout completes on the node by the reconciler.
	t.Log("Verifying AllowCompletion mode is now processing.")
	require.Eventually(t, func() bool {
		nodeEvents, err := setup.client.CoreV1().Events(metav1.NamespaceDefault).List(setup.ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node,reason=AwaitingPodCompletion", nodeName),
		})
		if err != nil {
			return false
		}

		return len(nodeEvents.Items) > 0
	}, 15*time.Second, 500*time.Millisecond, "AwaitingPodCompletion event should be created after DeleteAfterTimeout completes")

	// Now simulate completion-pod finishing (AllowCompletion mode)
	t.Log("Simulating completion-pod finishing")
	completionPod.Status.Phase = v1.PodSucceeded
	_, err = setup.client.CoreV1().Pods("completion-test").UpdateStatus(setup.ctx, completionPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Clean up the completed pod
	require.Eventually(t, func() bool {
		pod, err := setup.client.CoreV1().Pods("completion-test").Get(setup.ctx, "completion-pod", metav1.GetOptions{})
		if err != nil {
			return true
		}
		if pod.Status.Phase == v1.PodSucceeded {
			pod.Finalizers = nil
			_, _ = setup.client.CoreV1().Pods("completion-test").Update(setup.ctx, pod, metav1.UpdateOptions{})
			_ = setup.client.CoreV1().Pods("completion-test").Delete(setup.ctx, "completion-pod", metav1.DeleteOptions{
				GracePeriodSeconds: ptr.To(int64(0)),
			})
		}
		return false
	}, 10*time.Second, 500*time.Millisecond)

	// Node should transition to drain-succeeded after both modes complete
	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
}

type MockMongoCollection struct {
	// Database-agnostic functions
	UpdateDocumentFunc func(ctx context.Context, filter interface{}, update interface{}) (*sdkclient.UpdateResult, error)
	FindDocumentFunc   func(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error)
	FindDocumentsFunc  func(ctx context.Context, filter interface{}, options *sdkclient.FindOptions) (sdkclient.Cursor, error)
}

// mockSingleResult implements sdkclient.SingleResult interface for testing
type mockSingleResult struct {
	document map[string]interface{}
	err      error
}

func (m *mockSingleResult) Decode(v interface{}) error {
	if m.err != nil {
		return m.err
	}

	resultMap, ok := v.(*map[string]any)
	if !ok {
		jsonBytes, err := json.Marshal(m.document)
		if err != nil {
			return err
		}
		return json.Unmarshal(jsonBytes, v)
	}

	if *resultMap == nil {
		*resultMap = make(map[string]any)
	}

	maps.Copy(*resultMap, m.document)
	return nil
}

func (m *mockSingleResult) Err() error {
	return m.err
}

// DataStore interface methods
func (m *MockMongoCollection) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*sdkclient.UpdateResult, error) {
	if m.UpdateDocumentFunc != nil {
		return m.UpdateDocumentFunc(ctx, filter, update)
	}
	return &sdkclient.UpdateResult{ModifiedCount: 1}, nil
}

func (m *MockMongoCollection) FindDocument(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error) {
	if m.FindDocumentFunc != nil {
		return m.FindDocumentFunc(ctx, filter, options)
	}
	// Return a mock result for tests
	return &MockSingleResult{}, nil
}

func (m *MockMongoCollection) FindDocuments(ctx context.Context, filter interface{}, options *sdkclient.FindOptions) (sdkclient.Cursor, error) {
	if m.FindDocumentsFunc != nil {
		return m.FindDocumentsFunc(ctx, filter, options)
	}
	return &MockCursor{}, nil
}

// Mock implementations for client interfaces
type MockSingleResult struct {
	DecodeFunc func(v interface{}) error
	ErrFunc    func() error
}

func (m *MockSingleResult) Decode(v interface{}) error {
	if m.DecodeFunc != nil {
		return m.DecodeFunc(v)
	}
	return nil
}

func (m *MockSingleResult) Err() error {
	if m.ErrFunc != nil {
		return m.ErrFunc()
	}
	return nil
}

type MockCursor struct {
	AllFunc    func(ctx context.Context, results interface{}) error
	NextFunc   func(ctx context.Context) bool
	CloseFunc  func(ctx context.Context) error
	DecodeFunc func(val interface{}) error
	ErrFunc    func() error
}

func (m *MockCursor) All(ctx context.Context, results interface{}) error {
	if m.AllFunc != nil {
		return m.AllFunc(ctx, results)
	}
	return nil
}

func (m *MockCursor) Next(ctx context.Context) bool {
	if m.NextFunc != nil {
		return m.NextFunc(ctx)
	}
	return false
}

func (m *MockCursor) Close(ctx context.Context) error {
	if m.CloseFunc != nil {
		return m.CloseFunc(ctx)
	}
	return nil
}

func (m *MockCursor) Decode(val interface{}) error {
	if m.DecodeFunc != nil {
		return m.DecodeFunc(val)
	}
	return nil
}

func (m *MockCursor) Err() error {
	if m.ErrFunc != nil {
		return m.ErrFunc()
	}
	return nil
}

type requeueTestSetup struct {
	*testSetup
	queueMgr queue.EventQueueManager
}

type testSetup struct {
	ctx               context.Context
	client            kubernetes.Interface
	reconciler        *reconciler.Reconciler
	mockCollection    *MockMongoCollection
	healthEventStore  *mockHealthEventStore
	informersInstance *informers.Informers
	restConfig        *rest.Config
	dynamicClient     dynamic.Interface
	mockDB            *mockDataStore
}

func setupDirectTest(t *testing.T, userNamespaces []config.UserNamespace, dryRun bool) *testSetup {
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
		PartialDrainEnabled:       true,
	}

	// Create mock database config for testing
	mockDatabaseConfig := &mockDatabaseConfig{
		connectionURI:  "mongodb://localhost:27017",
		databaseName:   "test_db",
		collectionName: "test_collection",
	}

	reconcilerConfig := config.ReconcilerConfig{
		TomlConfig:     tomlConfig,
		DatabaseConfig: mockDatabaseConfig,
		TokenConfig: sdkclient.TokenConfig{
			ClientName:      "test-client",
			TokenDatabase:   "test_db",
			TokenCollection: "tokens",
		},
		StateManager: statemanager.NewStateManager(client),
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), dryRun)
	require.NoError(t, err)

	go func() { _ = informersInstance.Run(ctx) }()
	require.Eventually(t, informersInstance.HasSynced, 30*time.Second, 1*time.Second)

	// Create a mock database client for the test
	mockDB := &mockDataStore{}
	healthEventStore := newMockHealthEventStore(nil, nil)
	r, err := reconciler.NewReconciler(reconcilerConfig, dryRun, client, informersInstance, mockDB, healthEventStore, nil, nil)
	require.NoError(t, err)

	return &testSetup{
		ctx:               ctx,
		client:            client,
		reconciler:        r,
		mockCollection:    &MockMongoCollection{},
		healthEventStore:  healthEventStore,
		informersInstance: informersInstance,
	}
}

func setupRequeueTest(t *testing.T, userNamespaces []config.UserNamespace) *requeueTestSetup {
	t.Helper()
	setup := setupDirectTest(t, userNamespaces, false)

	queueMgr := setup.reconciler.GetQueueManager()
	queueMgr.Start(setup.ctx)
	t.Cleanup(setup.reconciler.Shutdown)

	return &requeueTestSetup{
		testSetup: setup,
		queueMgr:  queueMgr,
	}
}

func setupCustomDrainTest(t *testing.T, customDrainConfig config.CustomDrainConfig) *testSetup {
	t.Helper()
	ctx := t.Context()

	testEnv := envtest.Environment{
		CRDDirectoryPaths: []string{"testdata/crds"},
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = testEnv.Stop() })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	dynamicClient, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err)

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	gvk := schema.GroupVersionKind{
		Group:   customDrainConfig.ApiGroup,
		Version: customDrainConfig.Version,
		Kind:    customDrainConfig.Kind,
	}
	require.Eventually(t, func() bool {
		_, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		return err == nil
	}, 10*time.Second, 500*time.Millisecond, "CRD should be registered")

	tomlConfig := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
		SystemNamespaces:          "kube-*",
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    2,
		CustomDrain:               customDrainConfig,
		PartialDrainEnabled:       true,
	}

	mockDatabaseConfig := &mockDatabaseConfig{
		connectionURI:  "mongodb://localhost:27017",
		databaseName:   "test_db",
		collectionName: "test_collection",
	}

	reconcilerConfig := config.ReconcilerConfig{
		TomlConfig:     tomlConfig,
		DatabaseConfig: mockDatabaseConfig,
		TokenConfig: sdkclient.TokenConfig{
			ClientName:      "test-client",
			TokenDatabase:   "test_db",
			TokenCollection: "tokens",
		},
		StateManager: statemanager.NewStateManager(client),
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), false)
	require.NoError(t, err)

	go func() { _ = informersInstance.Run(ctx) }()
	require.Eventually(t, informersInstance.HasSynced, 30*time.Second, 1*time.Second)

	mockDB := newMockDataStore()
	healthEventStore := newMockHealthEventStore(nil, nil)
	r, err := reconciler.NewReconciler(reconcilerConfig, false, client, informersInstance, mockDB, healthEventStore, dynamicClient, restMapper)
	require.NoError(t, err)

	return &testSetup{
		ctx:               ctx,
		client:            client,
		reconciler:        r,
		mockCollection:    &MockMongoCollection{},
		informersInstance: informersInstance,
		restConfig:        cfg,
		dynamicClient:     dynamicClient,
		mockDB:            mockDB,
	}
}

func createNode(ctx context.Context, t *testing.T, client kubernetes.Interface, nodeName string) {
	t.Helper()
	createNodeWithLabelsAndAnnotations(ctx, t, client, nodeName, map[string]string{"test": "true"}, nil)
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

func createNamespace(ctx context.Context, t *testing.T, client kubernetes.Interface, name string) {
	t.Helper()
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

func createPod(ctx context.Context, t *testing.T, client kubernetes.Interface, namespace, name, nodeName string, phase v1.PodPhase,
	annotations map[string]string, limits v1.ResourceList) {
	t.Helper()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
		Spec: v1.PodSpec{NodeName: nodeName, Containers: []v1.Container{{Name: "c", Image: "nginx",
			Resources: v1.ResourceRequirements{Limits: limits}}}},
		Status: v1.PodStatus{Phase: phase},
	}
	po, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	po.Status = pod.Status
	_, err = client.CoreV1().Pods(namespace).UpdateStatus(ctx, po, metav1.UpdateOptions{})
	require.NoError(t, err)
}

type healthEventOptions struct {
	nodeName          string
	nodeQuarantined   model.Status
	drainForce        bool
	recommendedAction protos.RecommendedAction
	entitiesImpacted  []*protos.Entity
}

func createHealthEvent(opts healthEventOptions) map[string]interface{} {
	healthEvent := &protos.HealthEvent{
		NodeName:          opts.nodeName,
		CheckName:         "test-check",
		RecommendedAction: opts.recommendedAction,
		EntitiesImpacted:  opts.entitiesImpacted,
	}

	if opts.drainForce {
		healthEvent.DrainOverrides = &protos.BehaviourOverrides{Force: true}
	}

	eventID := opts.nodeName + "-event"
	// Return just the fullDocument content, as the event watcher extracts this
	// from the change stream before passing to the reconciler
	return map[string]interface{}{
		"_id":         eventID,
		"healthevent": healthEvent,
		"healtheventstatus": model.HealthEventStatus{
			NodeQuarantined:        &opts.nodeQuarantined,
			UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusInProgress},
		},
		"createdAt": time.Now(),
	}
}

func enqueueHealthEvent(ctx context.Context, t *testing.T, queueMgr queue.EventQueueManager,
	collection *MockMongoCollection, healthEventStore datastore.HealthEventStore, nodeName string) {
	t.Helper()
	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	require.NoError(t, queueMgr.EnqueueEventGeneric(ctx, nodeName, event, collection, healthEventStore))
}

func processHealthEvent(ctx context.Context, t *testing.T, r *reconciler.Reconciler, collection *MockMongoCollection,
	healthEventStore datastore.HealthEventStore, opts healthEventOptions) error {
	t.Helper()
	event := createHealthEvent(opts)
	return r.ProcessEventGeneric(ctx, event, collection, healthEventStore, opts.nodeName)
}

func assertNodeLabel(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, expectedLabel statemanager.NVSentinelStateLabelValue) {
	t.Helper()
	require.Eventually(t, func() bool {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		label, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
		return exists && label == string(expectedLabel)
	}, 30*time.Second, 1*time.Second)
}

func assertPodsEvicted(t *testing.T, client kubernetes.Interface, ctx context.Context, namespace string) {
	t.Helper()

	require.Eventually(t, func() bool {
		pods, _ := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		allMarkedForDeletion := true
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil {
				allMarkedForDeletion = false
			} else {
				pod := p
				pod.Finalizers = nil
				_, _ = client.CoreV1().Pods(namespace).Update(ctx, &pod, metav1.UpdateOptions{})
			}
		}
		return allMarkedForDeletion
	}, 30*time.Second, 1*time.Second)

	require.Eventually(t, func() bool {
		pods, _ := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		for _, p := range pods.Items {
			if p.DeletionTimestamp != nil {
				_ = client.CoreV1().Pods(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
					GracePeriodSeconds: ptr.To(int64(0)),
				})
			}
		}
		return len(pods.Items) == 0
	}, 10*time.Second, 200*time.Millisecond)
}

// Metrics Tests

// TestMetrics_ProcessingErrors tests unmarshal errors
func TestMetrics_ProcessingErrors(t *testing.T) {
	setup := setupDirectTest(t, []config.UserNamespace{
		{Name: "test-*", Mode: config.ModeImmediateEvict},
	}, false)

	nodeName := "test-node"
	beforeError := getCounterVecValue(t, metrics.ProcessingErrors, "unmarshal_error", nodeName)

	invalidEvent := map[string]interface{}{
		"invalid": "structure",
	}
	_ = setup.reconciler.ProcessEventGeneric(setup.ctx, invalidEvent, setup.mockCollection, setup.healthEventStore, nodeName)

	afterError := getCounterVecValue(t, metrics.ProcessingErrors, "unmarshal_error", nodeName)
	assert.Greater(t, afterError, beforeError, "ProcessingErrors should increment for unmarshal_error")
}

// TestMetrics_NodeDrainTimeout tests timeout tracking
func TestMetrics_NodeDrainTimeout(t *testing.T) {
	setup := setupDirectTest(t, []config.UserNamespace{
		{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
	}, false)

	nodeName := "metrics-timeout-node"
	createNode(setup.ctx, t, setup.client, nodeName)
	createNamespace(setup.ctx, t, setup.client, "timeout-test")
	createPod(setup.ctx, t, setup.client, "timeout-test", "timeout-pod", nodeName, v1.PodRunning, nil, nil)

	_ = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})

	timeoutValue := getGaugeVecValue(t, metrics.NodeDrainTimeout, nodeName)
	assert.Equal(t, float64(1), timeoutValue, "NodeDrainTimeout should be 1 for node in timeout mode")
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

// TestMetrics_AlreadyQuarantinedDoesNotIncrementDrainSuccess tests that AlreadyDrained doesn't double-count
func TestMetrics_AlreadyQuarantinedDoesNotIncrementDrainSuccess(t *testing.T) {
	setup := setupDirectTest(t, []config.UserNamespace{
		{Name: "test-*", Mode: config.ModeImmediateEvict},
	}, false)

	nodeName := "metrics-already-drained-node"
	annotationValue, err := createHealthEventAnnotationsMap([]*protos.HealthEvent{
		{
			Id: "68ba1f82edf24b925c99cce8",
		},
	})
	assert.NoError(t, err)
	createNodeWithLabelsAndAnnotations(setup.ctx, t, setup.client, nodeName, map[string]string{"test": "true"},
		map[string]string{common.QuarantineHealthEventAnnotationKey: annotationValue})

	setup.healthEventStore.healthEvents = []datastore.HealthEventWithStatus{
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
	}

	beforeSkipped := getCounterVecValue(t, metrics.EventsProcessed, metrics.DrainStatusSkipped, nodeName)

	_ = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.AlreadyQuarantined,
	})

	afterSkipped := getCounterVecValue(t, metrics.EventsProcessed, metrics.DrainStatusSkipped, nodeName)
	assert.GreaterOrEqual(t, afterSkipped, beforeSkipped+1, "EventsProcessed with drain_status=skipped should increment for already drained nodes")
}

// Helper functions for reading Prometheus metrics

func getCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	err := counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getCounterVecValue(t *testing.T, counterVec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()
	counter, err := counterVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	metric := &dto.Metric{}
	err = counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	err := gauge.Write(metric)
	require.NoError(t, err)
	return metric.Gauge.GetValue()
}

func getGaugeVecValue(t *testing.T, gaugeVec *prometheus.GaugeVec, labelValues ...string) float64 {
	t.Helper()
	gauge, err := gaugeVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	metric := &dto.Metric{}
	err = gauge.Write(metric)
	require.NoError(t, err)
	return metric.Gauge.GetValue()
}

func getHistogramCount(t *testing.T, histogram prometheus.Histogram) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	err := histogram.Write(metric)
	require.NoError(t, err)
	return metric.Histogram.GetSampleCount()
}

// TestReconciler_CancelledEventWithOngoingDrain validates that Cancelled events stop ongoing drain operations
func TestReconciler_CancelledEventWithOngoingDrain(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
	})

	nodeName := testutils.GenerateTestNodeName("cancel-during-drain-node")
	createNode(setup.ctx, t, setup.client, nodeName)

	createNamespace(setup.ctx, t, setup.client, "timeout-test")

	createPod(setup.ctx, t, setup.client, "timeout-test", "stuck-pod", nodeName, v1.PodRunning, nil, nil)

	beforeCancelled := getCounterVecValue(t, metrics.CancelledEvent, nodeName, "test-check")

	t.Log("Enqueue Quarantined event - should start deleteAfterTimeout drain")
	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	document := event
	eventID := fmt.Sprintf("%v", document["_id"])

	healthEventStore := newMockHealthEventStore(nil, nil)
	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event, setup.mockCollection, healthEventStore)
	require.NoError(t, err)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	t.Log("Simulate Cancelled event from change stream - should stop draining immediately")
	setup.reconciler.HandleCancellation(eventID, nodeName, model.Cancelled)

	require.Eventually(t, func() bool {
		node, err := setup.client.CoreV1().Nodes().Get(setup.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
		return !exists
	}, 15*time.Second, 500*time.Millisecond, "draining label should be removed quickly after cancellation")

	afterCancelled := getCounterVecValue(t, metrics.CancelledEvent, nodeName, "test-check")
	assert.GreaterOrEqual(t, afterCancelled, beforeCancelled+1, "CancelledEvent metric should increment")

	pod, err := setup.client.CoreV1().Pods("timeout-test").Get(setup.ctx, "stuck-pod", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, pod.DeletionTimestamp, "pod should not be deleted after cancellation")
}

// TestReconciler_UnQuarantinedEventCancelsOngoingDrain validates that UnQuarantined events cancel all in-progress drains for a node
func TestReconciler_UnQuarantinedEventCancelsOngoingDrain(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
	})

	nodeName := testutils.GenerateTestNodeName("unquarantine-cancel-node")
	createNode(setup.ctx, t, setup.client, nodeName)

	createNamespace(setup.ctx, t, setup.client, "timeout-test")

	createPod(setup.ctx, t, setup.client, "timeout-test", "stuck-pod", nodeName, v1.PodRunning, nil, nil)

	t.Log("Enqueue Quarantined event - should start deleteAfterTimeout drain")
	quarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})

	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, quarantinedEvent, setup.mockCollection,
		setup.healthEventStore)
	require.NoError(t, err)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	t.Log("Simulate UnQuarantined event from change stream - should cancel in-progress drains")
	setup.reconciler.HandleCancellation("", nodeName, model.UnQuarantined)

	t.Log("Enqueue UnQuarantined event - should process and clean up")
	unquarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.UnQuarantined,
	})
	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, unquarantinedEvent, setup.mockCollection,
		setup.healthEventStore)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		node, err := setup.client.CoreV1().Nodes().Get(setup.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
		return !exists
	}, 15*time.Second, 500*time.Millisecond, "draining label should be removed after UnQuarantined event")

	pod, err := setup.client.CoreV1().Pods("timeout-test").Get(setup.ctx, "stuck-pod", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, pod.DeletionTimestamp, "pod should not be deleted after cancellation")
}

// TestReconciler_MultipleEventsOnNodeCancelledByUnQuarantine validates that UnQuarantined cancels all in-progress events
func TestReconciler_MultipleEventsOnNodeCancelledByUnQuarantine(t *testing.T) {
	setup := setupRequeueTest(t, []config.UserNamespace{
		{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
	})

	nodeName := testutils.GenerateTestNodeName("multi-event-cancel-node")
	createNode(setup.ctx, t, setup.client, nodeName)

	createNamespace(setup.ctx, t, setup.client, "timeout-test")

	createPod(setup.ctx, t, setup.client, "timeout-test", "pod-1", nodeName, v1.PodRunning, nil, nil)
	createPod(setup.ctx, t, setup.client, "timeout-test", "pod-2", nodeName, v1.PodRunning, nil, nil)

	t.Log("Enqueue two Quarantined events for the same node")
	event1 := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	event1["_id"] = nodeName + "-event-1"

	event2 := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	event2["_id"] = nodeName + "-event-2"

	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event1, setup.mockCollection, setup.healthEventStore)
	require.NoError(t, err)

	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event2, setup.mockCollection, setup.healthEventStore)
	require.NoError(t, err)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	t.Log("Send UnQuarantined event - should cancel both in-progress events")
	setup.reconciler.HandleCancellation("", nodeName, model.UnQuarantined)

	unquarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.UnQuarantined,
	})
	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, unquarantinedEvent, setup.mockCollection,
		setup.healthEventStore)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		node, err := setup.client.CoreV1().Nodes().Get(setup.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
		return !exists
	}, 15*time.Second, 500*time.Millisecond, "draining label should be removed after UnQuarantined")

	pod1, err := setup.client.CoreV1().Pods("timeout-test").Get(setup.ctx, "pod-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, pod1.DeletionTimestamp, "pod-1 should not be deleted")

	pod2, err := setup.client.CoreV1().Pods("timeout-test").Get(setup.ctx, "pod-2", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, pod2.DeletionTimestamp, "pod-2 should not be deleted")
}

func TestReconciler_CustomDrainHappyPath(t *testing.T) {
	customDrainCfg := config.CustomDrainConfig{
		Enabled:               true,
		TemplateMountPath:     "../customdrain/testdata",
		TemplateFileName:      "drain-template.yaml",
		Namespace:             "default",
		ApiGroup:              "drain.example.com",
		Version:               "v1alpha1",
		Kind:                  "DrainRequest",
		StatusConditionType:   "Complete",
		StatusConditionStatus: "True",
		Timeout:               config.Duration{Duration: 30 * time.Minute},
	}

	setup := setupCustomDrainTest(t, customDrainCfg)

	nodeName := "custom-drain-node"
	createNode(setup.ctx, t, setup.client, nodeName)
	createNamespace(setup.ctx, t, setup.client, "default")
	createNamespace(setup.ctx, t, setup.client, "app-ns")

	createPod(setup.ctx, t, setup.client, "default", "pod-1", nodeName, v1.PodRunning, nil, nil)
	createPod(setup.ctx, t, setup.client, "app-ns", "app-pod", nodeName, v1.PodRunning, nil, nil)

	gvr := schema.GroupVersionResource{
		Group:    "drain.example.com",
		Version:  "v1alpha1",
		Resource: "drainrequests",
	}

	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	setup.mockDB.storeEvent(nodeName, event)

	err := setup.reconciler.ProcessEventGeneric(setup.ctx, event, setup.mockDB, setup.healthEventStore, nodeName)
	require.Error(t, err, "First call should create CR and return error to trigger retry")
	assert.Contains(t, err.Error(), "waiting for custom drain CR to complete")

	eventID := nodeName + "-event"
	crName := customdrain.GenerateCRName(nodeName, eventID)

	var retrievedCR *unstructured.Unstructured
	require.Eventually(t, func() bool {
		cr, err := setup.dynamicClient.Resource(gvr).Namespace("default").Get(setup.ctx, crName, metav1.GetOptions{})
		if err == nil {
			retrievedCR = cr
			return true
		}
		return false
	}, 10*time.Second, 500*time.Millisecond, "DrainRequest CR should be created")

	spec, found, err := unstructured.NestedMap(retrievedCR.Object, "spec")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, nodeName, spec["nodeName"])

	err = setup.reconciler.ProcessEventGeneric(setup.ctx, event, setup.mockDB, setup.healthEventStore, nodeName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for retry delay")

	retrievedCR, err = setup.dynamicClient.Resource(gvr).Namespace("default").Get(setup.ctx, crName, metav1.GetOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedField(retrievedCR.Object, []any{
		map[string]any{
			"type":               "Complete",
			"status":             "True",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
			"reason":             "DrainSucceeded",
			"message":            "All pods drained",
		},
	}, "status", "conditions")
	require.NoError(t, err)

	_, err = setup.dynamicClient.Resource(gvr).Namespace("default").UpdateStatus(setup.ctx, retrievedCR, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = setup.reconciler.ProcessEventGeneric(setup.ctx, event, setup.mockDB, setup.healthEventStore, nodeName)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := setup.dynamicClient.Resource(gvr).Namespace("default").Get(setup.ctx, crName, metav1.GetOptions{})
		return errors.IsNotFound(err)
	}, 10*time.Second, 500*time.Millisecond, "CR should be deleted after drain completes")
}

func TestReconciler_CustomDrainCRDNotFound(t *testing.T) {
	customDrainCfg := config.CustomDrainConfig{
		Enabled:               true,
		TemplateMountPath:     "../customdrain/testdata",
		TemplateFileName:      "drain-template.yaml",
		Namespace:             "default",
		ApiGroup:              "nonexistent.example.com",
		Version:               "v1",
		Kind:                  "FakeResource",
		StatusConditionType:   "Complete",
		StatusConditionStatus: "True",
		Timeout:               config.Duration{Duration: 30 * time.Minute},
	}

	ctx := context.Background()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	defer func() { _ = testEnv.Stop() }()

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	dynamicClient, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err)

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	tomlConfig := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
		SystemNamespaces:          "kube-*",
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    2,
		CustomDrain:               customDrainCfg,
		UserNamespaces: []config.UserNamespace{
			{Name: "*", Mode: config.ModeImmediateEvict},
		},
		PartialDrainEnabled: true,
	}

	mockDatabaseConfig := &mockDatabaseConfig{
		connectionURI:  "mongodb://localhost:27017",
		databaseName:   "test_db",
		collectionName: "test_collection",
	}

	reconcilerConfig := config.ReconcilerConfig{
		TomlConfig:     tomlConfig,
		DatabaseConfig: mockDatabaseConfig,
		TokenConfig: sdkclient.TokenConfig{
			ClientName:      "test-client",
			TokenDatabase:   "test_db",
			TokenCollection: "tokens",
		},
		StateManager: statemanager.NewStateManager(client),
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), false)
	require.NoError(t, err)

	go func() { _ = informersInstance.Run(ctx) }()
	require.Eventually(t, informersInstance.HasSynced, 30*time.Second, 1*time.Second)

	mockDB := newMockDataStore()
	healthEventStore := newMockHealthEventStore(nil, nil)
	r, err := reconciler.NewReconciler(reconcilerConfig, false, client, informersInstance, mockDB,
		healthEventStore, dynamicClient, restMapper)

	// Verify that reconciler creation failed when CRD doesn't exist
	require.Error(t, err, "Reconciler initialization should fail when custom drain CRD doesn't exist")
	require.Nil(t, r, "Reconciler should be nil when initialization fails")
	assert.Contains(t, err.Error(), "failed to initialize custom drain client", "Error should indicate custom drain client initialization failure")
	assert.Contains(t, err.Error(), "failed to find rest mapping for custom drain CRD", "Error should indicate CRD validation failure")
}
