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

package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

func TestEvaluate(t *testing.T) {
	pod := func(fields map[string]any) *unstructured.Unstructured {
		obj := map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "p1", "namespace": "default"},
		}
		for k, v := range fields {
			obj[k] = v
		}

		return &unstructured.Unstructured{Object: obj}
	}

	tests := []struct {
		name      string
		policy    config.Policy
		resource  *unstructured.Unstructured
		wantMatch bool
		wantNode  string
		wantErr   error
	}{
		{
			name: "node association",
			policy: config.Policy{
				Predicate:       config.PredicateSpec{Expression: `resource.status.phase == "Failed"`},
				NodeAssociation: &config.AssociationSpec{Expression: "resource.spec.nodeName"},
			},
			resource: pod(map[string]any{
				"spec":   map[string]any{"nodeName": "node-a"},
				"status": map[string]any{"phase": "Failed"},
			}),
			wantMatch: true,
			wantNode:  "node-a",
		},
		{
			name:      "no node association falls back to the resource name",
			policy:    config.Policy{Predicate: config.PredicateSpec{Expression: "false"}},
			resource:  pod(nil),
			wantMatch: false,
			wantNode:  "p1",
		},
		{
			name: "a Node is its own node",
			policy: config.Policy{
				Predicate:       config.PredicateSpec{Expression: "true"},
				NodeAssociation: &config.AssociationSpec{Expression: `"ignored"`},
			},
			resource: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node-b"},
			}},
			wantMatch: true,
			wantNode:  "node-b",
		},
		{
			name:     "predicate error",
			policy:   config.Policy{Predicate: config.PredicateSpec{Expression: "resource.spec.missing == 1"}},
			resource: pod(nil),
			wantErr:  ErrPredicate,
		},
		{
			name: "node association error",
			policy: config.Policy{
				Predicate:       config.PredicateSpec{Expression: "true"},
				NodeAssociation: &config.AssociationSpec{Expression: "resource.spec.missing"},
			},
			resource: pod(nil),
			wantErr:  ErrNodeAssociation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.policy.Name = "p"
			tt.policy.Enabled = true
			tt.policy.Resource = config.ResourceSpec{Version: "v1", Kind: tt.resource.GetKind()}

			env, err := celenv.NewEnvironment(fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())
			require.NoError(t, err)

			evaluator, err := NewEvaluator(env, []config.Policy{tt.policy})
			require.NoError(t, err)

			matched, node, err := evaluator.Evaluate(context.Background(), "p", tt.resource)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantMatch, matched)
			assert.Equal(t, tt.wantNode, node)
		})
	}
}
