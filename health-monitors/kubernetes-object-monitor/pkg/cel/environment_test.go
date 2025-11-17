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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
