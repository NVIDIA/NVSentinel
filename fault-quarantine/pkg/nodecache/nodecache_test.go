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

package nodecache

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	cordonlabels "github.com/nvidia/nvsentinel/commons/pkg/labels"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

const (
	testGPULabelKey   = "nvidia.com/gpu.present"
	testGPULabelValue = "true"
	testLabelPrefix   = "k8saas.nvidia.com/"

	managedByKey = "k8saas.nvidia.com/ManagedByNVSentinel"
	managedKey   = "nvsentinel.dgxc.nvidia.com/managed"
)

// shippedPredicate is the Node rule in the fault-quarantine chart values.
const shippedPredicate = `
!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels &&
  node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels &&
  node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")
`

func testOperational() Operational {
	return Operational{GPUNodeLabelKey: testGPULabelKey}
}

// nodeRuleConfig returns a config carrying one Node rule with expression.
func nodeRuleConfig(expression string) config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: testLabelPrefix,
		RuleSets: []config.QuarantineRuleSet{{
			RuleSetMeta: config.RuleSetMeta{
				Enabled: true,
				Name:    "test",
				Match: config.Match{
					All: []config.Rule{{Kind: nodeRuleKind, Expression: expression}},
				},
			},
		}},
	}
}

// transformLabels runs the transform over a node carrying labels and returns
// what the cache would hold.
func transformLabels(t *testing.T, keys Keys, labels map[string]string) map[string]string {
	t.Helper()

	node := &v1.Node{Name: "test-node", Labels: labels}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	return cached.Labels
}

func TestDerive_ShippedPredicate_RetainsOnlyTheGuardedKeys(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	cached := transformLabels(t, keys, map[string]string{
		managedByKey:                  "false",
		managedKey:                    "true",
		testGPULabelKey:               testGPULabelValue,
		"nvidia.com/gpu.count":        "8",
		"kubernetes.io/arch":          "amd64",
		"topology.kubernetes.io/zone": "us-east-1a",
	})

	require.Equal(t, map[string]string{
		managedByKey:    "false",
		managedKey:      "true",
		testGPULabelKey: testGPULabelValue,
	}, cached, "only the guarded keys and the breaker's selector key survive")
}

// TestDerive_RemovableKeys_SurvivePruning is the guard against the quietest way
// this change could break a live cluster.
//
// A node write is a merge-patch diff against the cached object. delete() on a
// key the cache dropped is a no-op, so the diff is empty and no null is
// emitted: the label stays on the live node while the log line says it was
// removed. Every key fault-quarantine can remove therefore has to be retained,
// even the ones nothing ever reads.
func TestDerive_RemovableKeys_SurvivePruning(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	removable := map[string]string{
		statemanager.NVSentinelStateLabelKey: string(statemanager.QuarantinedLabelValue),
	}

	for _, suffix := range []string{
		cordonlabels.CordonedBySuffix,
		cordonlabels.CordonedReasonSuffix,
		cordonlabels.CordonedTimestampSuffix,
		cordonlabels.UncordonedBySuffix,
		cordonlabels.UncordonedReasonSuffix,
		cordonlabels.UncordonedTimestampSuffix,
	} {
		removable[cordonlabels.Key(testLabelPrefix, suffix)] = "set"
	}

	cached := transformLabels(t, keys, removable)

	require.Equal(t, removable, cached)
}

// TestDerive_QuarantineAnnotations_SurvivePruning covers the read side of the
// same hazard. getNodeQuarantineAnnotations reads all eight off the cached
// node; losing one makes fault-quarantine conclude no quarantine is in
// progress and re-cordon a node it already handled.
func TestDerive_QuarantineAnnotations_SurvivePruning(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	annotations := make(map[string]string, len(common.QuarantineAnnotationKeys)+1)
	for _, key := range common.QuarantineAnnotationKeys {
		annotations[key] = "set"
	}

	annotations["unrelated.example.com/annotation"] = "dropped"

	node := &v1.Node{Name: "test-node", Annotations: annotations}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	for _, key := range common.QuarantineAnnotationKeys {
		require.Contains(t, cached.Annotations, key)
	}

	require.NotContains(t, cached.Annotations, "unrelated.example.com/annotation")
}

