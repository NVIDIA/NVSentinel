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

// The tests in this file need envtest binaries. See the module Makefile, or:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	source <(setup-envtest use -p env)

package initializer

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
)

// TestNodeNotReadyPolicy_PrunedCacheEntry_EvaluatesAndPublishes takes the
// as-shipped node-not-ready policy through the real cache built from it: a node
// created against a live API server is pruned on its way into the informer, and
// the reconciler then reads that pruned object, evaluates the predicate against
// it and publishes.
//
// The reconciler's client reads unstructured objects from the cache, matching
// what buildManagerOptions configures, so a pruned object missing anything the
// reconcile path needs fails this test.
func TestNodeNotReadyPolicy_PrunedCacheEntry_EvaluatesAndPublishes(t *testing.T) {
	const nodeName = "gpu-node-0042"

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	policies := []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
	}

	restConfig := startEnvtest(t)

	plan, err := buildCachePlan(restConfig, policies, time.Hour)
	require.NoError(t, err)

	cachedNodes, err := cache.New(restConfig, plan.options)
	require.NoError(t, err)

	startCache(t, ctx, cachedNodes)

	writer, err := client.New(restConfig, client.Options{})
	require.NoError(t, err)

	cachedReader, err := client.New(restConfig, client.Options{
		Cache: &client.CacheOptions{Reader: cachedNodes, Unstructured: true},
	})
	require.NoError(t, err)

	require.NoError(t, writer.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				"nvidia.com/gpu.present":       "true",
				"node.kubernetes.io/instance":  "p5.48xlarge",
				"topology.kubernetes.io/zone":  "us-west-2a",
				"kubernetes.io/os":             "linux",
				"nvsentinel.nvidia.com/tenant": "research",
			},
			Annotations: map[string]string{
				"nvsentinel.nvidia.com/notes": "a value that only exists to be pruned",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-0abcdef1234567890"},
	}))

	setNodeReady(t, ctx, writer, nodeName, corev1.ConditionTrue)
	awaitCachedReadyCondition(t, ctx, cachedNodes, nodeName, "True")

	cached := getCachedNode(t, ctx, cachedNodes, nodeName)

	// The transform engaged on a cluster-scoped kind, which is what the missing
	// ByObject entry used to prevent.
	_, found, err := unstructured.NestedMap(cached.Object, "metadata", "labels")
	require.NoError(t, err)
	require.False(t, found, "labels should have been pruned from the cached node")

	_, found, err = unstructured.NestedFieldNoCopy(cached.Object, "spec")
	require.NoError(t, err)
	require.False(t, found, "spec should have been pruned from the cached node")

	// Informer-critical metadata and the fields the policy reads survived.
	require.Equal(t, nodeName, cached.GetName())
	require.Equal(t, "Node", cached.GetKind())
	require.NotEmpty(t, cached.GetUID())
	require.NotEmpty(t, cached.GetResourceVersion())

	conditions, found, err := unstructured.NestedSlice(cached.Object, "status", "conditions")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, conditions)

	// A pruned node that is Ready does not match, and reconciling publishes
	// nothing.
	publisher := &recordingPublisher{}
	reconciler := newTestReconciler(t, cachedReader, publisher, policies)

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, publisher.events)

	// Transitioning to NotReady evaluates true against the pruned object and
	// publishes an unhealthy event for the node.
	setNodeReady(t, ctx, writer, nodeName, corev1.ConditionFalse)
	awaitCachedReadyCondition(t, ctx, cachedNodes, nodeName, "False")

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)

	require.Len(t, publisher.events, 1)
	require.Equal(t, nodeName, publisher.events[0].nodeName)
	require.False(t, publisher.events[0].isHealthy)
	require.Equal(t, "Node", publisher.events[0].resourceInfo.Kind)
	require.Equal(t, nodeName, publisher.events[0].resourceInfo.Name)

	// Recovering evaluates false again and publishes the healthy event.
	setNodeReady(t, ctx, writer, nodeName, corev1.ConditionTrue)
	awaitCachedReadyCondition(t, ctx, cachedNodes, nodeName, "True")

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)

	require.Len(t, publisher.events, 2)
	require.True(t, publisher.events[1].isHealthy)
}

