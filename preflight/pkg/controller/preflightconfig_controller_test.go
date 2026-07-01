// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"time"

	preflightv1alpha1 "github.com/nvidia/nvsentinel/preflight/api/v1alpha1"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func pfcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, preflightv1alpha1.AddToScheme(scheme))

	return scheme
}

func pfcTestRESTMapper() meta.RESTMapper {
	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "scheduling.k8s.io", Version: "v1alpha2"},
		{Group: "scheduling.volcano.sh", Version: "v1beta1"},
	})
	rm.Add(schema.GroupVersionKind{Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "PodGroup"}, meta.RESTScopeNamespace)
	rm.Add(schema.GroupVersionKind{Group: "scheduling.volcano.sh", Version: "v1beta1", Kind: "PodGroup"}, meta.RESTScopeNamespace)

	return rm
}

func volcanoPFC(namespace, name string) *preflightv1alpha1.PreflightConfig {
	return &preflightv1alpha1.PreflightConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: preflightv1alpha1.PreflightConfigSpec{
			GangDiscovery: preflightv1alpha1.GangDiscoverySpec{
				Name:           "volcano",
				AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
				PodGroupGVR: preflightv1alpha1.GroupVersionResource{
					Group: "scheduling.volcano.sh", Version: "v1beta1", Resource: "podgroups",
				},
				MinCountExpr: "podGroup.spec.minMember",
			},
		},
	}
}

func newReconcilerWith(
	t *testing.T,
	resolver *gang.DiscovererResolver,
	objs ...client.Object,
) (*PreflightConfigReconciler, client.Client) {
	t.Helper()

	scheme := pfcTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&preflightv1alpha1.PreflightConfig{}).
		Build()

	return NewPreflightConfigReconciler(c, pfcTestRESTMapper(), resolver), c
}

func reconcile(t *testing.T, r *PreflightConfigReconciler, namespace, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: namespace, Name: name},
	})
	require.NoError(t, err)
}

func TestPreflightConfigReconciler_RegistersDiscoverer(t *testing.T) {
	def := &mockDiscoverer{} // cluster-wide default
	resolver := gang.NewResolver(def, nil)

	pfc := volcanoPFC("team-a", "default")
	r, c := newReconcilerWith(t, resolver, pfc)

	reconcile(t, r, "team-a", "default")

	// Resolver now routes team-a to the volcano discoverer.
	require.NotNil(t, resolver.For("team-a"))
	assert.Equal(t, "volcano", resolver.For("team-a").Name())

	// Status reflects readiness via the Ready condition.
	var got preflightv1alpha1.PreflightConfig
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "default"}, &got))
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, preflightv1alpha1.ConditionReady))
	assert.Equal(t, "volcano", got.Status.Discoverer)
}

// readyCondition returns the Ready condition of a PreflightConfig (or nil).
func readyCondition(t *testing.T, c client.Client, namespace, name string) *metav1.Condition {
	t.Helper()

	var pfc preflightv1alpha1.PreflightConfig
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pfc))

	cond := meta.FindStatusCondition(pfc.Status.Conditions, preflightv1alpha1.ConditionReady)
	require.NotNil(t, cond, "Ready condition should be set")

	return cond
}

func TestPreflightConfigReconciler_InvalidConfigFallsBack(t *testing.T) {
	def := &mockDiscoverer{}
	resolver := gang.NewResolver(def, nil)

	// name set but missing GVR/keys/expr => invalid (partial PodGroup config).
	bad := &preflightv1alpha1.PreflightConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: preflightv1alpha1.PreflightConfigSpec{
			GangDiscovery: preflightv1alpha1.GangDiscoverySpec{Name: "broken"},
		},
	}
	r, c := newReconcilerWith(t, resolver, bad)

	reconcile(t, r, "team-a", "default")

	// No override registered; namespace falls back to the default discoverer.
	assert.Same(t, def, resolver.For("team-a"))

	cond := readyCondition(t, c, "team-a", "default")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "invalid configuration")
}

func TestPreflightConfigReconciler_MultipleOldestWins(t *testing.T) {
	def := &mockDiscoverer{}
	resolver := gang.NewResolver(def, nil)

	// Equal (zero) creation timestamps => tie-break by name, so "one" wins.
	r, c := newReconcilerWith(t, resolver, volcanoPFC("team-a", "one"), volcanoPFC("team-a", "two"))

	reconcile(t, r, "team-a", "two")

	// The oldest/winning config stays active; the namespace is not disrupted.
	require.NotNil(t, resolver.For("team-a"))
	assert.Equal(t, "volcano", resolver.For("team-a").Name())

	assert.Equal(t, metav1.ConditionTrue, readyCondition(t, c, "team-a", "one").Status)

	var winner preflightv1alpha1.PreflightConfig
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "one"}, &winner))
	assert.Equal(t, "volcano", winner.Status.Discoverer)

	loser := readyCondition(t, c, "team-a", "two")
	assert.Equal(t, metav1.ConditionFalse, loser.Status)
	assert.Contains(t, loser.Message, `"one" is already active`)
}

func TestPreflightConfigReconciler_MultipleOldestInvalid(t *testing.T) {
	def := &mockDiscoverer{}
	resolver := gang.NewResolver(def, nil)

	// Selection must be by age, not name: the invalid config is explicitly the
	// OLDEST (earlier CreationTimestamp) yet named so a name-based tiebreak would
	// pick the other one. If timestamp ordering works, "z-broken" wins.
	older := metav1.NewTime(time.Now().Add(-time.Hour))
	newer := metav1.NewTime(time.Now())

	broken := &preflightv1alpha1.PreflightConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "z-broken", CreationTimestamp: older},
		Spec: preflightv1alpha1.PreflightConfigSpec{
			GangDiscovery: preflightv1alpha1.GangDiscoverySpec{Name: "broken"},
		},
	}
	valid := volcanoPFC("team-a", "a-valid")
	valid.CreationTimestamp = newer
	r, c := newReconcilerWith(t, resolver, broken, valid)

	reconcile(t, r, "team-a", "z-broken")

	// Invalid winner is not activated => namespace falls back to the default.
	assert.Same(t, def, resolver.For("team-a"))

	winner := readyCondition(t, c, "team-a", "z-broken")
	assert.Equal(t, metav1.ConditionFalse, winner.Status)
	assert.Contains(t, winner.Message, "invalid configuration")

	younger := readyCondition(t, c, "team-a", "a-valid")
	assert.Equal(t, metav1.ConditionFalse, younger.Status)
	// Must not falsely claim the winner is active.
	assert.NotContains(t, younger.Message, "is already active")
	assert.Contains(t, younger.Message, "not active: invalid configuration")
}

func TestPreflightConfigReconciler_RemovalFallsBack(t *testing.T) {
	def := &mockDiscoverer{}
	volcano := &mockDiscoverer{}
	resolver := gang.NewResolver(def, map[string]gang.GangDiscoverer{"team-a": volcano})

	// No objects in the namespace (the config was deleted).
	r, _ := newReconcilerWith(t, resolver)

	reconcile(t, r, "team-a", "default")

	assert.Same(t, def, resolver.For("team-a"))
}
