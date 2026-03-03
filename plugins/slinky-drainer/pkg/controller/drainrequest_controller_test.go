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

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	drainv1alpha1 "github.com/nvidia/nvsentinel/plugins/slinky-drainer/api/v1alpha1"
)

const (
	testSlinkyNamespace = "slinky-test"
	testTimeout         = 30 * time.Second
	testPollInterval    = 500 * time.Millisecond
)

type testEnvContext struct {
	client client.Client
	ctx    context.Context
}


func TestReconcile_FullDrainCycle(t *testing.T) {
	tc := setupTestEnv(t, "drain-full-cycle")

	node := createNode(t, tc, "test-node-drain-cycle", nil, map[string]string{
		nvsentinelStateLabelKey: "draining",
	})
	pod := createSlinkyPod(t, tc, node.Name)
	markPodReady(t, tc, pod.Name, pod.Namespace)
	// Create a failed pod on the same node — should not block the drain.
	createFailedPod(t, tc, node.Name)
	createDrainRequest(t, tc, "drain-full-cycle", drainv1alpha1.DrainRequestSpec{
		NodeName:         node.Name,
		ErrorCode:        []string{"79"},
		EntitiesImpacted: []drainv1alpha1.EntityImpacted{{Type: "GPU", Value: "0"}},
		Reason:           "GPU has fallen off the bus",
	})

	assertNodeAnnotation(t, tc, node.Name, "[J] [NVSentinel] 79 GPU:0 - GPU has fallen off the bus")

	// Simulate DRAINING state: Slurm accepted drain but jobs still running.
	markPodDraining(t, tc, pod.Name, pod.Namespace)

	// Pod must NOT be deleted while in DRAINING state (busy).
	assertPodNotDeleted(t, tc, pod.Name, pod.Namespace, 3*time.Second)

	// Simulate transition to DRAINED state: jobs finished, node is idle.
	markPodDrained(t, tc, pod.Name, pod.Namespace)

	waitForDrainComplete(t, tc, "drain-full-cycle", "default")
	waitForPodDeletion(t, tc, pod.Name, pod.Namespace)

	// Annotation stays while node is still in draining state.
	assertNodeAnnotation(t, tc, node.Name, "[J] [NVSentinel] 79 GPU:0 - GPU has fallen off the bus")

	// Simulate remediation success by removing the label (as statemanager would).
	removeNodeLabel(t, tc, node.Name, nvsentinelStateLabelKey)

	waitForAnnotationRemoved(t, tc, node.Name)
}

func TestReconcile_PreExistingAnnotationPreserved(t *testing.T) {
	tc := setupTestEnv(t, "drain-preexisting")

	node := createNode(t, tc, "test-node-preexisting", map[string]string{
		annotationKey: "Manual drain by operator",
	}, nil)
	createDrainRequest(t, tc, "drain-preexisting", drainv1alpha1.DrainRequestSpec{
		NodeName:  node.Name,
		ErrorCode: []string{"79"},
		Reason:    "GPU has fallen off the bus",
	})

	waitForDrainComplete(t, tc, "drain-preexisting", "default")
	assertNodeAnnotation(t, tc, node.Name, "Manual drain by operator")
}

func TestReconcile_DrainingPodNotDeleted(t *testing.T) {
	tc := setupTestEnv(t, "drain-still-draining")

	node := createNode(t, tc, "test-node-draining", nil, map[string]string{
		nvsentinelStateLabelKey: "draining",
	})
	pod := createSlinkyPod(t, tc, node.Name)
	markPodReady(t, tc, pod.Name, pod.Namespace)
	createDrainRequest(t, tc, "drain-still-draining", drainv1alpha1.DrainRequestSpec{
		NodeName:  node.Name,
		ErrorCode: []string{"79"},
		Reason:    "GPU has fallen off the bus",
	})

	assertNodeAnnotation(t, tc, node.Name, "[J] [NVSentinel] 79 - GPU has fallen off the bus")

	// Set pod to DRAINING: Drain flag set but node is still busy (Allocated).
	markPodDraining(t, tc, pod.Name, pod.Namespace)

	// Verify pod is NOT deleted and DrainRequest is NOT completed while draining.
	assertPodNotDeleted(t, tc, pod.Name, pod.Namespace, 5*time.Second)
	assertDrainNotComplete(t, tc, "drain-still-draining", "default")
}

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

