// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
)

// To run these tests, you need to install and setup envtest:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	source <(setup-envtest use -p env)
//
// Then run the tests:
//
//	go test -v ./pkg/controller/...

type mockDrainPublisher struct {
	mu      sync.Mutex
	calls   []publishCall
	publish func(ctx context.Context, reasons []parser.MatchedReason, nodeName string, isHealthy bool, podNamespace, podName string) error
}

type publishCall struct {
	Reasons      []parser.MatchedReason
	NodeName     string
	IsHealthy    bool
	PodNamespace string
	PodName      string
}

func (m *mockDrainPublisher) PublishDrainEvents(ctx context.Context, reasons []parser.MatchedReason, nodeName string, isHealthy bool, podNamespace, podName string) error {
	m.mu.Lock()
	m.calls = append(m.calls, publishCall{Reasons: reasons, NodeName: nodeName, IsHealthy: isHealthy, PodNamespace: podNamespace, PodName: podName})
	m.mu.Unlock()
	if m.publish != nil {
		return m.publish(ctx, reasons, nodeName, isHealthy, podNamespace, podName)
	}
	return nil
}

func (m *mockDrainPublisher) getCalls() []publishCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]publishCall(nil), m.calls...)
}

func (m *mockDrainPublisher) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

type testSetup struct {
	ctx        context.Context
	k8sClient  client.Client
	reconciler *controller.DrainReconciler
	publisher  *mockDrainPublisher
}

func setupTest(t *testing.T, patterns []config.Pattern, pub *mockDrainPublisher) *testSetup {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to start envtest")
	t.Cleanup(func() {
		assert.NoError(t, testEnv.Stop())
	})

	k8sClient, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)

	reconciler := controller.NewDrainReconciler(k8sClient, pr, pub)

	return &testSetup{
		ctx:        ctx,
		k8sClient:  k8sClient,
		reconciler: reconciler,
		publisher:  pub,
	}
}

func defaultPatterns() []config.Pattern {
	return []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHealthCheck", ComponentClass: "NODE"},
	}
}

func createPodWithDrainCondition(t *testing.T, setup *testSetup, namespace, name, nodeName, message string) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_ = setup.k8sClient.Create(setup.ctx, ns)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "slurmd", Image: "slurmd:latest"}},
		},
	}
	require.NoError(t, setup.k8sClient.Create(setup.ctx, pod))

	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:    corev1.PodConditionType("SlurmNodeStateDrain"),
			Status:  corev1.ConditionTrue,
			Message: message,
		},
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, pod))
}

func reconcile(t *testing.T, setup *testSetup, namespace, name string) (ctrl.Result, error) {
	t.Helper()
	return setup.reconciler.Reconcile(setup.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func TestReconciler_ExternalDrain_PublishesUnhealthy(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	assert.Equal(t, "default", calls[0].PodNamespace)
	assert.Equal(t, "slurmd-0", calls[0].PodName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHealthCheck", calls[0].Reasons[0].CheckName)
	assert.Equal(t, "[HC] GPU ECC", calls[0].Reasons[0].Segment)
}

func TestReconciler_OperatorPrefixed_Skipped(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "slurm-operator: cordon")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	assert.Len(t, mockPub.getCalls(), 0)
}

func TestReconciler_DrainCleared_PublishesHealthy(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, _ = reconcile(t, setup, "default", "slurmd-0")
	require.Len(t, mockPub.getCalls(), 1)
	assert.False(t, mockPub.getCalls()[0].IsHealthy)
	mockPub.reset()

	pod := &corev1.Pod{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod))
	pod.Status.Conditions = nil
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, pod))

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHealthCheck", calls[0].Reasons[0].CheckName)
}

func forceDeletePod(t *testing.T, setup *testSetup, namespace, name string) {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod))
	zero := int64(0)
	require.NoError(t, setup.k8sClient.Delete(setup.ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &zero}))
}

