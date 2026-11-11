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

package policytest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cacheplan"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

func podPolicies() []config.Policy {
	return []config.Policy{
		{
			Name:     "pod-on-cordoned-node",
			Enabled:  true,
			Resource: config.ResourceSpec{Version: "v1", Kind: "Pod", Namespace: "workloads"},
			Predicate: config.PredicateSpec{
				Expression: `resource.status.phase == "Failed" &&
					lookup("v1", "Node", "", resource.spec.nodeName).spec.unschedulable == true`,
			},
			NodeAssociation: &config.AssociationSpec{Expression: "resource.spec.nodeName"},
		},
		{
			Name:      "disabled-policy",
			Enabled:   false,
			Resource:  config.ResourceSpec{Version: "v1", Kind: "Pod"},
			Predicate: config.PredicateSpec{Expression: "true"},
		},
	}
}

const suiteYAML = `
objects:
  - apiVersion: v1
    kind: Node
    metadata:
      name: node-a
    spec:
      unschedulable: true
  - apiVersion: v1
    kind: Node
    metadata:
      name: node-b
    spec:
      unschedulable: false
cases:
  - name: failed pod on a cordoned node
    policy: pod-on-cordoned-node
    resource:
      apiVersion: v1
      kind: Pod
      metadata: {name: p1, namespace: workloads}
      spec: {nodeName: node-a}
      status: {phase: Failed}
    expect:
      match: true
      node: node-a
  - name: failed pod on a schedulable node
    policy: pod-on-cordoned-node
    resource:
      apiVersion: v1
      kind: Pod
      metadata: {name: p2, namespace: workloads}
      spec: {nodeName: node-b}
      status: {phase: Failed}
    expect:
      match: false
`

func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tests.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestRunPassingSuite(t *testing.T) {
	suite, err := LoadSuite(writeFile(t, suiteYAML))
	require.NoError(t, err)

	results, err := Run(context.Background(), podPolicies(), suite)
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Empty(t, results[0].Failures)
	assert.True(t, results[0].Match)
	assert.Equal(t, "node-a", results[0].Node)

	assert.Empty(t, results[1].Failures)
	assert.False(t, results[1].Match)

	var out bytes.Buffer
	assert.Equal(t, 0, Report(&out, results))
	assert.Contains(t, out.String(), "2 passed, 0 failed")
}

func TestRunReportsFailures(t *testing.T) {
	node := "node-b"

	tests := []struct {
		name        string
		c           Case
		wantFailure string
	}{
		{
			name: "wrong expected match",
			c: Case{Name: "c", Policy: "pod-on-cordoned-node", Expect: Expect{Match: false}, Resource: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "p", "namespace": "workloads"},
				"spec":     map[string]any{"nodeName": "node-a"},
				"status":   map[string]any{"phase": "Failed"},
			}},
			wantFailure: "match is true, want false",
		},
		{
			name: "wrong expected node",
			c: Case{Name: "c", Policy: "pod-on-cordoned-node", Expect: Expect{Match: true, Node: &node}, Resource: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "p", "namespace": "workloads"},
				"spec":     map[string]any{"nodeName": "node-a"},
				"status":   map[string]any{"phase": "Failed"},
			}},
			wantFailure: `node is "node-a", want "node-b"`,
		},
		{
			name:        "unknown policy",
			c:           Case{Name: "c", Policy: "missing", Resource: map[string]any{"apiVersion": "v1", "kind": "Pod"}},
			wantFailure: `policy "missing" is not in the config`,
		},
		{
			name:        "disabled policy",
			c:           Case{Name: "c", Policy: "disabled-policy", Resource: map[string]any{"apiVersion": "v1", "kind": "Pod"}},
			wantFailure: `policy "disabled-policy" is disabled`,
		},
		{
			name: "resource kind the policy does not watch",
			c: Case{Name: "c", Policy: "pod-on-cordoned-node", Resource: map[string]any{
				"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"name": "e", "namespace": "workloads"},
			}},
			wantFailure: "resource is v1/Event but policy",
		},
		{
			name: "namespace the policy does not watch",
			c: Case{Name: "c", Policy: "pod-on-cordoned-node", Resource: map[string]any{
				"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "p", "namespace": "other"},
			}},
			wantFailure: `resource is in namespace "other"`,
		},
		{
			name: "lookup of an object the test file does not list",
			c: Case{Name: "c", Policy: "pod-on-cordoned-node", Resource: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "p", "namespace": "workloads"},
				"spec":     map[string]any{"nodeName": "node-missing"},
				"status":   map[string]any{"phase": "Failed"},
			}},
			wantFailure: "predicate evaluation failed:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suite := &Suite{
				Objects: []map[string]any{{
					"apiVersion": "v1", "kind": "Node",
					"metadata": map[string]any{"name": "node-a"},
					"spec":     map[string]any{"unschedulable": true},
				}},
				Cases: []Case{tt.c},
			}

			results, err := Run(context.Background(), podPolicies(), suite)
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.NotEmpty(t, results[0].Failures)
			assert.Contains(t, results[0].Failures[0], tt.wantFailure)

			var out bytes.Buffer
			assert.Equal(t, 1, Report(&out, results))
			assert.Contains(t, out.String(), "FAIL")
		})
	}
}