func setupTestEnv(t *testing.T, controllerName string) *testEnvContext {
	t.Helper()

	scheme := clientgoscheme.Scheme
	require.NoError(t, drainv1alpha1.AddToScheme(scheme))

	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := te.Start()
	require.NoError(t, err, "failed to start envtest")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err, "failed to create manager")

	reconciler := &DrainRequestReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		PodCheckInterval: 1 * time.Second,
		DrainTimeout:     5 * time.Minute,
		SlinkyNamespace:  testSlinkyNamespace,
	}

	require.NoError(t, mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		"spec.nodeName",
		func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		},
	), "failed to set up field indexer")

	require.NoError(t,
		ctrl.NewControllerManagedBy(mgr).
			Named(controllerName).
			For(&drainv1alpha1.DrainRequest{}).
			Watches(&corev1.Node{},
				handler.EnqueueRequestsFromMapFunc(reconciler.nodeToMatchingDrainRequests),
			).
			Complete(reconciler),
		"failed to setup controller",
	)

	ctx, cancel := context.WithCancel(context.Background())
	mgrDone := make(chan struct{})

	go func() {
		defer close(mgrDone)

		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	k := mgr.GetClient()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testSlinkyNamespace}}
	_ = k.Create(ctx, ns)

	t.Cleanup(func() {
		cancel()
		<-mgrDone

		if err := te.Stop(); err != nil {
			t.Logf("failed to stop envtest: %v", err)
		}
	})

	return &testEnvContext{client: k, ctx: ctx}
}

// ---------------------------------------------------------------------------
// Resource creation helpers
// ---------------------------------------------------------------------------

func createNode(t *testing.T, tc *testEnvContext, name string, annotations, labels map[string]string) *corev1.Node {
	t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      labels,
		},
	}
	require.NoError(t, tc.client.Create(tc.ctx, node))

	return node
}

func createSlinkyPod(t *testing.T, tc *testEnvContext, nodeName string) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "slinky-pod-" + nodeName,
			Namespace: testSlinkyNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "slurmd", Image: "nvcr.io/nvidia/slinky:latest"}},
		},
	}
	require.NoError(t, tc.client.Create(tc.ctx, pod))

	return pod
}

func createFailedPod(t *testing.T, tc *testEnvContext, nodeName string) {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failed-pod-" + nodeName,
			Namespace: testSlinkyNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "slurmd", Image: "nvcr.io/nvidia/slinky:latest"}},
		},
	}
	require.NoError(t, tc.client.Create(tc.ctx, pod))

	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, tc.client.Status().Update(tc.ctx, pod))
}

func markPodReady(t *testing.T, tc *testEnvContext, podName, podNamespace string) {
	t.Helper()

	var pod corev1.Pod

	require.Eventually(t, func() bool {
		return tc.client.Get(tc.ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, &pod) == nil
	}, testTimeout, testPollInterval, "Pod %s/%s should exist", podNamespace, podName)

	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	require.NoError(t, tc.client.Status().Update(tc.ctx, &pod))
}

func markPodDraining(t *testing.T, tc *testEnvContext, podName, podNamespace string) {
	t.Helper()

	pod := &corev1.Pod{}
	require.NoError(t, tc.client.Get(tc.ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, pod))

	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		{Type: slurmNodeStateDrainConditionType, Status: corev1.ConditionTrue},
		{Type: slurmNodeStateAllocatedConditionType, Status: corev1.ConditionTrue},
	}
	require.NoError(t, tc.client.Status().Update(tc.ctx, pod))
}

func markPodDrained(t *testing.T, tc *testEnvContext, podName, podNamespace string) {
	t.Helper()

	pod := &corev1.Pod{}
	require.NoError(t, tc.client.Get(tc.ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, pod))

	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		{Type: slurmNodeStateDrainConditionType, Status: corev1.ConditionTrue},
		{Type: "SlurmNodeStateIdle", Status: corev1.ConditionTrue},
	}
	require.NoError(t, tc.client.Status().Update(tc.ctx, pod))
}

