// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"

	"github.com/nvidia/nvsentinel/commons/pkg/celevent"
	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type HealthEventsAnalyzerRule struct {
	Name              string   `toml:"name"`
	Description       string   `toml:"description"`
	Stage             []string `toml:"stage"`
	RecommendedAction string   `toml:"recommended_action"`
	Message           string   `toml:"message"`
	EvaluateRule      bool     `toml:"evaluate_rule"`
	// Optional: override the module-level processing strategy for events published by this rule.
	ProcessingStrategy string `toml:"processing_strategy"`
	// Optional: a CEL expression over the incoming health event. The analyzer runs the rule's
	// stages only for events on which it is true, so it must be true for every event the stages
	// can match. See commons/pkg/celevent for the fields it can read.
	When string `toml:"when"`

	when *celevent.Filter
}

// Applies reports whether the rule's stages must run for event. A rule without a when
// expression always applies.
//
// If the expression cannot be evaluated, or was never compiled, Applies returns true with the
// error. The when expression only saves a query that cannot match, so a failure must fall back
// to running the stages, which remain the only judge of a match.
func (r HealthEventsAnalyzerRule) Applies(event *protos.HealthEvent) (bool, error) {
	if r.when == nil {
		if strings.TrimSpace(r.When) != "" {
			return true, fmt.Errorf("when expression of rule %q is not compiled", r.Name)
		}

		return true, nil
	}

	applies, err := r.when.Matches(event)
	if err != nil {
		return true, fmt.Errorf("when expression of rule %q: %w", r.Name, err)
	}

	return applies, nil
}

func (r *HealthEventsAnalyzerRule) compileWhen() error {
	r.when = nil

	if strings.TrimSpace(r.When) == "" {
		return nil
	}

	filter, err := celevent.Compile(r.When)
	if err != nil {
		return fmt.Errorf("when expression %q: %w", r.When, err)
	}

	r.when = filter

	return nil
}

type TomlConfig struct {
	// Registers rule_matched_entity_total. Off by default because entity
	// labels raise cardinality (GPU × GPC × TPC × SM per node).
	RuleMatchedEntityMetricEnabled bool                       `toml:"ruleMatchedEntityMetricEnabled"`
	Rules                          []HealthEventsAnalyzerRule `toml:"rules"`
}

// Compile compiles the when expression of every rule. LoadTomlConfig calls it, so a malformed
// expression stops the analyzer at startup instead of failing on every event.
func (c *TomlConfig) Compile() error {
	for i := range c.Rules {
		if err := c.Rules[i].compileWhen(); err != nil {
			return fmt.Errorf("rule %q: %w", c.Rules[i].Name, err)
		}
	}

	return nil
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if err := configmanager.LoadTOMLConfig(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	if err := config.Compile(); err != nil {
		return nil, fmt.Errorf("invalid rule in TOML config %s: %w", path, err)
	}

	return &config, nil
}
