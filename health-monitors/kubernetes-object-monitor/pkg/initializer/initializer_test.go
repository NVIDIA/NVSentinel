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
package initializer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

func TestBuildManagerOptions_CacheSyncTimeout_PreservesConfiguredValue(t *testing.T) {
	timeout := 10 * time.Minute

	opts := buildManagerOptions(Params{CacheSyncTimeout: timeout}, cache.Options{})

	require.Equal(t, timeout, opts.Controller.CacheSyncTimeout)
}

func TestBuildManagerOptions_UnstructuredReads_AreServedFromCache(t *testing.T) {
	opts := buildManagerOptions(Params{}, cache.Options{})

	// Without this the reconciler's Get of an unstructured object is a live
	// call to the API server on every reconcile.
	require.NotNil(t, opts.Client.Cache)
	require.True(t, opts.Client.Cache.Unstructured)
}

func TestBuildCachePlan_NamespacedPolicies_LimitGVKToThoseNamespaces(t *testing.T) {
	resyncPeriod := time.Minute
	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("gpu-operator-pod-health", "", "v1", "Pod", "gpu-operator"),
		testPolicy("monitoring-pod-health", "", "v1", "Pod", "monitoring"),
		testPolicy("node-not-ready", "", "v1", "Node", ""),
	}, resyncPeriod)
	require.NoError(t, err)

	require.NotNil(t, plan.options.SyncPeriod)
	require.Equal(t, resyncPeriod, *plan.options.SyncPeriod)

	byObj, ok := byObjectForGVK(plan.options, schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	require.True(t, ok)
	require.Contains(t, byObj.Namespaces, "gpu-operator")
	require.Contains(t, byObj.Namespaces, "monitoring")

	// The cluster-scoped Node still gets an entry so that it has somewhere to
	// carry a transform, and its Namespaces stays nil, which controller-runtime
	// requires for cluster-scoped kinds and which caches cluster-wide.
	byObj, ok = byObjectForGVK(plan.options, schema.GroupVersionKind{Version: "v1", Kind: "Node"})
	require.True(t, ok)
	require.Nil(t, byObj.Namespaces)
}

func TestBuildCachePlan_PolicyWithoutNamespace_CachesGVKClusterWide(t *testing.T) {
	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("gpu-operator-pod-health", "", "v1", "Pod", "gpu-operator"),
		testPolicy("all-pod-health", "", "v1", "Pod", ""),
	}, time.Minute)
	require.NoError(t, err)

	byObj, ok := byObjectForGVK(plan.options, schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	require.True(t, ok)
	require.Empty(t, byObj.Namespaces)
}

func TestBuildCachePlan_WatchedGVKs_EachGetATransform(t *testing.T) {
	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
		policyWithExpressions("pod-health", podGVK, `resource.status.phase != 'Running'`, ""),
	}, time.Minute)
	require.NoError(t, err)

	for _, gvk := range []schema.GroupVersionKind{nodeGVK, podGVK} {
		byObj, ok := byObjectForGVK(plan.options, gvk)
		require.True(t, ok, "no cache entry for %s", gvk)
		require.NotNil(t, byObj.Transform, "no transform for %s", gvk)
	}
}

func TestBuildCachePlan_UnderivablePolicyFields_OmitTransform(t *testing.T) {
	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		policyWithExpressions("node-opaque", nodeGVK, `size(resource) > 3`, ""),
	}, time.Minute)
	require.NoError(t, err)

	byObj, ok := byObjectForGVK(plan.options, nodeGVK)
	require.True(t, ok)
	require.Nil(t, byObj.Transform)
}

func TestBuildCachePlan_LiteralGVKLookup_GetsAClusterWideEntry(t *testing.T) {
	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		policyWithExpressions("node-owns-pod", nodeGVK,
			`lookup('v1', 'Pod', 'default', 'device-plugin').spec.nodeName == resource.metadata.name`, ""),
	}, time.Minute)
	require.NoError(t, err)

	require.Equal(t, []schema.GroupVersionKind{podGVK}, plan.lookupGVKs)

	// A lookup names whichever namespace it likes, so the entry has to hold
	// them all.
	byObj, ok := byObjectForGVK(plan.options, podGVK)
	require.True(t, ok, "no cache entry for the looked-up GVK")
	require.Nil(t, byObj.Namespaces)
	require.NotNil(t, byObj.Transform)
}

// TestBuildCachePlan_NamespaceRestrictedLookupGVK_ReadsThroughAPI covers a GVK
// cached for named namespaces because that is all its own policy watches. A
// lookup() is free to name any other namespace, and reading an entry that does
// not hold it fails, so such a GVK keeps reading through the API.
func TestBuildCachePlan_NamespaceRestrictedLookupGVK_ReadsThroughAPI(t *testing.T) {
	podsInOneNamespace := policyWithExpressions("pod-health", podGVK, `resource.status.phase != 'Running'`, "")
	podsInOneNamespace.Resource.Namespace = "gpu-operator"

	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		podsInOneNamespace,
		policyWithExpressions("node-owns-pod", nodeGVK,
			`lookup('v1', 'Pod', 'monitoring', 'device-plugin').spec.nodeName == resource.metadata.name`, ""),
	}, time.Minute)
	require.NoError(t, err)

	require.Empty(t, plan.lookupGVKs)

	byObj, ok := byObjectForGVK(plan.options, podGVK)
	require.True(t, ok)
	require.Contains(t, byObj.Namespaces, "gpu-operator")
}

func TestBuildCachePlan_NoEnabledPolicies_HasNoEntries(t *testing.T) {
	disabled := testPolicy("node-not-ready", "", "v1", "Node", "")
	disabled.Enabled = false

	plan, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{disabled}, time.Minute)
	require.NoError(t, err)

	require.Empty(t, plan.options.ByObject)
}

func TestBuildCachePlan_NamespaceForClusterScopedGVK_IsRejected(t *testing.T) {
	_, err := buildCachePlanWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("cluster-thing-health", "example.com", "v1", "ClusterThing", "gpu-operator"),
	}, time.Minute)

	require.Error(t, err)
	require.Contains(t, err.Error(), "resource.namespace cannot be set for cluster-scoped resource example.com/v1, Kind=ClusterThing")
}

func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Version: "v1"},
		{Group: "example.com", Version: "v1"},
	})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "ClusterThing"}, meta.RESTScopeRoot)

	return mapper
}

func byObjectForGVK(opts cache.Options, gvk schema.GroupVersionKind) (cache.ByObject, bool) {
	for obj, byObj := range opts.ByObject {
		if obj.GetObjectKind().GroupVersionKind() == gvk {
			return byObj, true
		}
	}

	return cache.ByObject{}, false
}

func testPolicy(name, group, version, kind, namespace string) config.Policy {
	return config.Policy{
		Name:    name,
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:     group,
			Version:   version,
			Kind:      kind,
			Namespace: namespace,
		},
		Predicate: config.PredicateSpec{
			Expression: "true",
		},
		HealthEvent: config.HealthEventSpec{
			ComponentClass: "Software",
			Message:        "test",
		},
	}
}
