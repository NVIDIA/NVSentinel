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
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "no cases", content: "objects: []\n", wantErr: "has no cases"},
		{name: "unknown field", content: "cases:\n  - name: a\n    policy: p\n    resource: {kind: Pod}\n    expected: {match: true}\n", wantErr: "unknown field"},
		{name: "missing name", content: "cases:\n  - policy: p\n    resource: {kind: Pod}\n", wantErr: "has no name"},
		{name: "duplicate name", content: "cases:\n  - {name: a, policy: p, resource: {kind: Pod}}\n  - {name: a, policy: p, resource: {kind: Pod}}\n", wantErr: "duplicate case name"},
		{name: "missing policy", content: "cases:\n  - {name: a, resource: {kind: Pod}}\n", wantErr: "has no policy"},
		{name: "missing resource", content: "cases:\n  - {name: a, policy: p}\n", wantErr: "has no resource"},
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
