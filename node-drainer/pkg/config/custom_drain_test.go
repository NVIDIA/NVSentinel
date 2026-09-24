// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const validCustomDrain = `[customDrain]
enabled = true
templateMountPath = "/etc/drain-template"
templateFileName = "drain-template.yaml"
namespace = "nvsentinel"
apiGroup = "nvsentinel.nvidia.com"
version = "v1alpha1"
kind = "DrainRequest"
statusConditionType = "DrainComplete"
statusConditionStatus = "True"
`

// nativeEvictionRules is what the nodes outside the selector are drained with. A scoped
// custom drain is rejected without it, so it prefixes every scoped config below.
const nativeEvictionRules = `[[userNamespaces]]
name = "*"
mode = "AllowCompletion"
`

// TestCustomDrainNodeMatcher_Selector_SplitsNodesBetweenDrainPaths
// checks selector semantics for the nodes custom drain owns.
func TestCustomDrainNodeMatcher_Selector_SplitsNodesBetweenDrainPaths(t *testing.T) {
	cfg, err := LoadTomlConfigFromString(nativeEvictionRules + validCustomDrain +
		`nodeSelector = "scheduler in (slurm,lsf),!drain.example.com/native"` + "\n")
	require.NoError(t, err)

	matcher, err := CompileCustomDrainNodeSelector(cfg.CustomDrain)
	require.NoError(t, err)
	require.True(t, matcher.IsScoped())
	require.Equal(t, []string{"drain.example.com/native", "scheduler"}, matcher.LabelKeys())

	tests := []struct {
		name   string
		labels map[string]string
		custom bool
	}{
		{"selected scheduler", map[string]string{"scheduler": "slurm"}, true},
		{"other selected scheduler", map[string]string{"scheduler": "lsf"}, true},
		{"unselected scheduler", map[string]string{"scheduler": "kueue"}, false},
		{"nonexistence requirement", map[string]string{"scheduler": "slurm", "drain.example.com/native": ""}, false},
		{"unlabelled node keeps native eviction", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: tt.labels}}
			require.Equal(t, tt.custom, matcher.Matches(node))
		})
	}
}

// TestCustomDrainNodeMatcher_NoSelector_KeepsWholeClusterOnCustomDrain
// pins the behaviour of an upgrade that does not set the new field.
func TestCustomDrainNodeMatcher_NoSelector_KeepsWholeClusterOnCustomDrain(t *testing.T) {
	for name, input := range map[string]string{
		"omitted":    validCustomDrain,
		"empty":      validCustomDrain + "nodeSelector = \"\"\n",
		"whitespace": validCustomDrain + "nodeSelector = \"  \"\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadTomlConfigFromString(input)
			require.NoError(t, err)

			matcher, err := CompileCustomDrainNodeSelector(cfg.CustomDrain)
			require.NoError(t, err)
			require.False(t, matcher.IsScoped())
			require.Empty(t, matcher.LabelKeys())
			require.True(t, matcher.Matches(&v1.Node{}))
		})
	}
}

// TestCustomDrainNodeMatcher_Disabled_KeepsWholeClusterOnNativeEviction
// keeps clusters without custom drain from reading node labels.
func TestCustomDrainNodeMatcher_Disabled_KeepsWholeClusterOnNativeEviction(t *testing.T) {
	matcher, err := CompileCustomDrainNodeSelector(CustomDrainConfig{NodeSelector: "scheduler=slurm"})
	require.NoError(t, err)
	require.False(t, matcher.IsScoped())
	require.Empty(t, matcher.LabelKeys())
	require.False(t, matcher.Matches(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"scheduler": "slurm"}},
	}))
}

// TestLoadTomlConfigFromString_ScopedCustomDrain_AcceptsNativeEvictionRules
// lets a mixed cluster configure both drain paths, which is rejected without a selector.
func TestLoadTomlConfigFromString_ScopedCustomDrain_AcceptsNativeEvictionRules(t *testing.T) {
	nativeRules := map[string]string{
		"userNamespaces":   "[[userNamespaces]]\nname = \"*\"\nmode = \"AllowCompletion\"\n",
		"podDrainPolicies": "[[podDrainPolicies]]\nname = \"workers\"\npodSelector = \"app=worker\"\nmode = \"Immediate\"\n",
	}
	for name, rules := range nativeRules {
		t.Run(name, func(t *testing.T) {
			_, err := LoadTomlConfigFromString(rules + validCustomDrain)
			require.ErrorContains(t, err, "unless customDrain.nodeSelector is set")

			cfg, err := LoadTomlConfigFromString(rules + validCustomDrain + "nodeSelector = \"scheduler=slurm\"\n")
			require.NoError(t, err)
			require.Equal(t, "scheduler=slurm", cfg.CustomDrain.NodeSelector)
		})
	}
}

// TestLoadTomlConfigFromString_ScopedCustomDrain_RequiresNativeEvictionRules
// rejects a selector that leaves the unmatched nodes with no drain rule at all. Without
// it the evaluator has no namespace and no policy to act on, so it marks those nodes
// drained and logs a success without evicting a single pod.
func TestLoadTomlConfigFromString_ScopedCustomDrain_RequiresNativeEvictionRules(t *testing.T) {
	_, err := LoadTomlConfigFromString(validCustomDrain + "nodeSelector = \"scheduler=slurm\"\n")
	require.ErrorContains(t, err, "customDrain.nodeSelector requires userNamespaces or podDrainPolicies")
}

// TestLoadTomlConfigFromString_InvalidNodeSelector_ReturnsValidationError
// rejects a malformed selector at startup rather than on the first health event.
func TestLoadTomlConfigFromString_InvalidNodeSelector_ReturnsValidationError(t *testing.T) {
	_, err := LoadTomlConfigFromString(validCustomDrain + "nodeSelector = \"scheduler in (\"\n")
	require.ErrorContains(t, err, "invalid customDrain.nodeSelector")
}