func TestReconciler_PodDeleted_PublishesHealthy(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, _ = reconcile(t, setup, "default", "slurmd-0")
	mockPub.reset()

	forceDeletePod(t, setup, "default", "slurmd-0")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHealthCheck", calls[0].Reasons[0].CheckName)
}

func TestReconciler_MessageChangesToNonMatching_PublishesHealthy(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, _ = reconcile(t, setup, "default", "slurmd-0")
	require.Len(t, mockPub.getCalls(), 1)
	mockPub.reset()

	pod := &corev1.Pod{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod))
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodConditionType("SlurmNodeStateDrain"), Status: corev1.ConditionTrue, Message: "some unrecognized reason"},
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, pod))

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHealthCheck", calls[0].Reasons[0].CheckName)

	mockPub.reset()
	_, err = reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)
	assert.Len(t, mockPub.getCalls(), 0)
}

func TestReconciler_UnchangedMessage_NoPublish(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, _ = reconcile(t, setup, "default", "slurmd-0")
	require.Len(t, mockPub.getCalls(), 1)
	mockPub.reset()

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	assert.Len(t, mockPub.getCalls(), 0)
}

func TestReconciler_PublishError_PreservesState(t *testing.T) {
	publishErr := fmt.Errorf("gRPC unavailable")
	callCount := 0
	mockPub := &mockDrainPublisher{
		publish: func(_ context.Context, _ []parser.MatchedReason, _ string, _ bool, _, _ string) error {
			callCount++
			if callCount == 1 {
				return nil
			}
			return publishErr
		},
	}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	forceDeletePod(t, setup, "default", "slurmd-0")

	_, err = reconcile(t, setup, "default", "slurmd-0")
	require.Error(t, err)

	// Verify state is preserved: reconciling again should attempt publish again (callCount increments)
	prevCallCount := callCount
	_, err = reconcile(t, setup, "default", "slurmd-0")
	require.Error(t, err)
	assert.Greater(t, callCount, prevCallCount, "reconciler should retry publish, proving state was preserved")
}

func TestReconciler_MessageChangeBetweenMatchingPatterns(t *testing.T) {
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, defaultPatterns(), mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)
	require.Len(t, mockPub.getCalls(), 1)
	assert.False(t, mockPub.getCalls()[0].IsHealthy)
	mockPub.reset()

	pod := &corev1.Pod{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod))
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodConditionType("SlurmNodeStateDrain"), Status: corev1.ConditionTrue, Message: "[HC] Memory Error"},
	}
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, pod))

	_, err = reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 2)
	assert.True(t, calls[0].IsHealthy, "first call should be healthy for previous match")
	assert.False(t, calls[1].IsHealthy, "second call should be unhealthy for new match")
	require.Len(t, calls[1].Reasons, 1)
	assert.Equal(t, "[HC] Memory Error", calls[1].Reasons[0].Segment)
}

func TestReconciler_MultiPatternMatch(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
		{Name: "notresp", Regex: `Not responding`, CheckName: "SlurmNotResponding", ComponentClass: "NODE"},
	}
	mockPub := &mockDrainPublisher{}
	setup := setupTest(t, patterns, mockPub)

	createPodWithDrainCondition(t, setup, "default", "slurmd-0", "node-1", "[HC] GPU ECC; Not responding")

	_, err := reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].IsHealthy)
	require.Len(t, calls[0].Reasons, 2)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
	assert.Equal(t, "SlurmNotResponding", calls[0].Reasons[1].CheckName)
	mockPub.reset()

	pod := &corev1.Pod{}
	require.NoError(t, setup.k8sClient.Get(setup.ctx, types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod))
	pod.Status.Conditions = nil
	require.NoError(t, setup.k8sClient.Status().Update(setup.ctx, pod))

	_, err = reconcile(t, setup, "default", "slurmd-0")
	require.NoError(t, err)

	calls = mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	require.Len(t, calls[0].Reasons, 2)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
	assert.Equal(t, "SlurmNotResponding", calls[0].Reasons[1].CheckName)
}
