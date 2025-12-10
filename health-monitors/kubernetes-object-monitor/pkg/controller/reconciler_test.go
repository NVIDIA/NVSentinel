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

package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
)

// To run these tests, you need to install and setup envtest:
//   go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//   source <(setup-envtest use -p env)
//
// Then run the tests:
//   go test -v ./pkg/controller/...

func TestReconciler_NodeHealthyToUnhealthy(t *testing.T) {
	setup := setupTest(t)
	nodeName := "test-node-1"

	createNode(t, setup, nodeName, v1.ConditionTrue)

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updateNodeStatus(t, setup, nodeName, v1.ConditionFalse)

	beforeMatches := getCounterVecValue(t, metrics.PolicyMatches, "node-not-ready", nodeName, "Node")

	result, err = setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	afterMatches := getCounterVecValue(t, metrics.PolicyMatches, "node-not-ready", nodeName, "Node")
	assert.Equal(t, beforeMatches+1, afterMatches)

	require.Eventually(t, func() bool {
		return len(setup.publisher.publishedEvents) == 1 &&
			setup.publisher.publishedEvents[0].nodeName == nodeName &&
			!setup.publisher.publishedEvents[0].isHealthy
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_NodeUnhealthyToHealthy(t *testing.T) {
	setup := setupTest(t)
	nodeName := "test-node-2"

	createNode(t, setup, nodeName, v1.ConditionFalse)

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		return len(setup.publisher.publishedEvents) > 0 &&
			!setup.publisher.publishedEvents[0].isHealthy
	}, time.Second, 50*time.Millisecond)

	updateNodeStatus(t, setup, nodeName, v1.ConditionTrue)
	setup.publisher.publishedEvents = []mockPublishedEvent{}

	result, err = setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		return len(setup.publisher.publishedEvents) > 0 &&
			setup.publisher.publishedEvents[0].isHealthy
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_NodeDeleted(t *testing.T) {
	setup := setupTest(t)
	nodeName := "test-node-3"

	node := createNode(t, setup, nodeName, v1.ConditionFalse)

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		return len(setup.publisher.publishedEvents) == 1 && !setup.publisher.publishedEvents[0].isHealthy
	}, time.Second, 50*time.Millisecond)

	setup.publisher.publishedEvents = []mockPublishedEvent{}

	require.NoError(t, setup.k8sClient.Delete(setup.ctx, node))

	result, err = setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) != 1 {
			return false
		}
		event := setup.publisher.publishedEvents[0]
		return event.nodeName == nodeName && event.isHealthy
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_MultipleNodes(t *testing.T) {
	setup := setupTest(t)
	nodeNames := []string{"node-1", "node-2", "node-3"}

	for _, nodeName := range nodeNames {
		createNode(t, setup, nodeName, v1.ConditionFalse)
	}

	for _, nodeName := range nodeNames {
		result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: nodeName},
		})
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)
	}

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) < len(nodeNames) {
			return false
		}
		nodeEvents := make(map[string]bool)
		for _, event := range setup.publisher.publishedEvents {
			nodeEvents[event.nodeName] = true
		}
		for _, nodeName := range nodeNames {
			if !nodeEvents[nodeName] {
				return false
			}
		}
		return true
	}, 2*time.Second, 50*time.Millisecond)
}

func TestReconciler_ResourceNotFound(t *testing.T) {
	setup := setupTest(t)
	nodeName := "non-existent-node"

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Never(t, func() bool {
		return len(setup.publisher.publishedEvents) > 0
	}, 500*time.Millisecond, 50*time.Millisecond)
}

func TestReconciler_DisabledPolicy(t *testing.T) {
	disabledPolicies := []config.Policy{
		{
			Name:    "disabled-policy",
			Enabled: false,
			Resource: config.ResourceSpec{
				Group:   "",
				Version: "v1",
				Kind:    "Node",
			},
			Predicate: config.PredicateSpec{
				Expression: `status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0`,
			},
			HealthEvent: config.HealthEventSpec{
				ComponentClass: "Node",
				Message:        "Disabled policy test",
			},
		},
	}

	setup := setupTestWithPolicies(t, disabledPolicies)
	nodeName := "test-node-5"

	createNode(t, setup, nodeName, v1.ConditionFalse)

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Never(t, func() bool {
		return len(setup.publisher.publishedEvents) > 0
	}, 500*time.Millisecond, 50*time.Millisecond)
}

