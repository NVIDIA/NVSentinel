// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/reconciler"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platform_connectors "github.com/nvidia/nvsentinel/platform-connectors/pkg/protos"
	"github.com/nvidia/nvsentinel/statemanager"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

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

// go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
// source <(setup-envtest use -p env)
func TestReconciler_ProcessEvent(t *testing.T) {
	tests := []struct {
		name                 string
		nodeName             string
		namespaces           []string
		pods                 []*v1.Pod
		nodeQuarantined      storeconnector.Status
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
			nodeQuarantined: storeconnector.Quarantined,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "immediate-test"},
					Spec:       v1.PodSpec{NodeName: "test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectError:       false,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
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
			nodeQuarantined: storeconnector.Quarantined,
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
			expectError:       false,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
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
			nodeQuarantined: storeconnector.UnQuarantined,
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
					klog.Infof("Node %s labels: %v", nodeName, node.Labels)
					_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
					return !exists
				}, 30*time.Second, 1*time.Second, "draining label should be removed")
			},
		},
		{
			name:            "AllowCompletion mode waits for pods to complete",
			nodeName:        "completion-node",
			namespaces:      []string{"completion-test"},
			nodeQuarantined: storeconnector.Quarantined,
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
			nodeQuarantined: storeconnector.Quarantined,
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
			nodeQuarantined: storeconnector.AlreadyQuarantined,
			pods:            []*v1.Pod{},
			mongoFindOneResponse: &bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusSucceeded),
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
			nodeQuarantined: storeconnector.Quarantined,
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
			expectError:       false,
			expectedNodeLabel: ptr.To(string(statemanager.DrainSucceededLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
				// DaemonSet pod should remain
				require.Eventually(t, func() bool {
					pods, _ := client.CoreV1().Pods("immediate-test").List(ctx, metav1.ListOptions{})
					t.Logf("Pods remaining after eviction: %d", len(pods.Items))
					return len(pods.Items) == 1
				}, 30*time.Second, 1*time.Second, "only daemonset pod should remain")
			},
		},
		{
			name:            "Immediate eviction completes in single pass",
			nodeName:        "single-pass-node",
			namespaces:      []string{"immediate-test"},
			nodeQuarantined: storeconnector.Quarantined,
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
			expectError:       false,
			expectedNodeLabel: ptr.To(string(statemanager.DrainingLabelValue)),
			validateFunc: func(t *testing.T, client kubernetes.Interface, ctx context.Context, nodeName string, err error) {
				assert.NoError(t, err)
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
			ctx := context.Background()
			testEnv := envtest.Environment{}
			timeout, poll := 60*time.Second, 1*time.Second

			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer testEnv.Stop()

			client, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			nodeLabels := tt.existingNodeLabels
			if nodeLabels == nil {
				nodeLabels = map[string]string{"test-label": "test-value"}
			}
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   tt.nodeName,
					Labels: nodeLabels,
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{Type: v1.NodeReady, Status: v1.ConditionTrue},
					},
				},
			}
			_, err = client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create test node")

			for _, ns := range tt.namespaces {
				namespace := &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: ns},
				}
				_, err = client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create namespace %s", ns)
			}

			for _, pod := range tt.pods {
				po, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create pod %s", pod.Name)

				po.Status = pod.Status
				_, err = client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, po, metav1.UpdateOptions{})
				require.NoError(t, err, "failed to update pod status")
			}

			informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), false)
			require.NoError(t, err)

			go func() {
				if err := informersInstance.Run(ctx); err != nil {
					t.Errorf("Failed to run informers: %v", err)
				}
			}()

			require.Eventually(t, func() bool {
				return informersInstance.HasSynced()
			}, 30*time.Second, 1*time.Second, "informers should be synced")

			tomlConfig := config.TomlConfig{
				EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
				SystemNamespaces:          "kube-*",
				DeleteAfterTimeoutMinutes: 5,
				NotReadyTimeoutMinutes:    2,
				UserNamespaces: []config.UserNamespace{
					{Name: "immediate-*", Mode: config.ModeImmediateEvict},
					{Name: "completion-*", Mode: config.ModeAllowCompletion},
					{Name: "timeout-*", Mode: config.ModeDeleteAfterTimeout},
				},
			}

			reconcilerConfig := config.ReconcilerConfig{
				TomlConfig:    tomlConfig,
				MongoConfig:   storewatcher.MongoDBConfig{},
				TokenConfig:   storewatcher.TokenConfig{},
				MongoPipeline: mongo.Pipeline{},
				StateManager:  statemanager.NewStateManager(client),
			}

			r := reconciler.NewReconciler(reconcilerConfig, false, client, informersInstance)

			mockCollection := &MockMongoCollection{}
			if tt.mongoFindOneResponse != nil {
				mockCollection.FindOneFunc = func(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult {
					return mongo.NewSingleResultFromDocument(*tt.mongoFindOneResponse, nil, nil)
				}
			}

			healthEvent := &storeconnector.HealthEventWithStatus{
				HealthEvent: &platform_connectors.HealthEvent{
					NodeName:  tt.nodeName,
					CheckName: "test-check",
				},
				HealthEventStatus: storeconnector.HealthEventStatus{
					NodeQuarantined: &tt.nodeQuarantined,
					UserPodsEvictionStatus: storeconnector.OperationStatus{
						Status: storeconnector.StatusInProgress,
					},
				},
				CreatedAt: time.Now(),
			}

			if tt.drainForce {
				healthEvent.HealthEvent.DrainOverrides = &platform_connectors.BehaviourOverrides{
					Force: true,
				}
			}

			event := bson.M{
				"fullDocument": bson.M{
					"_id":               "test-event-id",
					"healthevent":       healthEvent.HealthEvent,
					"healtheventstatus": healthEvent.HealthEventStatus,
					"createdAt":         healthEvent.CreatedAt,
				},
			}

			err = r.ProcessEvent(ctx, event, mockCollection, tt.nodeName)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectedNodeLabel != nil {
				require.Eventually(t, func() bool {
					node, err := client.CoreV1().Nodes().Get(ctx, tt.nodeName, metav1.GetOptions{})
					require.NoError(t, err)

					label, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
					if !exists && *tt.expectedNodeLabel == "" {
						return true
					}
					return exists && label == *tt.expectedNodeLabel
				}, timeout, poll, "node label should be %s", *tt.expectedNodeLabel)
			}

			if tt.validateFunc != nil {
				tt.validateFunc(t, client, ctx, tt.nodeName, err)
			}
		})
	}
}