// TestDerive_UnprunableExpressions_RetainTheWholeMap pins the fail-safe
// direction. Every shipped rule is a negative opt-out, so a rule that loses its
// key flips to true and fault-quarantine cordons a node its owner excluded.
// Any shape the extractor does not understand must therefore retain everything.
func TestDerive_UnprunableExpressions_RetainTheWholeMap(t *testing.T) {
	tests := []struct {
		name       string
		expression string
	}{
		{
			name:       "size of the label map",
			expression: `size(node.metadata.labels) > 0`,
		},
		{
			name:       "comprehension over the label map",
			expression: `node.metadata.labels.exists(k, k.startsWith("nvidia.com/"))`,
		},
		{
			name:       "key built at runtime",
			expression: `node.metadata.labels["nvidia.com/" + "gpu.present"] == "true"`,
		},
		{
			name:       "computed key",
			expression: `node.metadata.labels[node.spec.providerID] == "true"`,
		},
		{
			name:       "the whole node",
			expression: `size(node) > 0`,
		},
		{
			name:       "the whole of metadata",
			expression: `has(node.metadata)`,
		},
		{
			name:       "an expression that does not parse",
			expression: `node.metadata.labels[`,
		},
	}

	unread := map[string]string{
		"kubernetes.io/arch":   "amd64",
		"nvidia.com/gpu.count": "8",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := Derive(nodeRuleConfig(tt.expression), testOperational())

			cached := transformLabels(t, keys, unread)

			require.Equal(t, unread, cached, "an underivable rule must retain every label")
		})
	}
}

// TestDerive_OneUnprunableRule_DisablesPruningAcrossTheRuleset covers the
// interaction between rules. Pruning is a property of the cache, not of one
// rule, so a single map-level read anywhere in the ruleset has to retain the
// map for every rule.
func TestDerive_OneUnprunableRule_DisablesPruningAcrossTheRuleset(t *testing.T) {
	cfg := nodeRuleConfig(shippedPredicate)
	cfg.RuleSets = append(cfg.RuleSets, config.QuarantineRuleSet{
		RuleSetMeta: config.RuleSetMeta{
			Enabled: true,
			Name:    "map-level",
			Match: config.Match{
				Any: []config.Rule{{
					Kind:       nodeRuleKind,
					Expression: `size(node.metadata.labels) > 3`,
				}},
			},
		},
	})

	keys := Derive(cfg, testOperational())

	unread := map[string]string{"kubernetes.io/arch": "amd64"}

	require.Equal(t, unread, transformLabels(t, keys, unread))
}

// TestDerive_DisabledRuleSet_IsStillWalked records a deliberate choice to
// over-retain. The evaluator skips disabled rule sets, so walking them costs a
// few map entries; not walking them would fail open if that skip ever changed.
func TestDerive_DisabledRuleSet_IsStillWalked(t *testing.T) {
	cfg := nodeRuleConfig(shippedPredicate)
	cfg.RuleSets[0].Enabled = false

	keys := Derive(cfg, testOperational())

	cached := transformLabels(t, keys, map[string]string{
		managedByKey:           "false",
		"nvidia.com/gpu.count": "8",
	})

	require.Equal(t, map[string]string{managedByKey: "false"}, cached)
}

// TestDerive_ValidationRuleSets_AreWalked covers the second place a Node rule
// can be configured.
func TestDerive_ValidationRuleSets_AreWalked(t *testing.T) {
	cfg := config.TomlConfig{
		LabelPrefix: testLabelPrefix,
		Validation: config.ValidationConfig{
			Enabled: true,
			RuleSets: []config.ValidationRuleSet{{
				RuleSetMeta: config.RuleSetMeta{
					Enabled: true,
					Name:    "validation",
					Match: config.Match{
						All: []config.Rule{{
							Kind:       nodeRuleKind,
							Expression: `'validate-me' in node.metadata.labels`,
						}},
					},
				},
			}},
		},
	}

	keys := Derive(cfg, testOperational())

	cached := transformLabels(t, keys, map[string]string{
		"validate-me":          "yes",
		"nvidia.com/gpu.count": "8",
	})

	require.Equal(t, map[string]string{"validate-me": "yes"}, cached)
}