func TestReconciler_ErrorCodePropagation(t *testing.T) {
	setup := setupTest(t)
	nodeName := "test-node-error-code"

	createNode(t, setup, nodeName, v1.ConditionFalse)

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) != 1 {
			return false
		}
		event := setup.publisher.publishedEvents[0]
		return len(event.policy.HealthEvent.ErrorCode) == 2 &&
			event.policy.HealthEvent.ErrorCode[0] == "NODE_NOT_READY" &&
			event.policy.HealthEvent.ErrorCode[1] == "CONDITION_FAILED"
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_CustomResource(t *testing.T) {
	setup := setupTestWithCRD(t, []config.Policy{defaultGPUJobFailedPolicy()}, gpuJobCRD())
	nodeName := "gpu-test-node"
	jobName := "test-gpu-job"
	namespace := "default"

	createNode(t, setup, nodeName, v1.ConditionTrue)

	gpuJob := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "batch.nvidia.com/v1alpha1",
			"kind":       "GPUJob",
			"metadata": map[string]any{
				"name":      jobName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"nodeName": nodeName,
			},
		},
	}

	require.NoError(t, setup.k8sClient.Create(setup.ctx, gpuJob))

	require.Eventually(t, func() bool {
		err := setup.k8sClient.Get(setup.ctx, types.NamespacedName{
			Name:      jobName,
			Namespace: namespace,
		}, gpuJob)
		return err == nil
	}, time.Second, 50*time.Millisecond)

	gpuJob.Object["status"] = map[string]any{
		"state": "Failed",
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, gpuJob))

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: jobName, Namespace: namespace},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) != 1 {
			return false
		}
		event := setup.publisher.publishedEvents[0]
		return event.nodeName == nodeName &&
			!event.isHealthy &&
			event.policy.Name == "gpu-job-failed"
	}, time.Second, 50*time.Millisecond)

	setup.publisher.publishedEvents = []mockPublishedEvent{}

	require.NoError(t, setup.k8sClient.Delete(setup.ctx, gpuJob))

	result, err = setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: jobName, Namespace: namespace},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) != 1 {
			return false
		}
		event := setup.publisher.publishedEvents[0]
		return event.nodeName == nodeName &&
			event.isHealthy &&
			event.policy.Name == "gpu-job-failed"
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_CustomResourceColdStart(t *testing.T) {
	crPolicy := defaultGPUJobFailedPolicy()
	setup := setupTestWithCRD(t, []config.Policy{crPolicy}, gpuJobCRD())
	nodeName := "gpu-test-node-cold"
	jobName := "test-gpu-job-cold"
	namespace := "default"

	createNode(t, setup, nodeName, v1.ConditionTrue)

	gpuJob := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "batch.nvidia.com/v1alpha1",
			"kind":       "GPUJob",
			"metadata": map[string]any{
				"name":      jobName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"nodeName": nodeName,
			},
		},
	}

	require.NoError(t, setup.k8sClient.Create(setup.ctx, gpuJob))

	require.Eventually(t, func() bool {
		err := setup.k8sClient.Get(setup.ctx, types.NamespacedName{
			Name:      jobName,
			Namespace: namespace,
		}, gpuJob)
		return err == nil
	}, time.Second, 50*time.Millisecond)

	gpuJob.Object["status"] = map[string]any{
		"state": "Failed",
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, gpuJob))

	result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: jobName, Namespace: namespace},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(setup.publisher.publishedEvents) != 1 {
			return false
		}
		event := setup.publisher.publishedEvents[0]
		return event.nodeName == nodeName &&
			!event.isHealthy &&
			event.policy.Name == "gpu-job-failed"
	}, time.Second, 50*time.Millisecond)

	coldStartSetup := restartReconcilerWithCRD(t, setup, []config.Policy{crPolicy}, gpuJobCRD())

	require.NoError(t, coldStartSetup.k8sClient.Delete(coldStartSetup.ctx, gpuJob))

	result, err = coldStartSetup.reconciler.Reconcile(coldStartSetup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: jobName, Namespace: namespace},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	require.Eventually(t, func() bool {
		if len(coldStartSetup.publisher.publishedEvents) != 1 {
			return false
		}
		event := coldStartSetup.publisher.publishedEvents[0]
		return event.nodeName == nodeName &&
			event.isHealthy &&
			event.policy.Name == "gpu-job-failed"
	}, time.Second, 50*time.Millisecond)
}

