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
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/reconciler"
	sdkclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
	sdkconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// mockDatabaseConfig is a simple mock implementation for testing
type mockDatabaseConfig struct {
	connectionURI  string
	databaseName   string
	collectionName string
}

// mockDataStore is a mock implementation of queue.DataStore for testing
type mockDataStore struct{}

func (m *mockDataStore) UpdateDocument(ctx context.Context, filter, update interface{}) (*sdkclient.UpdateResult, error) {
	// Mock implementation - just return success
	return &sdkclient.UpdateResult{
		MatchedCount:  1,
		ModifiedCount: 1,
	}, nil
}

func (m *mockDataStore) FindDocument(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error) {
	// Mock implementation - return nil for now
	return nil, nil
}

func (m *mockDataStore) FindDocuments(ctx context.Context, filter interface{}, options *sdkclient.FindOptions) (sdkclient.Cursor, error) {
	// Mock implementation - return nil for now
	return nil, nil
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
		name                 string
		nodeName             string
		namespaces           []string
		pods                 []*v1.Pod
		nodeQuarantined      model.Status
		drainForce           bool
		existingNodeLabels   map[string]string
		mongoFindOneResponse map[string]interface{}
		expectError          bool
		expectedNodeLabel    *string
		numReconciles        int
		validateFunc         func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error)
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
			mongoFindOneResponse: map[string]interface{}{
				"healtheventstatus": map[string]interface{}{
					"nodequarantined": string(model.Quarantined),
					"userpodsevictionstatus": map[string]interface{}{
						"status": string(model.StatusSucceeded),
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
			createNodeWithLabels(setup.ctx, t, setup.client, tt.nodeName, nodeLabels)

			for _, ns := range tt.namespaces {
				createNamespace(setup.ctx, t, setup.client, ns)
			}

			for _, pod := range tt.pods {
				createPod(setup.ctx, t, setup.client, pod.Namespace, pod.Name, tt.nodeName, pod.Status.Phase)
			}

			if tt.mongoFindOneResponse != nil {
				setup.mockCollection.FindDocumentFunc = func(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error) {
					// Return a mock single result that contains the test data
					return &mockSingleResult{document: tt.mongoFindOneResponse}, nil
				}
			}

			beforeReceived := getCounterValue(t, metrics.TotalEventsReceived)
			beforeDuration := getHistogramCount(t, metrics.EventHandlingDuration)

			numReconciles := tt.numReconciles
			if numReconciles == 0 {
				numReconciles = 1
			}

			var err error
			for i := 0; i < numReconciles; i++ {
				err = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, healthEventOptions{
					nodeName:        tt.nodeName,
					nodeQuarantined: tt.nodeQuarantined,
					drainForce:      tt.drainForce,
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
	createPod(setup.ctx, t, setup.client, "immediate-test", "dry-pod", nodeName, v1.PodRunning)

	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, healthEventOptions{
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

	createPod(setup.ctx, t, setup.client, "immediate-test", "test-pod", nodeName, v1.PodRunning)
	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, nodeName)

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

	createPod(setup.ctx, t, setup.client, "completion-test", "running-pod", nodeName, v1.PodRunning)
	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, nodeName)

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
		createPod(setup.ctx, t, setup.client, "immediate-test", fmt.Sprintf("pod-%s", nodeName), nodeName, v1.PodRunning)
		enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, nodeName)
	}

	assertPodsEvicted(t, setup.client, setup.ctx, "immediate-test")

	for _, nodeName := range nodeNames {
		assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
	}

	afterReceived := getCounterValue(t, metrics.TotalEventsReceived)
	assert.GreaterOrEqual(t, afterReceived, beforeReceived+float64(len(nodeNames)), "Should receive events for all nodes")
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
	// Simple JSON marshal/unmarshal to simulate document decoding
	jsonBytes, err := json.Marshal(m.document)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonBytes, v)
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
	informersInstance *informers.Informers
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
	r := reconciler.NewReconciler(reconcilerConfig, dryRun, client, informersInstance, mockDB)

	return &testSetup{
		ctx:               ctx,
		client:            client,
		reconciler:        r,
		mockCollection:    &MockMongoCollection{},
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

func createNode(ctx context.Context, t *testing.T, client kubernetes.Interface, nodeName string) {
	t.Helper()
	createNodeWithLabels(ctx, t, client, nodeName, map[string]string{"test": "true"})
}

func createNodeWithLabels(ctx context.Context, t *testing.T, client kubernetes.Interface, nodeName string, labels map[string]string) {
	t.Helper()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: labels},
		Status:     v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}},
	}
	_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createNamespace(ctx context.Context, t *testing.T, client kubernetes.Interface, name string) {
	t.Helper()
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createPod(ctx context.Context, t *testing.T, client kubernetes.Interface, namespace, name, nodeName string, phase v1.PodPhase) {
	t.Helper()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1.PodSpec{NodeName: nodeName, Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
		Status:     v1.PodStatus{Phase: phase},
	}
	po, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	po.Status = pod.Status
	_, err = client.CoreV1().Pods(namespace).UpdateStatus(ctx, po, metav1.UpdateOptions{})
	require.NoError(t, err)
}

type healthEventOptions struct {
	nodeName        string
	nodeQuarantined model.Status
	drainForce      bool
}

func createHealthEvent(opts healthEventOptions) map[string]interface{} {
	healthEvent := &protos.HealthEvent{
		NodeName:  opts.nodeName,
		CheckName: "test-check",
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

func enqueueHealthEvent(ctx context.Context, t *testing.T, queueMgr queue.EventQueueManager, collection *MockMongoCollection, nodeName string) {
	t.Helper()
	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	require.NoError(t, queueMgr.EnqueueEventGeneric(ctx, nodeName, event, collection))
}

func processHealthEvent(ctx context.Context, t *testing.T, r *reconciler.Reconciler, collection *MockMongoCollection, opts healthEventOptions) error {
	t.Helper()
	event := createHealthEvent(opts)
	return r.ProcessEventGeneric(ctx, event, collection, opts.nodeName)
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
	_ = setup.reconciler.ProcessEventGeneric(setup.ctx, invalidEvent, setup.mockCollection, nodeName)

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
	createPod(setup.ctx, t, setup.client, "timeout-test", "timeout-pod", nodeName, v1.PodRunning)

	_ = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})

	timeoutValue := getGaugeVecValue(t, metrics.NodeDrainTimeout, nodeName)
	assert.Equal(t, float64(1), timeoutValue, "NodeDrainTimeout should be 1 for node in timeout mode")
}

// TestMetrics_AlreadyQuarantinedDoesNotIncrementDrainSuccess tests that AlreadyDrained doesn't double-count
func TestMetrics_AlreadyQuarantinedDoesNotIncrementDrainSuccess(t *testing.T) {
	setup := setupDirectTest(t, []config.UserNamespace{
		{Name: "test-*", Mode: config.ModeImmediateEvict},
	}, false)

	nodeName := "metrics-already-drained-node"
	createNode(setup.ctx, t, setup.client, nodeName)

	setup.mockCollection.FindDocumentFunc = func(ctx context.Context, filter interface{}, options *sdkclient.FindOneOptions) (sdkclient.SingleResult, error) {
		response := map[string]interface{}{
			"healtheventstatus": map[string]interface{}{
				"nodequarantined": string(model.Quarantined),
				"userpodsevictionstatus": map[string]interface{}{
					"status": string(model.StatusSucceeded),
				},
			},
		}
		return &mockSingleResult{document: response}, nil
	}

	beforeSkipped := getCounterVecValue(t, metrics.EventsProcessed, metrics.DrainStatusSkipped, nodeName)

	_ = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, healthEventOptions{
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

	createPod(setup.ctx, t, setup.client, "timeout-test", "stuck-pod", nodeName, v1.PodRunning)

	beforeCancelled := getCounterVecValue(t, metrics.CancelledEvent, nodeName, "test-check")

	t.Log("Enqueue Quarantined event - should start deleteAfterTimeout drain")
	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	document := event
	eventID := fmt.Sprintf("%v", document["_id"])

	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event, setup.mockCollection)
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

	createPod(setup.ctx, t, setup.client, "timeout-test", "stuck-pod", nodeName, v1.PodRunning)

	t.Log("Enqueue Quarantined event - should start deleteAfterTimeout drain")
	quarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})

	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, quarantinedEvent, setup.mockCollection)
	require.NoError(t, err)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	t.Log("Simulate UnQuarantined event from change stream - should cancel in-progress drains")
	setup.reconciler.HandleCancellation("", nodeName, model.UnQuarantined)

	t.Log("Enqueue UnQuarantined event - should process and clean up")
	unquarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.UnQuarantined,
	})
	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, unquarantinedEvent, setup.mockCollection)
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

	createPod(setup.ctx, t, setup.client, "timeout-test", "pod-1", nodeName, v1.PodRunning)
	createPod(setup.ctx, t, setup.client, "timeout-test", "pod-2", nodeName, v1.PodRunning)

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

	err := setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event1, setup.mockCollection)
	require.NoError(t, err)

	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, event2, setup.mockCollection)
	require.NoError(t, err)

	assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainingLabelValue)

	t.Log("Send UnQuarantined event - should cancel both in-progress events")
	setup.reconciler.HandleCancellation("", nodeName, model.UnQuarantined)

	unquarantinedEvent := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.UnQuarantined,
	})
	err = setup.queueMgr.EnqueueEventGeneric(setup.ctx, nodeName, unquarantinedEvent, setup.mockCollection)
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

