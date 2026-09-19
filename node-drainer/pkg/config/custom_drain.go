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
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// CustomDrainNodeMatcher decides which nodes custom drain owns.
// It is immutable after construction and can be shared by drain workers.
type CustomDrainNodeMatcher struct {
	enabled   bool
	scoped    bool
	selector  labels.Selector
	labelKeys []string
}

// CompileCustomDrainNodeSelector parses customDrain.nodeSelector once at startup.
// With customDrain disabled, or enabled without a selector, the matcher answers the
// same for every node and no node label has to be read.
func CompileCustomDrainNodeSelector(cfg CustomDrainConfig) (*CustomDrainNodeMatcher, error) {
	matcher := &CustomDrainNodeMatcher{enabled: cfg.Enabled}

	if !cfg.Enabled || strings.TrimSpace(cfg.NodeSelector) == "" {
		return matcher, nil
	}

	selector, err := labels.Parse(cfg.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid customDrain.nodeSelector: %w", err)
	}

	requirements, _ := selector.Requirements()
	if len(requirements) == 0 {
		return matcher, nil
	}

	keys := make(map[string]bool, len(requirements))
	for _, requirement := range requirements {
		keys[requirement.Key()] = true
	}

	matcher.scoped = true
	matcher.selector = selector

	for key := range keys {
		matcher.labelKeys = append(matcher.labelKeys, key)
	}

	sort.Strings(matcher.labelKeys)

	return matcher, nil
}

// IsScoped reports whether the decision depends on node labels. When false the caller
// must not look the node up: every node takes the same path.
func (m *CustomDrainNodeMatcher) IsScoped() bool {
	return m.scoped
}

// LabelKeys lists the only node labels the informer needs to retain.
func (m *CustomDrainNodeMatcher) LabelKeys() []string {
	return append([]string(nil), m.labelKeys...)
}

// Matches reports whether the node should be drained through the custom drain path.
func (m *CustomDrainNodeMatcher) Matches(node *v1.Node) bool {
	if !m.enabled {
		return false
	}

	if !m.scoped {
		return true
	}

	return node != nil && m.selector.Matches(labels.Set(node.Labels))
}