func TestReconciler_ColdStart(t *testing.T) {
	tests := []struct {
		name              string
		postRestartAction func(*testing.T, *testSetup, string, *v1.Node)
		expectEvent       bool
		expectHealthy     bool
	}{
		{
			name: "unhealthy resource",
			postRestartAction: func(t *testing.T, s *testSetup, nodeName string, _ *v1.Node) {
				updateNodeStatus(t, s, nodeName, v1.ConditionFalse)
			},
			expectEvent: false,
		},
		{
			name: "healthy resource",
			postRestartAction: func(t *testing.T, s *testSetup, nodeName string, _ *v1.Node) {
				updateNodeStatus(t, s, nodeName, v1.ConditionTrue)
			},
			expectEvent:   true,
			expectHealthy: true,
		},
		{
			name: "deleted resource",
			postRestartAction: func(t *testing.T, s *testSetup, _ string, node *v1.Node) {
				require.NoError(t, s.k8sClient.Delete(s.ctx, node))
			},
			expectEvent:   true,
			expectHealthy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := setupTest(t)
			nodeName := "cold-start-" + strings.ReplaceAll(tt.name, " ", "-")

			node := createNode(t, setup, nodeName, v1.ConditionFalse)

			result, err := setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			})
			assert.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, result)

			require.Eventually(t, func() bool {
				return len(setup.publisher.publishedEvents) == 1
			}, time.Second, 50*time.Millisecond)

			coldStartSetup := restartReconciler(t, setup)

			tt.postRestartAction(t, coldStartSetup, nodeName, node)

			result, err = coldStartSetup.reconciler.Reconcile(coldStartSetup.ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: nodeName},
			})
			assert.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, result)

			if tt.expectEvent {
				require.Eventually(t, func() bool {
					if len(coldStartSetup.publisher.publishedEvents) != 1 {
						return false
					}
					return coldStartSetup.publisher.publishedEvents[0].isHealthy == tt.expectHealthy
				}, time.Second, 50*time.Millisecond)
			} else {
				require.Never(t, func() bool {
					return len(coldStartSetup.publisher.publishedEvents) > 0
				}, 500*time.Millisecond, 50*time.Millisecond)
			}
		})
	}
}

type testSetup struct {
	ctx        context.Context
	k8sClient  client.Client
	reconciler *controller.ResourceReconciler
	publisher  *mockPublisher
	evaluator  *policy.Evaluator
	testEnv    *envtest.Environment
}

func defaultNodeNotReadyPolicy() config.Policy {
	return config.Policy{
		Name:    "node-not-ready",
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:   "",
			Version: "v1",
			Kind:    "Node",
		},
		Predicate: config.PredicateSpec{
			Expression: `resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0`,
		},
		HealthEvent: config.HealthEventSpec{
			ComponentClass:    "Node",
			IsFatal:           true,
			Message:           "Node is not ready",
			RecommendedAction: "CONTACT_SUPPORT",
			ErrorCode:         []string{"NODE_NOT_READY", "CONDITION_FAILED"},
		},
	}
}

func defaultGPUJobFailedPolicy() config.Policy {
	return config.Policy{
		Name:    "gpu-job-failed",
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:   "batch.nvidia.com",
			Version: "v1alpha1",
			Kind:    "GPUJob",
		},
		Predicate: config.PredicateSpec{
			Expression: `has(resource.status.state) && resource.status.state == "Failed"`,
		},
		NodeAssociation: &config.AssociationSpec{
			Expression: `resource.spec.nodeName`,
		},
		HealthEvent: config.HealthEventSpec{
			ComponentClass:    "GPU",
			IsFatal:           false,
			Message:           "GPU job failed",
			RecommendedAction: "CONTACT_SUPPORT",
			ErrorCode:         []string{"GPU_JOB_FAILED"},
		},
	}
}

func setupTest(t *testing.T) *testSetup {
	t.Helper()
	return setupTestWithPolicies(t, []config.Policy{defaultNodeNotReadyPolicy()})
}

func setupTestWithPolicies(t *testing.T, policies []config.Policy) *testSetup {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, testEnv.Stop())
	})

	k8sClient, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	mockPub := &mockPublisher{
		publishedEvents: []mockPublishedEvent{},
	}

	celEnvironment, err := celenv.NewEnvironment(k8sClient)
	require.NoError(t, err)

	evaluator, err := policy.NewEvaluator(celEnvironment, policies)
	require.NoError(t, err)

	gvk := schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Node",
	}

	annotationMgr := annotations.NewManager(k8sClient)

	reconciler := controller.NewResourceReconciler(
		k8sClient,
		evaluator,
		mockPub,
		annotationMgr,
		policies,
		gvk,
	)

	return &testSetup{
		ctx:        ctx,
		k8sClient:  k8sClient,
		reconciler: reconciler,
		publisher:  mockPub,
		evaluator:  evaluator,
		testEnv:    testEnv,
	}
}

