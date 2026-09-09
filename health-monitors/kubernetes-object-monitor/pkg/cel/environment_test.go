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
package cel

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func eval(t *testing.T, c client.Client, expr string, resource any, objs ...client.Object) any {
	t.Helper()
	env, err := NewEnvironment(c)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	ast, err := env.Compile(expr)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	result, err := env.Evaluate(ast, resource, context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return result.Value()
}

func obj(apiVersion, kind, ns, name string, data map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: data}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetNamespace(ns)
	o.SetName(name)
	return o
}

func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).Build()
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		objs   []client.Object
		expr   string
		res    any
		expect any
	}{
		{
			name: "basic",
			objs: []client.Object{
				obj("v1", "Pod", "default", "test-pod", map[string]any{"spec": map[string]any{"nodeName": "node-1"}}),
			},
			expr:   `lookup('v1', 'Pod', 'default', 'test-pod').spec.nodeName`,
			res:    map[string]any{},
			expect: "node-1",
		},
		{
			name:   "not found returns null",
			objs:   nil,
			expr:   `lookup('v1', 'Pod', 'default', 'missing') == null`,
			res:    map[string]any{},
			expect: true,
		},
		{
			name: "cluster-scoped resource",
			objs: []client.Object{
				obj("v1", "Node", "", "node-1", map[string]any{"metadata": map[string]any{"labels": map[string]any{"role": "worker"}}}),
			},
			expr:   `lookup('v1', 'Node', '', 'node-1').metadata.labels.role`,
			res:    map[string]any{},
			expect: "worker",
		},
		{
			name: "with resource variable",
			objs: []client.Object{
				obj("v1", "Pod", "default", "my-pod", map[string]any{"status": map[string]any{"phase": "Running"}}),
			},
			expr:   `lookup('v1', 'Pod', resource.ns, resource.name).status.phase`,
			res:    map[string]any{"ns": "default", "name": "my-pod"},
			expect: "Running",
		},
		{
			name: "different api version",
			objs: []client.Object{
				obj("apps/v1", "Deployment", "default", "app", map[string]any{"spec": map[string]any{"replicas": int64(3)}}),
			},
			expr:   `lookup('apps/v1', 'Deployment', 'default', 'app').spec.replicas`,
			res:    map[string]any{},
			expect: int64(3),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval(t, fakeClient(tt.objs...), tt.expr, tt.res)
			if result != tt.expect {
				t.Errorf("expected %v, got %v", tt.expect, result)
			}
		})
	}
}

// erroringReader stands in for a cached client that cannot serve a GVK, which
// is what a deployment without cluster-wide list and watch on it produces.
type erroringReader struct {
	client.Reader
	err error
}

func (r erroringReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return r.err
}

// stubCache reads through a fake client and reports the informer behind every
// GVK as caught up, or as still catching up when caughtUp is false.
type stubCache struct {
	client.Reader
	caughtUp bool
}

func (c stubCache) GetInformer(
	_ context.Context,
	_ client.Object,
	_ ...cache.InformerGetOption,
) (cache.Informer, error) {
	return stubInformer{caughtUp: c.caughtUp}, nil
}

type stubInformer struct {
	cache.Informer
	caughtUp bool
}

func (i stubInformer) HasSynced() bool {
	return i.caughtUp
}

func caughtUpCache(objs ...client.Object) stubCache {
	return stubCache{Reader: fakeClient(objs...), caughtUp: true}
}

func TestLookup_CachedGVK_ReadsFromTheCache(t *testing.T) {
	cached := caughtUpCache(obj("v1", "Pod", "default", "test-pod",
		map[string]any{"spec": map[string]any{"nodeName": "node-1"}}))

	// Only the cache holds the pod, so a lookup that answers with it cannot
	// have read anywhere else.
	env, err := NewEnvironment(erroringReader{err: errors.New("read through the API server")})
	require.NoError(t, err)

	env.UseCacheForLookups(cached, []schema.GroupVersionKind{{Version: "v1", Kind: "Pod"}})

	compiled, err := env.Compile(`lookup('v1', 'Pod', 'default', 'test-pod').spec.nodeName`)
	require.NoError(t, err)

	result, err := env.Evaluate(compiled, map[string]any{}, context.Background())
	require.NoError(t, err)
	require.Equal(t, "node-1", result.Value())
}

