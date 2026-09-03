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
package cel

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

func TestResourceFieldPaths_Expressions_ExtractsExpectedPaths(t *testing.T) {
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

	env, err := NewCompilerEnvironment()
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled, err := env.Compile(tt.expression)
			require.NoError(t, err)

			got, ok := ResourceFieldPaths(compiled)

			require.Equal(t, tt.wantOK, ok)

			if tt.wantOK {
				require.Equal(t, tt.want, got)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

func TestResourceFieldPaths_ShadowedResourceBinding_IgnoresLoopVariable(t *testing.T) {
	env, err := NewCompilerEnvironment()
	require.NoError(t, err)

	// The iteration variable is named after the object, so the reads inside the
	// loop body are of a list element and not of the object itself.
	compiled, err := env.Compile(`resource.status.conditions.exists(resource, resource.type == "Ready")`)
	require.NoError(t, err)

	got, ok := ResourceFieldPaths(compiled)

	require.True(t, ok)
	require.Equal(t, [][]string{{"status", "conditions"}}, got)
}

func TestResourceFieldPaths_NilAST_ReturnsIncomplete(t *testing.T) {
	_, ok := ResourceFieldPaths(nil)
	require.False(t, ok)
}
