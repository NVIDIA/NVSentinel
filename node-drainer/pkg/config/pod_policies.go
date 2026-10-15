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
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// PodDrainPolicy selects the drain mode for matching pods.
// Policies are evaluated in order; the first match wins.
// OwnerKinds matches the kind of the pod's controller owner reference.
type PodDrainPolicy struct {
	Name        string    `toml:"name"`
	Namespace   string    `toml:"namespace"`
	PodSelector string    `toml:"podSelector"`
	OwnerKinds  []string  `toml:"ownerKinds"`
	Mode        EvictMode `toml:"mode"`
}

type compiledPodDrainPolicy struct {
	namespace  string
	selector   labels.Selector
	ownerKinds map[string]bool
	mode       EvictMode
}

// PodPolicyMatcher is immutable after construction and can be shared by drain workers.
type PodPolicyMatcher struct {
	policies  []compiledPodDrainPolicy
	labelKeys []string
}

// CompilePodDrainPolicies validates the policies and parses selectors once at startup.
func CompilePodDrainPolicies(policies []PodDrainPolicy) (*PodPolicyMatcher, error) {
	matcher := &PodPolicyMatcher{}
	names := make(map[string]bool)
	keys := make(map[string]bool)

	for index, policy := range policies {
		if strings.TrimSpace(policy.Name) == "" || names[policy.Name] {
			return nil, fmt.Errorf("podDrainPolicies[%d]: name must be non-empty and unique", index)
		}

		names[policy.Name] = true

		compiled, err := compilePodDrainPolicy(policy)
		if err != nil {
			return nil, fmt.Errorf("podDrainPolicies[%d]: %w", index, err)
		}

		selector := compiled.selector

		requirements, _ := selector.Requirements()
		for _, requirement := range requirements {
			keys[requirement.Key()] = true
		}

		matcher.policies = append(matcher.policies, compiled)
	}

	for key := range keys {
		matcher.labelKeys = append(matcher.labelKeys, key)
	}

	sort.Strings(matcher.labelKeys)

	return matcher, nil
}

// compilePodDrainPolicy validates the mode and namespace glob, parses the pod selector and
// collects the owner kinds. An omitted namespace matches all namespaces. A policy needs a
// podSelector, ownerKinds or both, so that no policy selects every pod by accident.
func compilePodDrainPolicy(policy PodDrainPolicy) (compiledPodDrainPolicy, error) {
	switch policy.Mode {
	case ModeImmediateEvict, ModeAllowCompletion, ModeDeleteAfterTimeout:
	default:
		return compiledPodDrainPolicy{}, fmt.Errorf("unsupported mode %q", policy.Mode)
	}

	namespace := policy.Namespace
	if namespace == "" {
		namespace = "*"
	}

	if _, err := filepath.Match(namespace, ""); err != nil {
		return compiledPodDrainPolicy{}, fmt.Errorf("invalid namespace pattern: %w", err)
	}

	ownerKinds, err := compileOwnerKinds(policy.OwnerKinds)
	if err != nil {
		return compiledPodDrainPolicy{}, err
	}

	if strings.TrimSpace(policy.PodSelector) == "" {
		if ownerKinds == nil {
			return compiledPodDrainPolicy{}, fmt.Errorf("podSelector and ownerKinds must not both be empty")
		}

		return compiledPodDrainPolicy{
			namespace: namespace, selector: labels.Everything(), ownerKinds: ownerKinds, mode: policy.Mode,
		}, nil
	}

	selector, err := labels.Parse(policy.PodSelector)
	if err != nil {
		return compiledPodDrainPolicy{}, fmt.Errorf("invalid podSelector: %w", err)
	}

	return compiledPodDrainPolicy{
		namespace: namespace, selector: selector, ownerKinds: ownerKinds, mode: policy.Mode,
	}, nil
}

// compileOwnerKinds returns the set of owner kinds, or nil when the policy does not match on owners.
// Kinds are compared exactly, as Kubernetes stores them in owner references (e.g. "StatefulSet").
func compileOwnerKinds(kinds []string) (map[string]bool, error) {
	if len(kinds) == 0 {
		return nil, nil
	}

	ownerKinds := make(map[string]bool, len(kinds))

	for index, kind := range kinds {
		if kind == "" || strings.TrimSpace(kind) != kind {
			return nil, fmt.Errorf("ownerKinds[%d]: invalid kind %q", index, kind)
		}

		ownerKinds[kind] = true
	}

	return ownerKinds, nil
}

// LabelKeys lists the only pod labels the informer needs to retain.
func (m *PodPolicyMatcher) LabelKeys() []string {
	return append([]string(nil), m.labelKeys...)
}

// Match returns the first matching policy's mode, or false for pods outside the drain scope.
func (m *PodPolicyMatcher) Match(pod *v1.Pod) (EvictMode, bool) {
	for _, policy := range m.policies {
		namespaceMatches, _ := filepath.Match(policy.namespace, pod.Namespace)
		if namespaceMatches && policy.selector.Matches(labels.Set(pod.Labels)) && policy.matchesOwner(pod) {
			return policy.mode, true
		}
	}

	return "", false
}

// matchesOwner reports whether the pod's controller has one of the policy's owner kinds.
// Pods without a controller, such as bare pods, never match a policy that sets ownerKinds.
func (p compiledPodDrainPolicy) matchesOwner(pod *v1.Pod) bool {
	if p.ownerKinds == nil {
		return true
	}

	controller := metav1.GetControllerOfNoCopy(pod)

	return controller != nil && p.ownerKinds[controller.Kind]
}