// TestDerive_RuleSetLabelKeys_SurvivePruning covers the labels a rule set
// applies on quarantine, which cleanup later removes.
func TestDerive_RuleSetLabelKeys_SurvivePruning(t *testing.T) {
	cfg := nodeRuleConfig(shippedPredicate)
	cfg.RuleSets[0].Label = config.Label{Key: "example.com/quarantined-by", Value: "nvsentinel"}

	keys := Derive(cfg, testOperational())

	cached := transformLabels(t, keys, map[string]string{
		"example.com/quarantined-by": "nvsentinel",
		"nvidia.com/gpu.count":       "8",
	})

	require.Equal(t, map[string]string{"example.com/quarantined-by": "nvsentinel"}, cached)
}

// TestTransform_AppliedLabelsAnnotation_RetainsItsKeys covers the keys that are
// not knowable when the retained set is derived. They are operator-chosen and
// recorded on the node, and cleanup removes them, so they are read per object.
func TestTransform_AppliedLabelsAnnotation_RetainsItsKeys(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	applied, err := json.Marshal([]config.AppliedLabel{
		{Key: "example.com/session-label", Value: "on"},
		{Key: "example.com/other-label", Value: "on"},
	})
	require.NoError(t, err)

	node := &v1.Node{
		Name: "test-node",
		Labels: map[string]string{
			"example.com/session-label": "on",
			"example.com/other-label":   "on",
			"nvidia.com/gpu.count":      "8",
		},
		Annotations: map[string]string{
			common.QuarantineHealthEventAppliedLabelsAnnotationKey: string(applied),
		},
	}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	require.Equal(t, map[string]string{
		"example.com/session-label": "on",
		"example.com/other-label":   "on",
	}, cached.Labels)
}

// TestTransform_UnparsableAppliedLabels_RetainsEveryLabel keeps the failure of
// one node's annotation from silently stranding its session labels.
func TestTransform_UnparsableAppliedLabels_RetainsEveryLabel(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	labels := map[string]string{
		"example.com/session-label": "on",
		"nvidia.com/gpu.count":      "8",
	}

	node := &v1.Node{
		Name:   "test-node",
		Labels: labels,
		Annotations: map[string]string{
			common.QuarantineHealthEventAppliedLabelsAnnotationKey: "{not json",
		},
	}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	require.Equal(t, labels, cached.Labels)
}

func TestTransform_NilMaps_StayNil(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	node := &v1.Node{Name: "test-node"}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	require.Nil(t, cached.Labels)
	require.Nil(t, cached.Annotations)
}

func TestTransform_ClearsStatus(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	node := &v1.Node{
		Name: "test-node",
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
	}

	transformed, err := keys.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	require.Equal(t, v1.NodeStatus{}, cached.Status)
}

func TestTransform_NonNodeObject_ReturnsError(t *testing.T) {
	keys := Derive(nodeRuleConfig(shippedPredicate), testOperational())

	_, err := keys.Transform()(&v1.Pod{Name: "not-a-node"})

	require.Error(t, err)
}

// TestZeroKeys_Transform_PrunesNothing pins the safety of the zero value. A
// caller that has not derived a set must hold whole objects.
func TestZeroKeys_Transform_PrunesNothing(t *testing.T) {
	labels := map[string]string{"kubernetes.io/arch": "amd64"}
	annotations := map[string]string{"unrelated": "kept"}

	node := &v1.Node{Name: "test-node", Labels: labels, Annotations: annotations}

	transformed, err := Keys{}.Transform()(node)
	require.NoError(t, err)

	cached, ok := transformed.(*v1.Node)
	require.True(t, ok)

	require.Equal(t, labels, cached.Labels)
	require.Equal(t, annotations, cached.Annotations)
	require.Equal(t, v1.NodeStatus{}, cached.Status)
}
