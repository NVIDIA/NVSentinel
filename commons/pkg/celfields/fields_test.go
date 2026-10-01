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

package celfields

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// gpuOperatorPodPredicate is the predicate of the gpu-operator pod policy
// documented in docs/monitoring-critical-operators.md, verbatim.
const gpuOperatorPodPredicate = `
resource.metadata.namespace == 'gpu-operator' &&
has(resource.metadata.ownerReferences) &&
resource.metadata.ownerReferences.exists(r, r.kind == 'DaemonSet') &&
has(resource.spec.nodeName) && resource.spec.nodeName != "" &&
has(resource.status.startTime) &&
now - timestamp(resource.status.startTime) > duration('30m') &&
(
  (resource.status.phase != 'Running' && resource.status.phase != 'Succeeded') ||
  (
    has(resource.status.containerStatuses) &&
    resource.status.containerStatuses.exists(cs,
      has(cs.state.waiting) &&
      has(cs.state.waiting.reason) &&
      cs.state.waiting.reason == 'CrashLoopBackOff'
    )
  )
)
`

// faultQuarantineNodePredicate is the Node rule shipped in the fault-quarantine
// chart values, verbatim. Both keys are guarded with `in` because reading an
// absent key raises `no such key` rather than evaluating to false.
const faultQuarantineNodePredicate = `
!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels &&
  node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels &&
  node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")
`

