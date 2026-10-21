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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPodPolicyMatcherMatch_OverlappingPolicies_SelectsFirstMatchingMode
// checks selector semantics and policy precedence.
func TestPodPolicyMatcherMatch_OverlappingPolicies_SelectsFirstMatchingMode(t *testing.T) {
	cfg, err := LoadTomlConfigFromString(`
[[podDrainPolicies]]
name = "protected-training"
namespace = "train-*"
podSelector = "app in (trainer,worker),checkpoint,!disposable"
mode = "AllowCompletion"
[[podDrainPolicies]]
name = "replaceable"
podSelector = "app=worker"
mode = "Immediate"
[[podDrainPolicies]]
name = "bounded-wait"
podSelector = "app notin (worker),deadline"
mode = "DeleteAfterTimeout"
`)
	require.NoError(t, err)
	matcher, err := CompilePodDrainPolicies(cfg.PodDrainPolicies)
	require.NoError(t, err)
	require.Equal(t, []string{"app", "checkpoint", "deadline", "disposable"}, matcher.LabelKeys())

	tests := []struct {
		name      string
		namespace string
		labels    map[string]string
		mode      EvictMode
		matched   bool
	}{
		{"first match wins over immediate", "train-a", map[string]string{"app": "worker", "checkpoint": "yes"}, ModeAllowCompletion, true},
		{"namespace restriction", "other", map[string]string{"app": "worker", "checkpoint": "yes"}, ModeImmediateEvict, true},
		{"nonexistence requirement", "train-a", map[string]string{"app": "worker", "checkpoint": "yes", "disposable": ""}, ModeImmediateEvict, true},
		{"set exclusion", "train-a", map[string]string{"app": "trainer", "deadline": "yes"}, ModeDeleteAfterTimeout, true},
		{"Kubernetes notin matches absent key", "train-a", map[string]string{"deadline": "yes"}, ModeDeleteAfterTimeout, true},
		{"unmatched stays outside scope", "train-a", map[string]string{"app": "trainer"}, "", false},
		{"unlabelled stays outside scope", "train-a", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, matched := matcher.Match(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: tt.namespace, Labels: tt.labels}})
			require.Equal(t, tt.matched, matched)
			require.Equal(t, tt.mode, mode)
		})
	}
}

// ownedBy returns a single owner reference of the given kind.
func ownedBy(kind string, controller bool) []metav1.OwnerReference {
	return []metav1.OwnerReference{{Kind: kind, Name: "owner", Controller: new(controller)}}
}

// TestPodPolicyMatcherMatch_OwnerKinds_SelectsByControllerKind
// checks owner-kind matching on its own, combined with a selector, and for pods without a controller.
func TestPodPolicyMatcherMatch_OwnerKinds_SelectsByControllerKind(t *testing.T) {
	cfg, err := LoadTomlConfigFromString(`
[[podDrainPolicies]]
name = "checkpointing-statefulsets"
podSelector = "checkpoint"
ownerKinds = ["StatefulSet"]
mode = "AllowCompletion"
[[podDrainPolicies]]
name = "training-controllers"
namespace = "train-*"
ownerKinds = ["StatefulSet", "Job"]
mode = "DeleteAfterTimeout"
[[podDrainPolicies]]
name = "replicasets"
ownerKinds = ["ReplicaSet"]
mode = "Immediate"
`)
	require.NoError(t, err)
	matcher, err := CompilePodDrainPolicies(cfg.PodDrainPolicies)
	require.NoError(t, err)
	require.Equal(t, []string{"checkpoint"}, matcher.LabelKeys())

	checkpoint := map[string]string{"checkpoint": "yes"}
	tests := []struct {
		name      string
		namespace string
		labels    map[string]string
		owners    []metav1.OwnerReference
		mode      EvictMode
		matched   bool
	}{
		{"selector and owner kind match", "other", checkpoint, ownedBy("StatefulSet", true), ModeAllowCompletion, true},
		{"owner kind alone matches", "train-a", nil, ownedBy("StatefulSet", true), ModeDeleteAfterTimeout, true},
		{"any listed kind matches", "train-a", nil, ownedBy("Job", true), ModeDeleteAfterTimeout, true},
		{"namespace restriction", "other", nil, ownedBy("Job", true), "", false},
		{"selector does not widen owner kinds", "train-a", checkpoint, ownedBy("ReplicaSet", true),
			ModeImmediateEvict, true},
		{"kind is case sensitive", "train-a", nil, ownedBy("statefulset", true), "", false},
		{"non-controller owner is ignored", "train-a", checkpoint, ownedBy("StatefulSet", false), "", false},
		{"controller among several owners", "train-a", nil,
			append(ownedBy("StatefulSet", false), ownedBy("ReplicaSet", true)...), ModeImmediateEvict, true},
		{"bare pod stays outside scope", "train-a", checkpoint, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: tt.namespace, Labels: tt.labels, OwnerReferences: tt.owners,
			}}
			mode, matched := matcher.Match(pod)
			require.Equal(t, tt.matched, matched)
			require.Equal(t, tt.mode, mode)
		})
	}
}

// TestLoadTomlConfigFromString_InvalidPodPolicies_ReturnsValidationError
// rejects malformed or conflicting policies at startup.
func TestLoadTomlConfigFromString_InvalidPodPolicies_ReturnsValidationError(t *testing.T) {
	valid := `[[podDrainPolicies]]
name = "workers"
podSelector = "app=worker"
mode = "Immediate"
`
	tests := map[string]string{
		"missing name":          strings.Replace(valid, `name = "workers"`, "", 1),
		"duplicate name":        valid + valid,
		"missing selector":      strings.Replace(valid, `podSelector = "app=worker"`, "", 1),
		"empty selector":        strings.Replace(valid, "app=worker", " ", 1),
		"invalid selector":      strings.Replace(valid, "app=worker", "app in (", 1),
		"invalid mode":          strings.Replace(valid, "Immediate", "DeleteEverything", 1),
		"empty owner kind":      strings.Replace(valid, `podSelector = "app=worker"`, `ownerKinds = [""]`, 1),
		"padded owner kind":     strings.Replace(valid, `podSelector = "app=worker"`, `ownerKinds = [" Job"]`, 1),
		"invalid namespace":     valid + "namespace = \"[\"\n",
		"custom drain conflict": valid + "[customDrain]\nenabled = true\n",
		"namespace conflict":    valid + "[[userNamespaces]]\nname = \"*\"\nmode = \"AllowCompletion\"\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadTomlConfigFromString(input)
			require.ErrorContains(t, err, "podDrainPolicies")
		})
	}
}

// TestPodPolicyMatcherLabelKeys_NamespaceOnlyConfig_ReturnsNoKeys
// keeps namespace-only deployments from retaining policy labels.
func TestPodPolicyMatcherLabelKeys_NamespaceOnlyConfig_ReturnsNoKeys(t *testing.T) {
	cfg, err := LoadTomlConfigFromString("[[userNamespaces]]\nname = \"*\"\nmode = \"AllowCompletion\"\n")
	require.NoError(t, err)
	matcher, err := CompilePodDrainPolicies(cfg.PodDrainPolicies)
	require.NoError(t, err)
	require.Empty(t, matcher.LabelKeys())
	_, matched := matcher.Match(&v1.Pod{})
	require.False(t, matched)
}
