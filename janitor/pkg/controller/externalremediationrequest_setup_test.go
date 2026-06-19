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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
)

func TestIndexExtRRByNodeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		obj  ctrlclient.Object
		want []string
	}{
		{
			name: "valid ExtRR returns its node name",
			obj:  newTestExtRR("a", "node-1"),
			want: []string{"node-1"},
		},
		{
			name: "nil spec returns nil",
			obj:  &nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
			want: nil,
		},
		{
			name: "nil HealthEvent returns nil",
			obj: &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec:       &protos.ExternalRemediationRequestSpec{},
			},
			want: nil,
		},
		{
			name: "empty nodeName returns nil",
			obj: &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec: &protos.ExternalRemediationRequestSpec{
					HealthEvent: &protos.HealthEvent{},
				},
			},
			want: nil,
		},
		{
			name: "wrong runtime type returns nil",
			obj:  &corev1.Node{},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, indexExtRRByNodeName(tc.obj))
		})
	}
}

func TestNodeReleaseStateChangedPredicate(t *testing.T) {
	t.Parallel()

	p := nodeReleaseStateChangedPredicate()
	base := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}

	t.Run("Create always allowed", func(t *testing.T) {
		t.Parallel()
		assert.True(t, p.Create(event.CreateEvent{Object: base}))
	})

	t.Run("Delete always allowed", func(t *testing.T) {
		t.Parallel()
		assert.True(t, p.Delete(event.DeleteEvent{Object: base}))
	})

	t.Run("Update with no release-state change is dropped", func(t *testing.T) {
		t.Parallel()
		assert.False(t, p.Update(event.UpdateEvent{
			ObjectOld: base.DeepCopy(),
			ObjectNew: base.DeepCopy(),
		}))
	})

	t.Run("Update with unrelated label change is dropped", func(t *testing.T) {
		t.Parallel()
		newNode := base.DeepCopy()
		newNode.Labels = map[string]string{"node.kubernetes.io/region": "us-east-1"}
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newNode}))
	})

	t.Run("Update setting managed label fires", func(t *testing.T) {
		t.Parallel()
		newNode := base.DeepCopy()
		newNode.Labels = map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse}
		assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newNode}))
	})

	t.Run("Update adding release taint fires", func(t *testing.T) {
		t.Parallel()
		newNode := base.DeepCopy()
		newNode.Spec.Taints = []corev1.Taint{{
			Key: ReleaseTaintKey, Value: "owner-a", Effect: corev1.TaintEffectNoSchedule,
		}}
		assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newNode}))
	})

	t.Run("Update changing release taint owner fires", func(t *testing.T) {
		t.Parallel()
		oldNode := base.DeepCopy()
		oldNode.Spec.Taints = []corev1.Taint{{Key: ReleaseTaintKey, Value: "owner-a"}}
		newNode := base.DeepCopy()
		newNode.Spec.Taints = []corev1.Taint{{Key: ReleaseTaintKey, Value: "owner-b"}}
		assert.True(t, p.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode}))
	})

	t.Run("Update with unrelated taint change is dropped", func(t *testing.T) {
		t.Parallel()
		newNode := base.DeepCopy()
		newNode.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unschedulable"}}
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newNode}))
	})

	t.Run("Update with non-Node object returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, p.Update(event.UpdateEvent{
			ObjectOld: &corev1.Pod{},
			ObjectNew: &corev1.Pod{},
		}))
	})
}

func TestMapNodeToExtRRs(t *testing.T) {
	t.Parallel()

	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, nvsentinelv1.AddNVSentinelToScheme(s))

	targetA := newTestExtRR("on-target-a", "node-target")
	targetB := newTestExtRR("on-target-b", "node-target")
	other := newTestExtRR("on-other", "node-other")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(targetA, targetB, other).
		WithIndex(&nvsentinelv1.ExternalRemediationRequest{}, extrrNodeNameIndexKey, indexExtRRByNodeName).
		Build()

	r := &ExternalRemediationRequestReconciler{Client: c}

	t.Run("returns ExtRRs whose nodeName matches", func(t *testing.T) {
		requests := r.mapNodeToExtRRs(context.Background(),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-target"}})

		var names []string
		for _, req := range requests {
			names = append(names, req.Name)
		}
		assert.ElementsMatch(t, []string{"on-target-a", "on-target-b"}, names)
	})

	t.Run("returns empty slice when no ExtRR targets the node", func(t *testing.T) {
		requests := r.mapNodeToExtRRs(context.Background(),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-with-nothing"}})
		assert.Empty(t, requests)
	})

	t.Run("returns nil for non-Node object", func(t *testing.T) {
		requests := r.mapNodeToExtRRs(context.Background(), &corev1.Pod{})
		assert.Nil(t, requests)
	})
}