// TestLookup_CachedReadFails_FallsBackToTheAPI keeps a policy working when the
// cache cannot serve the GVK it names. The informer for a GVK no policy watches
// is created by the first read that needs it and wants cluster-wide list and
// watch, which a deployment that granted only get will refuse.
func TestLookup_CachedReadFails_FallsBackToTheAPI(t *testing.T) {
	api := fakeClient(obj("v1", "Pod", "default", "test-pod",
		map[string]any{"spec": map[string]any{"nodeName": "node-1"}}))

	env, err := NewEnvironment(api)
	require.NoError(t, err)

	env.UseCacheForLookups(
		stubCache{
			Reader: erroringReader{err: apierrors.NewForbidden(
				schema.GroupResource{Resource: "pods"}, "", errors.New("cannot list pods"))},
			caughtUp: true,
		},
		[]schema.GroupVersionKind{{Version: "v1", Kind: "Pod"}},
	)

	compiled, err := env.Compile(`lookup('v1', 'Pod', 'default', 'test-pod').spec.nodeName`)
	require.NoError(t, err)

	result, err := env.Evaluate(compiled, map[string]any{}, context.Background())
	require.NoError(t, err)
	require.Equal(t, "node-1", result.Value())
}

// TestLookup_CachedGVKMissingObject_DoesNotFallBack keeps a negative answer
// cheap. Falling back on a missing object would put a live GET behind every
// lookup that finds nothing, which is the case a policy watching for a missing
// object hits on every evaluation.
func TestLookup_CachedGVKMissingObject_DoesNotFallBack(t *testing.T) {
	api := fakeClient(obj("v1", "Pod", "default", "test-pod",
		map[string]any{"spec": map[string]any{"nodeName": "node-1"}}))

	env, err := NewEnvironment(api)
	require.NoError(t, err)

	env.UseCacheForLookups(caughtUpCache(), []schema.GroupVersionKind{{Version: "v1", Kind: "Pod"}})

	compiled, err := env.Compile(`lookup('v1', 'Pod', 'default', 'test-pod') == null`)
	require.NoError(t, err)

	result, err := env.Evaluate(compiled, map[string]any{}, context.Background())
	require.NoError(t, err)
	require.Equal(t, true, result.Value())
}

// TestLookup_InformerStillCatchingUp_ReadsThroughTheAPI keeps evaluation off
// the critical path of an informer sync. The informer for a GVK no policy
// watches is created by the first read that needs it and lists the whole GVK
// before it can answer, and never answers at all where cluster-wide list and
// watch were withheld. Evaluate is serialised, so a read that waited on either
// would hold up every evaluation in the process.
func TestLookup_InformerStillCatchingUp_ReadsThroughTheAPI(t *testing.T) {
	api := fakeClient(obj("v1", "Pod", "default", "test-pod",
		map[string]any{"spec": map[string]any{"nodeName": "from-the-api"}}))

	env, err := NewEnvironment(api)
	require.NoError(t, err)

	env.UseCacheForLookups(
		stubCache{
			Reader: fakeClient(obj("v1", "Pod", "default", "test-pod",
				map[string]any{"spec": map[string]any{"nodeName": "from-the-cache"}})),
			caughtUp: false,
		},
		[]schema.GroupVersionKind{{Version: "v1", Kind: "Pod"}},
	)

	compiled, err := env.Compile(`lookup('v1', 'Pod', 'default', 'test-pod').spec.nodeName`)
	require.NoError(t, err)

	result, err := env.Evaluate(compiled, map[string]any{}, context.Background())
	require.NoError(t, err)
	require.Equal(t, "from-the-api", result.Value())
}

func TestLookupChaining(t *testing.T) {
	node := obj("v1", "Node", "", "node-1", map[string]any{
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	})
	pod := obj("v1", "Pod", "default", "test-pod", map[string]any{
		"spec": map[string]any{"nodeName": "node-1"},
	})
	event := map[string]any{
		"regarding": map[string]any{"namespace": "default", "name": "test-pod"},
	}

	expr := `lookup('v1', 'Node', '', lookup('v1', 'Pod', resource.regarding.namespace, resource.regarding.name).spec.nodeName).status.conditions[0].status`
	result := eval(t, fakeClient(node, pod), expr, event)

	if result != "True" {
		t.Errorf("expected 'True', got %v", result)
	}
}
