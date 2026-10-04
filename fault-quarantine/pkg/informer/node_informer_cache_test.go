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

package informer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
)

func TestNodeCacheTransform_RetainAll_StripsOnlyStatus(t *testing.T) {
	input := testFullNode()
	wantMetadata := input.ObjectMeta.DeepCopy()
	wantSpec := input.Spec.DeepCopy()

	// The zero value retains every key, so this is the status-only transform
	// the informer used before the retained set was derived from the rules.
	transformed, err := nodecache.Keys{}.Transform()(input)
	require.NoError(t, err)

	node, ok := transformed.(*v1.Node)
	require.True(t, ok, "Transform() returned %T", transformed)

	require.Same(t, input, node, "Transform() returned a copy instead of mutating in place")
	require.Equal(t, *wantMetadata, node.ObjectMeta, "cached node metadata changed")
	require.Equal(t, *wantSpec, node.Spec, "cached node spec changed")
	require.Equal(t, v1.NodeStatus{}, node.Status, "cached node retained status")
}

func TestNewNodeInformer_DerivedKeys_CachesOnlyRetainedEntries(t *testing.T) {
	client := fake.NewClientset(testFullNode())

	retained := nodecache.Derive(testRuleConfig(), nodecache.Operational{
		GPUNodeLabelKey: GPUNodeLabel,
	})

	nodeInformer, err := NewNodeInformer(client, 0, GPUNodeLabel, GPUNodeLabelValue, retained)
	require.NoError(t, err)

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })

	require.NoError(t, nodeInformer.Run(stopCh))

	node, err := nodeInformer.GetNode("test-node")
	require.NoError(t, err)

	require.Equal(t, v1.NodeStatus{}, node.Status, "cached node retained status")

	// The rule reads this key, so it survives.
	require.Equal(t, "false", node.Labels["opt-out"], "cached node dropped a label the rules read")

	// The circuit breaker selects on this key, so it survives even though no
	// rule mentions it.
	require.Equal(t, GPUNodeLabelValue, node.Labels[GPUNodeLabel],
		"cached node dropped the GPU label the breaker selects on")

	// Nothing reads this one.
	require.NotContains(t, node.Labels, "label", "cached node retained a label nothing reads")
	require.NotContains(t, node.Annotations, "annotation", "cached node retained an annotation nothing reads")

	// Spec is never pruned: the cordon path and untaint detection read it.
	require.Equal(t, "10.0.0.0/24", node.Spec.PodCIDR, "cached node is missing a spec field")
	require.True(t, node.Spec.Unschedulable, "cached node is missing a spec field")
}

// testRuleConfig is a ruleset of the shape the chart ships: one Node rule that
// guards a label read with `in`, so that an operator can opt a node out.
func testRuleConfig() config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: "k8saas.nvidia.com/",
		RuleSets: []config.QuarantineRuleSet{{
			RuleSetMeta: config.RuleSetMeta{
				Enabled: true,
				Name:    "test",
				Match: config.Match{
					All: []config.Rule{{
						Kind:       "Node",
						Expression: `!('opt-out' in node.metadata.labels && node.metadata.labels['opt-out'] == "false")`,
					}},
				},
			},
		}},
	}
}

func BenchmarkNodeCacheTransform(b *testing.B) {
	retained := nodecache.Derive(testRuleConfig(), nodecache.Operational{
		GPUNodeLabelKey: GPUNodeLabel,
	})
	transform := retained.Transform()

	fullJSON, err := json.Marshal(testFullNode())
	require.NoError(b, err)

	transformed, err := transform(testFullNode())
	require.NoError(b, err)

	slimJSON, err := json.Marshal(transformed)
	require.NoError(b, err)

	// The transform replaces these three fields rather than mutating what they
	// point at, so holding the originals is enough to hand it a whole node
	// again. Restoring them costs three assignments inside the measurement,
	// which is far less than stopping the timer would, and without it every
	// iteration after the first would prune an already pruned node.
	node := testFullNode()
	fullStatus, fullLabels, fullAnnotations := node.Status, node.Labels, node.Annotations

	var transformErr error

	b.ResetTimer()

	for b.Loop() {
		node.Status, node.Labels, node.Annotations = fullStatus, fullLabels, fullAnnotations

		_, transformErr = transform(node)
	}

	b.StopTimer()
	require.NoError(b, transformErr)

	b.ReportMetric(float64(len(fullJSON)), "full-json-bytes")
	b.ReportMetric(float64(len(slimJSON)), "cached-json-bytes")
}

func testFullNode() *v1.Node {
	return &v1.Node{
		APIVersion: "v1", Kind: "Node",
		Name:            "test-node",
		UID:             types.UID("test-uid"),
		ResourceVersion: "42",
		Labels: map[string]string{
			"label":      "value",
			"opt-out":    "false",
			GPUNodeLabel: GPUNodeLabelValue,
		},
		Annotations:     map[string]string{"annotation": "value"},
		OwnerReferences: []metav1.OwnerReference{{Name: "owner"}},
		ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "manager"}},
		Spec: v1.NodeSpec{
			Unschedulable: true,
			Taints: []v1.Taint{{
				Key:       "key",
				Value:     "value",
				Effect:    v1.TaintEffectNoSchedule,
				TimeAdded: &metav1.Time{Time: time.Unix(1, 0)},
			}},
			PodCIDR: "10.0.0.0/24",
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{v1.ResourceCPU: resource.MustParse("8")},
			Conditions: []v1.NodeCondition{{
				Type:   v1.NodeReady,
				Status: v1.ConditionTrue,
			}},
		},
	}
}
