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
	"k8s.io/apimachinery/pkg/labels"
)

// PodDrainPolicy overrides the namespace drain mode for matching pods.
// Policies are evaluated in order; the first match wins.
type PodDrainPolicy struct {
	Name        string    `toml:"name"`
	Namespace   string    `toml:"namespace"`
	PodSelector string    `toml:"podSelector"`
	Mode        EvictMode `toml:"mode"`
}

type compiledPodDrainPolicy struct {
	namespace string
	selector  labels.Selector
	mode      EvictMode
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

// compilePodDrainPolicy validates the mode and namespace glob and parses the pod selector.
// An omitted namespace matches all namespaces.
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

	if strings.TrimSpace(policy.PodSelector) == "" {
		return compiledPodDrainPolicy{}, fmt.Errorf("podSelector must not be empty; use userNamespaces for a fallback")
	}

	selector, err := labels.Parse(policy.PodSelector)
	if err != nil {
		return compiledPodDrainPolicy{}, fmt.Errorf("invalid podSelector: %w", err)
	}

	return compiledPodDrainPolicy{namespace: namespace, selector: selector, mode: policy.Mode}, nil
}

// LabelKeys lists the only pod labels the informer needs to retain.
func (m *PodPolicyMatcher) LabelKeys() []string {
	return append([]string(nil), m.labelKeys...)
}

// Match returns the first matching policy's mode, or false for namespace fallback.
func (m *PodPolicyMatcher) Match(pod *v1.Pod) (EvictMode, bool) {
	for _, policy := range m.policies {
		namespaceMatches, _ := filepath.Match(policy.namespace, pod.Namespace)
		if namespaceMatches && policy.selector.Matches(labels.Set(pod.Labels)) {
			return policy.mode, true
		}
	}

	return "", false
}
