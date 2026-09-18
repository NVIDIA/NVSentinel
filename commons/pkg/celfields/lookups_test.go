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

// chainedLookupExpression is the chained lookup() documented in the
// kubernetes-object-monitor chart values, reaching a node through a pod.
const chainedLookupExpression = `
lookup('v1', 'Node', '',
  lookup('v1', 'Pod', resource.metadata.namespace, resource.status.podName).spec.nodeName
).status.conditions.exists(c, c.type == 'Ready' && c.status == 'False')
`

func TestLookupTargets_Expressions_ExtractsGVKsAndPaths(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       []LookupTarget
	}{
		{
			name:       "literal GVK with a field read off the result",
			expression: `lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName != ''`,
			want: []LookupTarget{
				{APIVersion: "v1", Kind: "Pod", Paths: [][]string{{"spec", "nodeName"}}, Derivable: true},
			},
		},
		{
			name:       "group in the apiVersion",
			expression: `lookup('apps/v1', 'DaemonSet', 'gpu-operator', 'nvidia-driver').status.numberUnavailable > 0`,
			want: []LookupTarget{
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Paths:      [][]string{{"status", "numberUnavailable"}},
					Derivable:  true,
				},
			},
		},
		{
			name:       "chained lookups report both GVKs",
			expression: chainedLookupExpression,
			want: []LookupTarget{
				{APIVersion: "v1", Kind: "Node", Paths: [][]string{{"status", "conditions"}}, Derivable: true},
				{APIVersion: "v1", Kind: "Pod", Paths: [][]string{{"spec", "nodeName"}}, Derivable: true},
			},
		},
		{
			name: "reads of one GVK are merged across calls",
			expression: `lookup('v1', 'Pod', 'ns', 'a').spec.nodeName == ` +
				`lookup('v1', 'Pod', 'ns', 'b').spec.nodeName && ` +
				`lookup('v1', 'Pod', 'ns', 'a').status.phase == 'Running'`,
			want: []LookupTarget{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Paths:      [][]string{{"spec", "nodeName"}, {"status", "phase"}},
					Derivable:  true,
				},
			},
		},
		{
			name:       "a literal index key is a path segment",
			expression: `lookup('v1', 'Node', '', 'node-a').metadata.labels['nvidia.com/gpu.present'] == 'true'`,
			want: []LookupTarget{
				{
					APIVersion: "v1",
					Kind:       "Node",
					Paths:      [][]string{{"metadata", "labels", "nvidia.com/gpu.present"}},
					Derivable:  true,
				},
			},
		},
		{
			name:       "a membership test narrows a looked-up map the same way",
			expression: `'nvidia.com/gpu.present' in lookup('v1', 'Node', '', 'node-a').metadata.labels`,
			want: []LookupTarget{
				{
					APIVersion: "v1",
					Kind:       "Node",
					Paths:      [][]string{{"metadata", "labels", "nvidia.com/gpu.present"}},
					Derivable:  true,
				},
			},
		},
		{
			name:       "a computed index keeps the parent subtree",
			expression: `lookup('v1', 'Node', '', 'node-a').metadata.labels[resource.spec.nodeName] == 'true'`,
			want: []LookupTarget{
				{
					APIVersion: "v1",
					Kind:       "Node",
					Paths:      [][]string{{"metadata", "labels"}},
					Derivable:  true,
				},
			},
		},
		{
			name:       "result used as a whole is not derivable",
			expression: `lookup('v1', 'Pod', 'ns', 'a') != null`,
			want: []LookupTarget{
				{APIVersion: "v1", Kind: "Pod", Derivable: false},
			},
		},
		{
			name: "one whole-object use makes the GVK underivable",
			expression: `lookup('v1', 'Pod', 'ns', 'a').spec.nodeName != '' && ` +
				`size(lookup('v1', 'Pod', 'ns', 'a')) > 3`,
			want: []LookupTarget{
				{APIVersion: "v1", Kind: "Pod", Derivable: false},
			},
		},
		{
			name: "a whole-object use of the watched resource makes every GVK underivable",
			expression: `lookup('v1', 'Pod', 'ns', 'a').spec.nodeName != '' && ` +
				`size(resource) > 3 && ` +
				`lookup('v1', 'Pod', 'ns', 'a').status.phase == 'Running'`,
			want: []LookupTarget{
				{APIVersion: "v1", Kind: "Pod", Derivable: false},
			},
		},
		{
			name:       "computed apiVersion cannot be named",
			expression: `lookup(resource.apiVersion, 'Pod', 'ns', 'a').spec.nodeName != ''`,
			want:       nil,
		},
		{
			name:       "computed kind cannot be named",
			expression: `lookup('v1', resource.kind, 'ns', 'a').spec.nodeName != ''`,
			want:       nil,
		},
		{
			name:       "no lookup at all",
			expression: `resource.status.phase != 'Running'`,
			want:       nil,
		},
	}

	env := newTestEnv(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled := compile(t, env, tt.expression)

			require.Equal(t, tt.want, LookupTargets(compiled, testResourceVar, testLookupFunc))
		})
	}
}

func TestLookupTargets_LookupArguments_StillReadTheResource(t *testing.T) {
	env := newTestEnv(t)

	// The namespace and name handed to lookup() are read off the watched
	// object, so they have to survive pruning of the watched object too.
	compiled := compile(t, env, chainedLookupExpression)

	paths, ok := FieldPaths(compiled, testResourceVar)

	require.True(t, ok)
	require.Equal(t, [][]string{{"metadata", "namespace"}, {"status", "podName"}}, paths)
}

// TestLookupTargets_WalkStoppedEarly_DropsThePathsGatheredSoFar covers the
// worst way this could go wrong. The walk stops where the watched object is
// used as a whole, so a lookup() past that point is never seen — and a lookup()
// before it names the same GVK. Reporting the fields gathered so far as all
// that GVK is read for would prune the rest from its cache entry, and the call
// past the stop would read them as absent rather than as what they hold.
func TestLookupTargets_WalkStoppedEarly_DropsThePathsGatheredSoFar(t *testing.T) {
	env := newTestEnv(t)

	compiled := compile(t, env,
		`lookup('v1', 'Pod', 'ns', 'a').spec.nodeName != '' && size(resource) > 3 && `+
			`lookup('v1', 'Pod', 'ns', 'a').status.phase == 'Running'`)

	_, ok := FieldPaths(compiled, testResourceVar)
	require.False(t, ok, "the watched object is used as a whole, so its fields are underivable")

	targets := LookupTargets(compiled, testResourceVar, testLookupFunc)

	require.Len(t, targets, 1)
	require.False(t, targets[0].Derivable)
	require.Nil(t, targets[0].Paths)
}

// TestLookupTargets_NoLookupFunc_ReturnsNothing covers a caller whose
// environment declares no lookup function, such as fault-quarantine. Every
// call then ends a chain like any other, and none roots one the cache serves.
func TestLookupTargets_NoLookupFunc_ReturnsNothing(t *testing.T) {
	env := newTestEnv(t)
	compiled := compile(t, env, `lookup('v1', 'Pod', 'ns', 'a').spec.nodeName != ''`)

	require.Nil(t, LookupTargets(compiled, testResourceVar, ""))
}

func TestLookupTargets_NilAST_ReturnsNothing(t *testing.T) {
	require.Nil(t, LookupTargets(nil, testResourceVar, testLookupFunc))
}