func TestFieldPaths_Expressions_ExtractsExpectedPaths(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       [][]string
		wantOK     bool
	}{
		{
			name:       "node-not-ready predicate as shipped",
			expression: `resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0`,
			want:       [][]string{{"status", "conditions"}},
			wantOK:     true,
		},
		{
			name:       "gpu-operator pod predicate",
			expression: gpuOperatorPodPredicate,
			want: [][]string{
				{"metadata", "namespace"},
				{"metadata", "ownerReferences"},
				{"spec", "nodeName"},
				{"status", "containerStatuses"},
				{"status", "phase"},
				{"status", "startTime"},
			},
			wantOK: true,
		},
		{
			name:       "node association",
			expression: `resource.spec.nodeName`,
			want:       [][]string{{"spec", "nodeName"}},
			wantOK:     true,
		},
		{
			name:       "comprehension covers per-element bindings",
			expression: `resource.status.conditions.exists(c, c.type == "Ready" && c.status == "False")`,
			want:       [][]string{{"status", "conditions"}},
			wantOK:     true,
		},
		{
			name:       "computed index retains the whole subtree and the key expression",
			expression: `resource.metadata.labels[resource.spec.nodeName] == "true"`,
			want:       [][]string{{"metadata", "labels"}, {"spec", "nodeName"}},
			wantOK:     true,
		},
		{
			name:       "literal index narrows to the entry",
			expression: `resource.metadata.labels["gpu-present"] == "true"`,
			want:       [][]string{{"metadata", "labels", "gpu-present"}},
			wantOK:     true,
		},
		{
			// A key containing dots stays one segment, so it cannot be confused
			// with the nested fields metadata.labels.nvidia.com.gpu.present.
			name:       "literal index key containing dots stays a single segment",
			expression: `resource.metadata.labels["nvidia.com/gpu.present"] == "true"`,
			want:       [][]string{{"metadata", "labels", "nvidia.com/gpu.present"}},
			wantOK:     true,
		},
		{
			name:       "presence test records the tested path",
			expression: `has(resource.spec.nodeName) && resource.spec.nodeName != ""`,
			want:       [][]string{{"spec", "nodeName"}},
			wantOK:     true,
		},
		{
			name:       "lookup arguments are resource reads but its result is not",
			expression: `lookup('v1', 'Pod', resource.metadata.namespace, resource.status.podName).spec.nodeName`,
			want:       [][]string{{"metadata", "namespace"}, {"status", "podName"}},
			wantOK:     true,
		},
		{
			name:       "expression that reads nothing",
			expression: `true`,
			want:       nil,
			wantOK:     true,
		},
		{
			name:       "opaque use of the whole object fails extraction",
			expression: `size(resource) > 0`,
			wantOK:     false,
		},
		{
			name:       "iterating the object itself fails extraction",
			expression: `resource.all(k, k != "")`,
			wantOK:     false,
		},
		{
			name:       "computed index on the whole object fails extraction",
			expression: `resource[resource.kind] != null`,
			wantOK:     false,
		},
	}

	env := newTestEnv(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FieldPaths(compile(t, env, tt.expression), testResourceVar)

			require.Equal(t, tt.wantOK, ok)

			if tt.wantOK {
				require.Equal(t, tt.want, got)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

// TestFieldPaths_MembershipTest_NarrowsToTheTestedKey covers the case the
// shipped fault-quarantine rules are written in. An `in` guard that retained
// the whole map would leave every label cached, which is the bulk of what a
// node costs to hold.
func TestFieldPaths_MembershipTest_NarrowsToTheTestedKey(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		objectVar  string
		want       [][]string
		wantOK     bool
	}{
		{
			name:       "guarded read of one label",
			expression: `'k' in node.metadata.labels && node.metadata.labels['k'] == "false"`,
			objectVar:  testNodeVar,
			want:       [][]string{{"metadata", "labels", "k"}},
			wantOK:     true,
		},
		{
			name:       "fault-quarantine node predicate as shipped",
			expression: faultQuarantineNodePredicate,
			objectVar:  testNodeVar,
			want: [][]string{
				{"metadata", "labels", "k8saas.nvidia.com/ManagedByNVSentinel"},
				{"metadata", "labels", "nvsentinel.dgxc.nvidia.com/managed"},
			},
			wantOK: true,
		},
		{
			name:       "membership alone, with no read of the value",
			expression: `'nvidia.com/gpu.present' in node.metadata.labels`,
			objectVar:  testNodeVar,
			want:       [][]string{{"metadata", "labels", "nvidia.com/gpu.present"}},
			wantOK:     true,
		},
		{
			name:       "annotations are narrowed the same way",
			expression: `'quarantineHealthEvent' in node.metadata.annotations`,
			objectVar:  testNodeVar,
			want:       [][]string{{"metadata", "annotations", "quarantineHealthEvent"}},
			wantOK:     true,
		},
		{
			// The key is not known until the expression runs, so any entry
			// could be the one tested and the map is retained whole.
			name:       "computed key retains the whole map",
			expression: `node.spec.providerID in node.metadata.labels`,
			objectVar:  testNodeVar,
			want:       [][]string{{"metadata", "labels"}, {"spec", "providerID"}},
			wantOK:     true,
		},
		{
			// Membership of a list is not a key access, but recording the
			// element as a path segment is harmless: pruning only ever
			// descends into maps, so a list is carried over whole.
			name:       "membership of a list records a segment that prunes nothing",
			expression: `'Ready' in resource.status.conditions`,
			objectVar:  testResourceVar,
			want:       [][]string{{"status", "conditions", "Ready"}},
			wantOK:     true,
		},
		{
			name:       "membership on the object itself is a top-level field read",
			expression: `'spec' in resource`,
			objectVar:  testResourceVar,
			want:       [][]string{{"spec"}},
			wantOK:     true,
		},
		{
			name:       "a whole-object use elsewhere still fails extraction",
			expression: `'k' in node.metadata.labels && size(node) > 3`,
			objectVar:  testNodeVar,
			wantOK:     false,
		},
	}

	env := newTestEnv(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FieldPaths(compile(t, env, tt.expression), tt.objectVar)

			require.Equal(t, tt.wantOK, ok)

			if tt.wantOK {
				require.Equal(t, tt.want, got)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

// TestFieldPaths_MapLevelOperations_RetainTheWholeMap pins the shapes an
// operator must avoid for pruning to hold. Each of these has to report the map
// whole rather than an empty set, because retaining nothing would change what
// the expression evaluates to.
func TestFieldPaths_MapLevelOperations_RetainTheWholeMap(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       [][]string
		wantOK     bool
	}{
		{
			name:       "size of the map",
			expression: `size(node.metadata.labels) > 0`,
			want:       [][]string{{"metadata", "labels"}},
			wantOK:     true,
		},
		{
			name:       "comprehension over the map",
			expression: `node.metadata.labels.exists(k, k.startsWith("nvidia.com/"))`,
			want:       [][]string{{"metadata", "labels"}},
			wantOK:     true,
		},
		{
			name:       "key built at runtime",
			expression: `node.metadata.labels["nvidia.com/" + "gpu.present"] == "true"`,
			want:       [][]string{{"metadata", "labels"}},
			wantOK:     true,
		},
		{
			name:       "presence of the map itself",
			expression: `has(node.metadata.annotations)`,
			want:       [][]string{{"metadata", "annotations"}},
			wantOK:     true,
		},
	}

	env := newTestEnv(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FieldPaths(compile(t, env, tt.expression), testNodeVar)

			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestFieldPaths_ShadowedBinding_IgnoresLoopVariable(t *testing.T) {
	env := newTestEnv(t)

	// The iteration variable is named after the object, so the reads inside the
	// loop body are of a list element and not of the object itself.
	compiled := compile(t, env, `resource.status.conditions.exists(resource, resource.type == "Ready")`)

	got, ok := FieldPaths(compiled, testResourceVar)

	require.True(t, ok)
	require.Equal(t, [][]string{{"status", "conditions"}}, got)
}

// TestFieldPaths_ObjectVar_SelectsWhichVariableIsRead proves the walk is scoped
// to the variable the caller names. fault-quarantine binds both an event and a
// node, and only the node is cached.
func TestFieldPaths_ObjectVar_SelectsWhichVariableIsRead(t *testing.T) {
	env := newTestEnv(t)
	compiled := compile(t, env, `node.metadata.name == resource.spec.nodeName`)

	nodePaths, ok := FieldPaths(compiled, testNodeVar)
	require.True(t, ok)
	require.Equal(t, [][]string{{"metadata", "name"}}, nodePaths)

	resourcePaths, ok := FieldPaths(compiled, testResourceVar)
	require.True(t, ok)
	require.Equal(t, [][]string{{"spec", "nodeName"}}, resourcePaths)
}

// TestFieldPaths_UnboundVar_ReadsNothing guards the failure mode of a caller
// passing a variable name nothing binds: it must report an empty read, not an
// incomplete one, because the expression genuinely reads none of that object.
func TestFieldPaths_UnboundVar_ReadsNothing(t *testing.T) {
	env := newTestEnv(t)
	compiled := compile(t, env, `node.metadata.name != ""`)

	paths, ok := FieldPaths(compiled, testResourceVar)

	require.True(t, ok)
	require.Nil(t, paths)
}

func TestFieldPaths_NilAST_ReturnsIncomplete(t *testing.T) {
	_, ok := FieldPaths(nil, testResourceVar)
	require.False(t, ok)
}