func TestLoadSuiteRejectsInvalidFiles(t *testing.T) {
	const pod = "{apiVersion: v1, kind: Pod, metadata: {name: p}}"

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "no cases", content: "objects: []\n", wantErr: "has no cases"},
		{name: "unknown field", content: "cases:\n  - name: a\n    policy: p\n    resource: " + pod + "\n    expected: {match: true}\n", wantErr: "unknown field"},
		{name: "missing name", content: "cases:\n  - {policy: p, resource: " + pod + "}\n", wantErr: "has no name"},
		{name: "duplicate name", content: "cases:\n  - {name: a, policy: p, resource: " + pod + "}\n  - {name: a, policy: p, resource: " + pod + "}\n", wantErr: "duplicate case name"},
		{name: "missing policy", content: "cases:\n  - {name: a, resource: " + pod + "}\n", wantErr: "has no policy"},
		{name: "missing resource", content: "cases:\n  - {name: a, policy: p}\n", wantErr: "has no resource"},
		{name: "resource without apiVersion", content: "cases:\n  - {name: a, policy: p, resource: {kind: Pod, metadata: {name: p}}}\n", wantErr: `case "a" resource needs apiVersion, kind and metadata.name`},
		{name: "resource without kind", content: "cases:\n  - {name: a, policy: p, resource: {apiVersion: v1, metadata: {name: p}}}\n", wantErr: `case "a" resource needs apiVersion, kind and metadata.name`},
		{name: "resource without name", content: "cases:\n  - {name: a, policy: p, resource: {apiVersion: v1, kind: Pod}}\n", wantErr: `case "a" resource needs apiVersion, kind and metadata.name`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSuite(writeFile(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRunRejectsObjectsWithoutIdentity(t *testing.T) {
	suite := &Suite{
		Objects: []map[string]any{{"apiVersion": "v1", "kind": "Node"}},
		Cases:   []Case{{Name: "c", Policy: "pod-on-cordoned-node", Resource: map[string]any{"apiVersion": "v1", "kind": "Pod"}}},
	}

	_, err := Run(context.Background(), podPolicies(), suite)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs apiVersion, kind and metadata.name")
}

func TestRunRejectsDuplicateObjects(t *testing.T) {
	node := map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node-a"}}
	suite := &Suite{
		Objects: []map[string]any{node, node},
		Cases:   []Case{{Name: "c", Policy: "pod-on-cordoned-node", Resource: map[string]any{"apiVersion": "v1", "kind": "Pod"}}},
	}

	_, err := Run(context.Background(), podPolicies(), suite)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "object 1 duplicates object 0 (v1 Node /node-a)")
}

func TestObjectsArePrunedLikeTheCache(t *testing.T) {
	compiler, err := celenv.NewCompilerEnvironment()
	require.NoError(t, err)

	plan := cacheplan.Build(compiler, podPolicies())

	t.Run("watched resource keeps only the fields the policy reads", func(t *testing.T) {
		pod := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "p1", "namespace": "workloads", "labels": map[string]any{"app": "x"}},
			"spec":     map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "c"}}},
			"status":   map[string]any{"phase": "Failed"},
		}}

		got, err := pruned(plan[pod.GroupVersionKind()], pod)
		require.NoError(t, err)
		assert.Equal(t, "node-a", got.Object["spec"].(map[string]any)["nodeName"])
		assert.NotContains(t, got.Object["spec"], "containers")
		assert.NotContains(t, got.Object["metadata"], "labels")
		assert.Contains(t, pod.Object["metadata"], "labels", "the test file's object must not be modified")
	})

	t.Run("looked-up object served from the cache is pruned", func(t *testing.T) {
		node := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Node",
			"metadata": map[string]any{"name": "node-a", "labels": map[string]any{"zone": "a"}},
			"spec":     map[string]any{"unschedulable": true, "podCIDR": "10.0.0.0/24"},
		}}

		got, err := lookedUp(plan, node)
		require.NoError(t, err)
		assert.Equal(t, true, got.Object["spec"].(map[string]any)["unschedulable"])
		assert.NotContains(t, got.Object["spec"], "podCIDR")
		assert.NotContains(t, got.Object["metadata"], "labels")
	})

	t.Run("looked-up object read through the API stays whole", func(t *testing.T) {
		policies := append(podPolicies(), config.Policy{
			Name:      "pod-lookup",
			Enabled:   true,
			Resource:  config.ResourceSpec{Version: "v1", Kind: "Node"},
			Predicate: config.PredicateSpec{Expression: `lookup("v1", "Pod", "other", "p").status.phase == "Failed"`},
		})
		plan := cacheplan.Build(compiler, policies)

		pod := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "p", "namespace": "other", "labels": map[string]any{"app": "x"}},
		}}

		entry := plan[pod.GroupVersionKind()]
		require.NotNil(t, entry.Transform, "pods are pruned in the cache")
		require.False(t, entry.LookupsFromCache, "pods are cached for one namespace, so lookups read the API")

		got, err := lookedUp(plan, pod)
		require.NoError(t, err)
		assert.Contains(t, got.Object["metadata"], "labels")
	})
}

func TestRunReportsMatchWhenNodeAssociationFails(t *testing.T) {
	policies := []config.Policy{{
		Name:            "failed-pod",
		Enabled:         true,
		Resource:        config.ResourceSpec{Version: "v1", Kind: "Pod"},
		Predicate:       config.PredicateSpec{Expression: `resource.status.phase == "Failed"`},
		NodeAssociation: &config.AssociationSpec{Expression: "resource.spec.nodeName"},
	}}
	suite := &Suite{Cases: []Case{{Name: "c", Policy: "failed-pod", Resource: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "p", "namespace": "default"},
		"status":   map[string]any{"phase": "Failed"},
	}, Expect: Expect{Match: true}}}}

	results, err := Run(context.Background(), policies, suite)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Match)
	require.Len(t, results[0].Failures, 1)
	assert.Contains(t, results[0].Failures[0], "node association evaluation failed:")
}