func TestReconciler_EvictionModes(t *testing.T) {
	tests := []struct {
		name             string
		namespaceMode    config.EvictMode
		namespaceName    string
		nodeName         string
		pods             []*v1.Pod
		expectedBehavior string
		expectError      bool
		skipIfNoEnvtest  bool
	}{
		{
			name:          "Immediate mode triggers eviction",
			namespaceMode: config.ModeImmediateEvict,
			namespaceName: "immediate-evict-ns",
			nodeName:      "evict-test-node",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "evict-pod", Namespace: "immediate-evict-ns"},
					Spec:       v1.PodSpec{NodeName: "evict-test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectedBehavior: "evict",
			expectError:      false,
		},
		{
			name:          "AllowCompletion mode waits for pods",
			namespaceMode: config.ModeAllowCompletion,
			namespaceName: "completion-wait-ns",
			nodeName:      "wait-test-node",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "wait-pod", Namespace: "completion-wait-ns"},
					Spec:       v1.PodSpec{NodeName: "wait-test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectedBehavior: "wait",
			expectError:      true,
		},
		{
			name:          "DeleteAfterTimeout mode starts timeout",
			namespaceMode: config.ModeDeleteAfterTimeout,
			namespaceName: "timeout-delete-ns",
			nodeName:      "timeout-test-node",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "timeout-pod", Namespace: "timeout-delete-ns"},
					Spec:       v1.PodSpec{NodeName: "timeout-test-node", Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
					Status:     v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			expectedBehavior: "timeout",
			expectError:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			testEnv := envtest.Environment{}

			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer testEnv.Stop()

			client, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: tt.nodeName, Labels: map[string]string{"test-label": "test-value"}},
				Status:     v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}},
			}
			_, err = client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err)

			ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tt.namespaceName}}
			_, err = client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
			require.NoError(t, err)

			for _, pod := range tt.pods {
				po, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err)
				po.Status = pod.Status
				_, err = client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, po, metav1.UpdateOptions{})
				require.NoError(t, err)
			}

			tomlConfig := config.TomlConfig{
				EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
				SystemNamespaces:          "kube-*",
				DeleteAfterTimeoutMinutes: 5,
				NotReadyTimeoutMinutes:    2,
				UserNamespaces: []config.UserNamespace{
					{Name: tt.namespaceName, Mode: tt.namespaceMode},
				},
			}

			reconcilerConfig := config.ReconcilerConfig{
				TomlConfig:    tomlConfig,
				MongoConfig:   storewatcher.MongoDBConfig{},
				TokenConfig:   storewatcher.TokenConfig{},
				MongoPipeline: mongo.Pipeline{},
				StateManager:  statemanager.NewStateManager(client),
			}

			informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), false)
			require.NoError(t, err)

			go func() {
				_ = informersInstance.Run(ctx)
			}()

			require.Eventually(t, func() bool {
				return informersInstance.HasSynced()
			}, 30*time.Second, 1*time.Second, "informers should be synced")

			r := reconciler.NewReconciler(reconcilerConfig, false, client, informersInstance)
			mockCollection := &MockMongoCollection{}

			healthEvent := &storeconnector.HealthEventWithStatus{
				HealthEvent: &platform_connectors.HealthEvent{NodeName: tt.nodeName, CheckName: "test"},
				HealthEventStatus: storeconnector.HealthEventStatus{
					NodeQuarantined:        ptr.To(storeconnector.Quarantined),
					UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusInProgress},
				},
				CreatedAt: time.Now(),
			}

			event := bson.M{
				"fullDocument": bson.M{
					"_id":               "test-id",
					"healthevent":       healthEvent.HealthEvent,
					"healtheventstatus": healthEvent.HealthEventStatus,
					"createdAt":         healthEvent.CreatedAt,
				},
			}

			err = r.ProcessEvent(ctx, event, mockCollection, tt.nodeName)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			switch tt.expectedBehavior {
			case "evict":
				require.Eventually(t, func() bool {
					pods, listErr := client.CoreV1().Pods(tt.namespaceName).List(ctx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("spec.nodeName=%s", tt.nodeName),
					})
					if listErr != nil {
						return false
					}
					// Count pods that don't have DeletionTimestamp (i.e., not being deleted)
					activePodsCount := 0
					for _, pod := range pods.Items {
						if pod.DeletionTimestamp == nil {
							activePodsCount++
						}
					}
					return activePodsCount == 0
				}, 30*time.Second, 1*time.Second, "All pods should be evicted in immediate mode")
			case "wait":
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "waiting for pods to complete", "Error should indicate waiting for pod completion")

				nodeEvents, eventErr := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{
					FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", tt.nodeName),
				})
				require.NoError(t, eventErr)
				require.Greater(t, len(nodeEvents.Items), 0, "Node event should be created")
				assert.Equal(t, "AwaitingPodCompletion", nodeEvents.Items[0].Reason)
			case "timeout":
				assert.Error(t, err)
				// Pods should still exist as timeout hasn't elapsed yet
				pods, listErr := client.CoreV1().Pods(tt.namespaceName).List(ctx, metav1.ListOptions{
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", tt.nodeName),
				})
				require.NoError(t, listErr)
				assert.Greater(t, len(pods.Items), 0, "Pods should still exist during timeout period")
			}
		})
	}
}

