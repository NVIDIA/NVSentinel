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
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

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
		mongoFindOneResponse *bson.M
		expectError          bool
		expectedNodeLabel    *string
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
					t.Logf("Active pods remaining after eviction: %d (total: %d)", activePodsCount, len(pods.Items))
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
					slog.Info("Node %s labels: %v", nodeName, node.Labels)
					_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
					return !exists
				}, 30*time.Second, 1*time.Second, "draining label should be removed")
			},
		},
		{
			name:            "AllowCompletion mode waits for pods to complete",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: model.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "running-pod", Namespace: "completion-test"},
					Spec:       v1.PodSpec{NodeName: "completion-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       true,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "waiting for pods to complete")

				nodeEvents, err := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName)})
				require.NoError(t, err)
				require.Len(t, nodeEvents.Items, 1)
				require.Equal(t, nodeEvents.Items[0].Reason, "AwaitingPodCompletion")
				require.Equal(t, nodeEvents.Items[0].Message, "Waiting for following pods to finish: [completion-test/running-pod]")

				pod, err := client.CoreV1().Pods("completion-test").Get(ctx, "running-pod", metav1.GetOptions{})
				require.NoError(t, err)
				pod.Status.Phase = v1.PodSucceeded
				_, err = client.CoreV1().Pods("completion-test").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)

				require.Eventually(t, func() bool {
					updatedPod, err := client.CoreV1().Pods("completion-test").Get(ctx, "running-pod", metav1.GetOptions{})
					return err == nil && updatedPod.Status.Phase == v1.PodSucceeded
				}, 30*time.Second, 1*time.Second, "pod status should be updated to succeeded")
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
			mongoFindOneResponse: &bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(model.Quarantined),
					"userpodsevictionstatus": bson.M{
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
				setup.mockCollection.FindOneFunc = func(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult {
					return mongo.NewSingleResultFromDocument(*tt.mongoFindOneResponse, nil, nil)
				}
			}

			err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, healthEventOptions{
				nodeName:        tt.nodeName,
				nodeQuarantined: tt.nodeQuarantined,
				drainForce:      tt.drainForce,
			})

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

	createPod(setup.ctx, t, setup.client, "immediate-test", "test-pod", nodeName, v1.PodRunning)
	enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, nodeName)

	assertPodsEvicted(t, setup.client, setup.ctx, "immediate-test")
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

	for _, nodeName := range nodeNames {
		createPod(setup.ctx, t, setup.client, "immediate-test", fmt.Sprintf("pod-%s", nodeName), nodeName, v1.PodRunning)
		enqueueHealthEvent(setup.ctx, t, setup.queueMgr, setup.mockCollection, nodeName)
	}

	assertPodsEvicted(t, setup.client, setup.ctx, "immediate-test")

	for _, nodeName := range nodeNames {
		assertNodeLabel(t, setup.client, setup.ctx, nodeName, statemanager.DrainSucceededLabelValue)
	}
}

type MockMongoCollection struct {
	UpdateOneFunc func(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	FindOneFunc   func(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult
	FindFunc      func(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, error)
}

func (m *MockMongoCollection) UpdateOne(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	if m.UpdateOneFunc != nil {
		return m.UpdateOneFunc(ctx, filter, update, opts...)
	}
	return &mongo.UpdateResult{ModifiedCount: 1}, nil
}

func (m *MockMongoCollection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult {
	if m.FindOneFunc != nil {
		return m.FindOneFunc(ctx, filter, opts...)
	}
	return &mongo.SingleResult{}
}

func (m *MockMongoCollection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	if m.FindFunc != nil {
		return m.FindFunc(ctx, filter, opts...)
	}
	return nil, nil
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
	t.Cleanup(func() { testEnv.Stop() })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	tomlConfig := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
		SystemNamespaces:          "kube-*",
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    2,
		UserNamespaces:            userNamespaces,
	}

	reconcilerConfig := config.ReconcilerConfig{
		TomlConfig:    tomlConfig,
		MongoConfig:   storewatcher.MongoDBConfig{},
		TokenConfig:   storewatcher.TokenConfig{},
		MongoPipeline: mongo.Pipeline{},
		StateManager:  statemanager.NewStateManager(client),
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), dryRun)
	require.NoError(t, err)

	go informersInstance.Run(ctx)
	require.Eventually(t, informersInstance.HasSynced, 30*time.Second, 1*time.Second)

	r := reconciler.NewReconciler(reconcilerConfig, dryRun, client, informersInstance)

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

func createHealthEvent(opts healthEventOptions) bson.M {
	healthEvent := &protos.HealthEvent{
		NodeName:  opts.nodeName,
		CheckName: "test-check",
	}

	if opts.drainForce {
		healthEvent.DrainOverrides = &protos.BehaviourOverrides{Force: true}
	}

	return bson.M{
		"fullDocument": bson.M{
			"_id":         opts.nodeName + "-event",
			"healthevent": healthEvent,
			"healtheventstatus": model.HealthEventStatus{
				NodeQuarantined:        &opts.nodeQuarantined,
				UserPodsEvictionStatus: model.OperationStatus{Status: model.StatusInProgress},
			},
			"createdAt": time.Now(),
		},
	}
}

func enqueueHealthEvent(ctx context.Context, t *testing.T, queueMgr queue.EventQueueManager, collection *MockMongoCollection, nodeName string) {
	t.Helper()
	event := createHealthEvent(healthEventOptions{
		nodeName:        nodeName,
		nodeQuarantined: model.Quarantined,
	})
	require.NoError(t, queueMgr.EnqueueEvent(ctx, nodeName, event, collection))
}

func processHealthEvent(ctx context.Context, t *testing.T, r *reconciler.Reconciler, collection *MockMongoCollection, opts healthEventOptions) error {
	t.Helper()
	event := createHealthEvent(opts)
	return r.ProcessEvent(ctx, event, collection, opts.nodeName)
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