// TestLookup_GVKWithoutCacheEntry_StartsNoInformer guards the pairing of the
// two clients. Unstructured reads are served from the cache so that reconciling
// does not hit the API server, but a cached read of a GVK the cache has no
// entry for starts a cluster-wide informer for it on demand and holds it in
// full. Only the GVKs the policies name with literals get an entry, so a GVK
// named any other way has to be read through the API.
func TestLookup_GVKWithoutCacheEntry_StartsNoInformer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	// The policy names no GVK in a lookup, so nothing but Node is cached.
	policies := []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
	}

	restConfig := startEnvtest(t)

	plan, err := buildCachePlan(restConfig, policies, time.Hour)
	require.NoError(t, err)
	require.Empty(t, plan.lookupGVKs)

	informedKinds := newKindRecorder(&plan.options)

	cachedPods, err := cache.New(restConfig, plan.options)
	require.NoError(t, err)

	startCache(t, ctx, cachedPods)

	apiReader, err := client.New(restConfig, client.Options{})
	require.NoError(t, err)

	require.NoError(t, apiReader.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "device-plugin-abcde", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "gpu-node-0042",
			Containers: []corev1.Container{{Name: "ctr", Image: "registry.k8s.io/pause:3.10"}},
		},
	}))

	celEnv, err := celenv.NewEnvironment(apiReader)
	require.NoError(t, err)

	celEnv.UseCacheForLookups(cachedPods, plan.lookupGVKs)

	compiled, err := celEnv.Compile(`lookup('v1', 'Pod', 'default', 'device-plugin-abcde').spec.nodeName`)
	require.NoError(t, err)

	result, err := celEnv.Evaluate(compiled, map[string]any{}, ctx)
	require.NoError(t, err)
	require.Equal(t, "gpu-node-0042", result.Value())

	require.NotContains(t, informedKinds(), "Pod",
		"lookup() started an informer for a GVK with no cache entry")
}

// TestLookup_LiteralGVK_ReadsFromPrunedCacheEntry covers the other side of that
// pairing: a lookup() naming its GVK with literals has the fields it reads
// derived like any other, so the GVK gets an entry pruned to them and the call
// is served from the informer instead of the API server.
func TestLookup_LiteralGVK_ReadsFromPrunedCacheEntry(t *testing.T) {
	const (
		nodeName = "gpu-node-0042"
		podName  = "device-plugin-abcde"
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	policies := []config.Policy{
		policyWithExpressions("node-owns-device-plugin", nodeGVK,
			`lookup('v1', 'Pod', 'default', '`+podName+`').spec.nodeName == resource.metadata.name`, ""),
	}

	restConfig := startEnvtest(t)

	plan, err := buildCachePlan(restConfig, policies, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []schema.GroupVersionKind{podGVK}, plan.lookupGVKs)

	informedKinds := newKindRecorder(&plan.options)

	cachedPods, err := cache.New(restConfig, plan.options)
	require.NoError(t, err)

	startCache(t, ctx, cachedPods)

	writer, err := client.New(restConfig, client.Options{})
	require.NoError(t, err)

	require.NoError(t, writer.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   "default",
			Labels:      map[string]string{"app": "nvidia-device-plugin"},
			Annotations: map[string]string{"nvsentinel.nvidia.com/notes": "only exists to be pruned"},
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "ctr", Image: "registry.k8s.io/pause:3.10"}},
		},
	}))

	// The API reader is left out, so a read that reaches it fails the test
	// rather than quietly passing on a live GET.
	celEnv, err := celenv.NewEnvironment(nil)
	require.NoError(t, err)

	celEnv.UseCacheForLookups(cachedPods, plan.lookupGVKs)

	// A lookup reads through the API server until the informer behind the GVK
	// has caught up, which is what leaving the API reader out would trip over.
	// Waiting here is what the first evaluation of a running monitor declines
	// to do.
	warmLookupInformer(t, ctx, cachedPods, podGVK)

	compiled, err := celEnv.Compile(policies[0].Predicate.Expression)
	require.NoError(t, err)

	node := map[string]any{"metadata": map[string]any{"name": nodeName}}

	result, err := celEnv.Evaluate(compiled, node, ctx)
	require.NoError(t, err)
	require.Equal(t, true, result.Value())

	require.Contains(t, informedKinds(), "Pod",
		"the looked-up GVK should be served by an informer")

	// The entry holds what the expression reads off the pod and nothing else.
	cachedPod := &unstructured.Unstructured{}
	cachedPod.SetGroupVersionKind(podGVK)

	require.NoError(t, cachedPods.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: podName}, cachedPod))

	nodeNameField, found, err := unstructured.NestedString(cachedPod.Object, "spec", "nodeName")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, nodeName, nodeNameField)

	_, found, err = unstructured.NestedMap(cachedPod.Object, "metadata", "annotations")
	require.NoError(t, err)
	require.False(t, found, "annotations should have been pruned from the cached pod")

	_, found, err = unstructured.NestedSlice(cachedPod.Object, "spec", "containers")
	require.NoError(t, err)
	require.False(t, found, "containers should have been pruned from the cached pod")
}