func TestReconciler_DryRunMode(t *testing.T) {
	ctx := context.Background()
	testEnv := envtest.Environment{}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")
	defer testEnv.Stop()

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create kubernetes client")

	nodeName := "dry-run-node"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: map[string]string{"test-label": "test-value"}},
		Status:     v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}},
	}
	_, err = client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "immediate-test"}}
	_, err = client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dry-pod", Namespace: "immediate-test"},
		Spec:       v1.PodSpec{NodeName: nodeName, Containers: []v1.Container{{Name: "c", Image: "nginx"}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}
	po, err := client.CoreV1().Pods("immediate-test").Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	po.Status = pod.Status
	_, err = client.CoreV1().Pods("immediate-test").UpdateStatus(ctx, po, metav1.UpdateOptions{})
	require.NoError(t, err)

	tomlConfig := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 30 * time.Second},
		SystemNamespaces:          "kube-*",
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    2,
		UserNamespaces: []config.UserNamespace{
			{Name: "immediate-*", Mode: config.ModeImmediateEvict},
		},
	}

	reconcilerConfig := config.ReconcilerConfig{
		TomlConfig:    tomlConfig,
		MongoConfig:   storewatcher.MongoDBConfig{},
		TokenConfig:   storewatcher.TokenConfig{},
		MongoPipeline: mongo.Pipeline{},
		StateManager:  statemanager.NewStateManager(client),
	}

	informersInstance, err := informers.NewInformers(client, 1*time.Minute, ptr.To(2), true)
	require.NoError(t, err)

	go func() {
		_ = informersInstance.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		return informersInstance.HasSynced()
	}, 30*time.Second, 1*time.Second, "informers should be synced")

	r := reconciler.NewReconciler(reconcilerConfig, true, client, informersInstance)
	mockCollection := &MockMongoCollection{}

	healthEvent := &storeconnector.HealthEventWithStatus{
		HealthEvent: &platform_connectors.HealthEvent{NodeName: nodeName, CheckName: "test"},
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.Quarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{Status: storeconnector.StatusInProgress},
		},
		CreatedAt: time.Now(),
	}

	event := bson.M{
		"fullDocument": bson.M{
			"_id":               "test-id",
			"healthevent":       healthEvent.HealthEvent,
			"healtheventstatus": healthEvent.HealthEventStatus,
			"createdAt":         healthEvent.CreatedAt,
		},
	}

	err = r.ProcessEvent(ctx, event, mockCollection, nodeName)
	assert.NoError(t, err)

	require.Eventually(t, func() bool {
		pods, err := client.CoreV1().Pods("immediate-test").List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		return len(pods.Items) == 1
	}, 30*time.Second, 1*time.Second, "pod should still exist in dry-run mode")
}