func createDrainRequest(t *testing.T, tc *testEnvContext, name string, spec drainv1alpha1.DrainRequestSpec) {
	t.Helper()

	dr := &drainv1alpha1.DrainRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
	require.NoError(t, tc.client.Create(tc.ctx, dr))
}

func removeNodeLabel(t *testing.T, tc *testEnvContext, nodeName, labelKey string) {
	t.Helper()

	node := &corev1.Node{}
	require.NoError(t, tc.client.Get(tc.ctx, types.NamespacedName{Name: nodeName}, node))

	delete(node.Labels, labelKey)
	require.NoError(t, tc.client.Update(tc.ctx, node))
}

// ---------------------------------------------------------------------------
// Assertion / wait helpers
// ---------------------------------------------------------------------------

func waitForDrainComplete(t *testing.T, tc *testEnvContext, drName, drNamespace string) {
	t.Helper()

	require.Eventually(t, func() bool {
		dr := &drainv1alpha1.DrainRequest{}
		if err := tc.client.Get(tc.ctx, types.NamespacedName{Name: drName, Namespace: drNamespace}, dr); err != nil {
			return false
		}

		for _, c := range dr.Status.Conditions {
			if c.Type == drainCompleteConditionType && c.Status == metav1.ConditionTrue {
				return true
			}
		}

		return false
	}, testTimeout, testPollInterval, "DrainRequest %s/%s should have DrainComplete=True", drNamespace, drName)
}

func waitForPodDeletion(t *testing.T, tc *testEnvContext, podName, podNamespace string) {
	t.Helper()

	require.Eventually(t, func() bool {
		p := &corev1.Pod{}
		if err := tc.client.Get(tc.ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, p); err != nil {
			return apierrors.IsNotFound(err)
		}

		return p.DeletionTimestamp != nil
	}, testTimeout, testPollInterval, "Pod %s/%s should be marked for deletion", podNamespace, podName)
}

func waitForAnnotationRemoved(t *testing.T, tc *testEnvContext, nodeName string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node := &corev1.Node{}
		if err := tc.client.Get(tc.ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			return false
		}

		_, exists := node.Annotations[annotationKey]

		return !exists
	}, testTimeout, testPollInterval, "Annotation on node %s should be removed", nodeName)
}

func assertPodNotDeleted(t *testing.T, tc *testEnvContext, podName, podNamespace string, waitDuration time.Duration) {
	t.Helper()

	assert.Never(t, func() bool {
		p := &corev1.Pod{}
		if err := tc.client.Get(tc.ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, p); err != nil {
			return apierrors.IsNotFound(err)
		}

		return p.DeletionTimestamp != nil
	}, waitDuration, testPollInterval, "Pod %s/%s should NOT be deleted while draining", podNamespace, podName)
}

func assertDrainNotComplete(t *testing.T, tc *testEnvContext, drName, drNamespace string) {
	t.Helper()

	dr := &drainv1alpha1.DrainRequest{}
	require.NoError(t, tc.client.Get(tc.ctx, types.NamespacedName{Name: drName, Namespace: drNamespace}, dr))

	for _, c := range dr.Status.Conditions {
		if c.Type == drainCompleteConditionType && c.Status == metav1.ConditionTrue {
			t.Fatalf("DrainRequest %s/%s should NOT have DrainComplete=True while pods are still draining", drNamespace, drName)
		}
	}
}

func assertNodeAnnotation(t *testing.T, tc *testEnvContext, nodeName, expectedValue string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node := &corev1.Node{}
		if err := tc.client.Get(tc.ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			return false
		}

		val, exists := node.Annotations[annotationKey]

		return exists && val == expectedValue
	}, testTimeout, testPollInterval, "Node %s should have annotation %q=%q", nodeName, annotationKey, expectedValue)

	node := &corev1.Node{}
	require.NoError(t, tc.client.Get(tc.ctx, types.NamespacedName{Name: nodeName}, node))
	assert.Equal(t, expectedValue, node.Annotations[annotationKey])
}