// newKindRecorder installs an informer constructor that records the kind of
// every informer the cache builds.
func newKindRecorder(opts *cache.Options) func() []string {
	var (
		mu    sync.Mutex
		kinds []string
	)

	opts.NewInformer = func(
		lw toolscache.ListerWatcher,
		obj runtime.Object,
		resync time.Duration,
		indexers toolscache.Indexers,
	) toolscache.SharedIndexInformer {
		mu.Lock()
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
		mu.Unlock()

		return toolscache.NewSharedIndexInformer(lw, obj, resync, indexers)
	}

	return func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(kinds)
	}
}

func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()

	testEnv := &envtest.Environment{}

	restConfig, err := testEnv.Start()
	require.NoError(t, err)

	t.Cleanup(func() {
		assert.NoError(t, testEnv.Stop())
	})

	return restConfig
}

func startCache(t *testing.T, ctx context.Context, c cache.Cache) {
	t.Helper()

	cacheCtx, stopCache := context.WithCancel(ctx)
	started := make(chan error, 1)

	go func() { started <- c.Start(cacheCtx) }()

	t.Cleanup(func() {
		stopCache()
		<-started
	})

	require.True(t, c.WaitForCacheSync(ctx), "cache did not sync")
}

// warmLookupInformer creates the informer behind gvk and waits for it to catch
// up, which a cached lookup of that GVK will not do for itself.
func warmLookupInformer(t *testing.T, ctx context.Context, c cache.Cache, gvk schema.GroupVersionKind) {
	t.Helper()

	_, err := c.GetInformer(ctx, newUnstructuredForGVK(gvk))
	require.NoError(t, err)
}

func newTestReconciler(
	t *testing.T,
	c client.Client,
	publisher controller.HealthEventPublisher,
	policies []config.Policy,
) *controller.ResourceReconciler {
	t.Helper()

	celEnv, err := celenv.NewEnvironment(c)
	require.NoError(t, err)

	evaluator, err := policy.NewEvaluator(celEnv, policies)
	require.NoError(t, err)

	return controller.NewResourceReconciler(
		c, evaluator, publisher, annotations.NewManager(c), policies, nodeGVK,
	)
}

func getCachedNode(t *testing.T, ctx context.Context, c cache.Cache, name string) *unstructured.Unstructured {
	t.Helper()

	node := newUnstructuredForGVK(nodeGVK).(*unstructured.Unstructured)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name}, node))

	return node
}

func setNodeReady(t *testing.T, ctx context.Context, c client.Client, name string, status corev1.ConditionStatus) {
	t.Helper()

	node := &corev1.Node{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name}, node))

	node.Status.Conditions = []corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             status,
		Reason:             "KubeletReady",
		LastHeartbeatTime:  metav1.Now(),
		LastTransitionTime: metav1.Now(),
	}}

	require.NoError(t, c.Status().Update(ctx, node))
}

// awaitCachedReadyCondition waits for the informer to observe the node's Ready
// condition, so that reads afterwards are of the state under test.
func awaitCachedReadyCondition(t *testing.T, ctx context.Context, c cache.Cache, name, status string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node := newUnstructuredForGVK(nodeGVK).(*unstructured.Unstructured)
		if err := c.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
			return false
		}

		conditions, found, err := unstructured.NestedSlice(node.Object, "status", "conditions")
		if err != nil || !found {
			return false
		}

		for _, entry := range conditions {
			condition, ok := entry.(map[string]any)
			if ok && condition["type"] == "Ready" && condition["status"] == status {
				return true
			}
		}

		return false
	}, 15*time.Second, 100*time.Millisecond)
}

type recordedEvent struct {
	nodeName     string
	isHealthy    bool
	resourceInfo *config.ResourceInfo
}

type recordingPublisher struct {
	events []recordedEvent
}

func (p *recordingPublisher) PublishHealthEvent(
	_ context.Context,
	_ *config.Policy,
	nodeName string,
	isHealthy bool,
	resourceInfo *config.ResourceInfo,
) error {
	p.events = append(p.events, recordedEvent{
		nodeName:     nodeName,
		isHealthy:    isHealthy,
		resourceInfo: resourceInfo,
	})

	return nil
}