func setupTestWithCRD(t *testing.T, policies []config.Policy, crd *apiextensionsv1.CustomResourceDefinition) *testSetup {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	testEnv := &envtest.Environment{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{crd},
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, testEnv.Stop())
	})

	k8sClient, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	mockPub := &mockPublisher{
		publishedEvents: []mockPublishedEvent{},
	}

	celEnvironment, err := celenv.NewEnvironment(k8sClient)
	require.NoError(t, err)

	evaluator, err := policy.NewEvaluator(celEnvironment, policies)
	require.NoError(t, err)

	gvk := schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: crd.Spec.Versions[0].Name,
		Kind:    crd.Spec.Names.Kind,
	}

	annotationMgr := annotations.NewManager(k8sClient)

	reconciler := controller.NewResourceReconciler(
		k8sClient,
		evaluator,
		mockPub,
		annotationMgr,
		policies,
		gvk,
	)

	return &testSetup{
		ctx:        ctx,
		k8sClient:  k8sClient,
		reconciler: reconciler,
		publisher:  mockPub,
		evaluator:  evaluator,
		testEnv:    testEnv,
	}
}

type mockPublishedEvent struct {
	ctx       context.Context
	policy    *config.Policy
	nodeName  string
	isHealthy bool
}

type mockPublisher struct {
	publishedEvents []mockPublishedEvent
}

func (m *mockPublisher) PublishHealthEvent(
	ctx context.Context,
	policy *config.Policy,
	nodeName string,
	isHealthy bool,
) error {
	m.publishedEvents = append(m.publishedEvents, mockPublishedEvent{
		ctx:       ctx,
		policy:    policy,
		nodeName:  nodeName,
		isHealthy: isHealthy,
	})
	return nil
}

func createNode(t *testing.T, setup *testSetup, name string, readyStatus v1.ConditionStatus) *v1.Node {
	t.Helper()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{Type: v1.NodeReady, Status: readyStatus},
			},
		},
	}
	require.NoError(t, setup.k8sClient.Create(setup.ctx, node))

	require.Eventually(t, func() bool {
		err := setup.k8sClient.Get(setup.ctx, types.NamespacedName{Name: name}, node)
		return err == nil
	}, time.Second, 50*time.Millisecond)

	return node
}

func updateNodeStatus(t *testing.T, setup *testSetup, name string, readyStatus v1.ConditionStatus) {
	t.Helper()

	node := &v1.Node{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Name: name}, node))

	node.Status.Conditions = []v1.NodeCondition{
		{Type: v1.NodeReady, Status: readyStatus},
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, node))

	require.Eventually(t, func() bool {
		updatedNode := &v1.Node{}
		if err := setup.k8sClient.Get(setup.ctx, types.NamespacedName{Name: name}, updatedNode); err != nil {
			return false
		}
		for _, cond := range updatedNode.Status.Conditions {
			if cond.Type == v1.NodeReady && cond.Status == readyStatus {
				return true
			}
		}
		return false
	}, time.Second, 50*time.Millisecond)
}

func restartReconciler(t *testing.T, setup *testSetup) *testSetup {
	t.Helper()

	gvk := schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Node",
	}

	mockPub := &mockPublisher{
		publishedEvents: []mockPublishedEvent{},
	}

	policies := []config.Policy{defaultNodeNotReadyPolicy()}

	annotationMgr := annotations.NewManager(setup.k8sClient)

	reconciler := controller.NewResourceReconciler(
		setup.k8sClient,
		setup.evaluator,
		mockPub,
		annotationMgr,
		policies,
		gvk,
	)

	if err := reconciler.LoadState(setup.ctx); err != nil {
		t.Fatalf("Failed to load state after restart: %v", err)
	}

	return &testSetup{
		ctx:        setup.ctx,
		k8sClient:  setup.k8sClient,
		reconciler: reconciler,
		publisher:  mockPub,
		evaluator:  setup.evaluator,
		testEnv:    setup.testEnv,
	}
}

func restartReconcilerWithCRD(t *testing.T, setup *testSetup, policies []config.Policy, crd *apiextensionsv1.CustomResourceDefinition) *testSetup {
	t.Helper()

	gvk := schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: crd.Spec.Versions[0].Name,
		Kind:    crd.Spec.Names.Kind,
	}

	mockPub := &mockPublisher{
		publishedEvents: []mockPublishedEvent{},
	}

	annotationMgr := annotations.NewManager(setup.k8sClient)

	reconciler := controller.NewResourceReconciler(
		setup.k8sClient,
		setup.evaluator,
		mockPub,
		annotationMgr,
		policies,
		gvk,
	)

	if err := reconciler.LoadState(setup.ctx); err != nil {
		t.Fatalf("Failed to load state after restart: %v", err)
	}

	return &testSetup{
		ctx:        setup.ctx,
		k8sClient:  setup.k8sClient,
		reconciler: reconciler,
		publisher:  mockPub,
		evaluator:  setup.evaluator,
		testEnv:    setup.testEnv,
	}
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

func gpuJobCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpujobs.batch.nvidia.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "batch.nvidia.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "gpujobs",
				Singular: "gpujob",
				Kind:     "GPUJob",
				ListKind: "GPUJobList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"nodeName": {Type: "string"},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"state": {Type: "string"},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				},
			},
		},
	}
}
