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

package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
)

// mockDrainPublisher records PublishDrainEvents calls for tests.
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

func TestReconciler_ExternalDrain_PublishesUnhealthy(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHealthCheck", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
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
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "slurm-operator: cordon"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls := mockPub.getCalls()
	assert.Len(t, calls, 0)
}

func TestReconciler_DrainCleared_PublishesHealthy(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].IsHealthy)
	mockPub.reset()

	// Clear drain condition
	pod2 := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod2))
	pod2.Status.Conditions = nil
	require.NoError(t, cl.Status().Update(context.Background(), pod2))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls = mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	// Healthy event should carry the previously matched reasons for per-check correlation.
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
}

func TestReconciler_PodDeleted_PublishesHealthy(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	mockPub.reset()

	require.NoError(t, cl.Delete(context.Background(), pod))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
}

func TestReconciler_MessageChangesToNonMatching_PublishesHealthy(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.Len(t, mockPub.getCalls(), 1)
	mockPub.reset()

	// Change message to something that doesn't match any pattern
	pod2 := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod2))
	pod2.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "some unrecognized reason"},
	}
	require.NoError(t, cl.Status().Update(context.Background(), pod2))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	assert.Equal(t, "node-1", calls[0].NodeName)
	require.Len(t, calls[0].Reasons, 1)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)

	// Verify state is cleared — re-reconcile should not publish again
	mockPub.reset()
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)
	assert.Len(t, mockPub.getCalls(), 0)
}

func TestReconciler_UnchangedMessage_NoPublish(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.Len(t, mockPub.getCalls(), 1)
	mockPub.reset()

	// Re-reconcile same pod (no change)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	assert.Len(t, mockPub.getCalls(), 0)
}

func TestReconciler_PublishError_PreservesState(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)

	publishErr := fmt.Errorf("gRPC unavailable")
	callCount := 0
	mockPub := &mockDrainPublisher{
		publish: func(_ context.Context, _ []parser.MatchedReason, _ string, _ bool, _, _ string) error {
			callCount++
			// First call succeeds (unhealthy publish), second call fails (healthy publish)
			if callCount == 1 {
				return nil
			}
			return publishErr
		},
	}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	// First reconcile: publishes unhealthy (succeeds)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	// Delete the pod
	require.NoError(t, cl.Delete(context.Background(), pod))

	// Second reconcile: pod deleted, tries to publish healthy (fails)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.Error(t, err)

	// State should be preserved for retry
	reconciler.mu.RLock()
	_, stillTracked := reconciler.matchStates["default/slurmd-0"]
	reconciler.mu.RUnlock()
	assert.True(t, stillTracked, "matchStates should preserve state on publish failure")
}

func TestReconciler_MessageChangeBetweenMatchingPatterns(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)
	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].IsHealthy)
	mockPub.reset()

	// Change message to different reason that still matches
	pod2 := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod2))
	pod2.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] Memory Error"},
	}
	require.NoError(t, cl.Status().Update(context.Background(), pod2))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	// Should get healthy (for old reasons) then unhealthy (for new reasons)
	calls = mockPub.getCalls()
	require.Len(t, calls, 2)
	assert.True(t, calls[0].IsHealthy, "first call should be healthy for previous match")
	assert.False(t, calls[1].IsHealthy, "second call should be unhealthy for new match")
	require.Len(t, calls[1].Reasons, 1)
	assert.Equal(t, "[HC] Memory Error", calls[1].Reasons[0].Segment)
}

func TestReconciler_MultiPatternMatch(t *testing.T) {
	s := newTestScheme()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
		{Name: "notresp", Regex: `Not responding`, CheckName: "SlurmNotResponding", ComponentClass: "NODE"},
	}
	pr, err := parser.New("; ", patterns)
	require.NoError(t, err)
	mockPub := &mockDrainPublisher{}
	reconciler := NewDrainReconciler(cl, pr, mockPub)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slurmd-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodConditionType(ConditionTypeDrain), Status: corev1.ConditionTrue, Message: "[HC] GPU ECC; Not responding"},
			},
		},
	}
	require.NoError(t, cl.Create(context.Background(), pod))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls := mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].IsHealthy)
	require.Len(t, calls[0].Reasons, 2)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
	assert.Equal(t, "SlurmNotResponding", calls[0].Reasons[1].CheckName)
	mockPub.reset()

	// Clear drain — healthy event should carry both previous reasons
	pod2 := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slurmd-0"}, pod2))
	pod2.Status.Conditions = nil
	require.NoError(t, cl.Status().Update(context.Background(), pod2))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "slurmd-0"}})
	require.NoError(t, err)

	calls = mockPub.getCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].IsHealthy)
	require.Len(t, calls[0].Reasons, 2)
	assert.Equal(t, "SlurmHC", calls[0].Reasons[0].CheckName)
	assert.Equal(t, "SlurmNotResponding", calls[0].Reasons[1].CheckName)
}

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)

	return s
}
