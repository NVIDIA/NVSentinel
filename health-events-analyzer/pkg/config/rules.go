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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type RecoveryScope string

const (
	RecoveryScopeNode   RecoveryScope = "node"
	RecoveryScopeEntity RecoveryScope = "entity"
	analyzerAgentName                 = "health-events-analyzer"
)

// RecoveryMapping identifies the healthy source event that resolves a derived
// condition. Rules without this block retain the existing manual-recovery
// behavior.
type RecoveryMapping struct {
	SourceAgent      string        `toml:"source_agent"`
	SourceCheckName  string        `toml:"source_check_name"`
	SourceErrorCodes []string      `toml:"source_error_codes"`
	Scope            RecoveryScope `toml:"scope"`
	EntityTypes      []string      `toml:"entity_types"`
}

type HealthEventsAnalyzerRule struct {
	Name              string   `toml:"name"`
	Description       string   `toml:"description"`
	Stage             []string `toml:"stage"`
	RecommendedAction string   `toml:"recommended_action"`
	Message           string   `toml:"message"`
	EvaluateRule      bool     `toml:"evaluate_rule"`
	// Optional: override the module-level processing strategy for events published by this rule.
	ProcessingStrategy string           `toml:"processing_strategy"`
	Recovery           *RecoveryMapping `toml:"recovery"`
}

type TomlConfig struct {
	Rules []HealthEventsAnalyzerRule `toml:"rules"`
}

func (c *TomlConfig) HasEnabledRecovery() bool {
	if c == nil {
		return false
	}

	for i := range c.Rules {
		if c.Rules[i].EvaluateRule && c.Rules[i].Recovery != nil {
			return true
		}
	}

	return false
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if err := configmanager.LoadTOMLConfigStrict(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid health-events-analyzer config: %w", err)
	}

	return &config, nil
}

func (c *TomlConfig) Validate() error {
	for i := range c.Rules {
		if err := c.Rules[i].validateProcessingStrategy(); err != nil {
			return fmt.Errorf("rule %q: %w", c.Rules[i].Name, err)
		}

		if err := c.Rules[i].validateStages(); err != nil {
			return fmt.Errorf("rule %q: %w", c.Rules[i].Name, err)
		}

		if err := c.Rules[i].validateRecovery(); err != nil {
			return fmt.Errorf("rule %q: %w", c.Rules[i].Name, err)
		}
	}

	return nil
}

func (r *HealthEventsAnalyzerRule) validateProcessingStrategy() error {
	if r.ProcessingStrategy == "" {
		return nil
	}

	if _, ok := protos.ProcessingStrategy_value[r.ProcessingStrategy]; !ok {
		return fmt.Errorf("processing_strategy has invalid value %q", r.ProcessingStrategy)
	}

	return nil
}

func (r *HealthEventsAnalyzerRule) validateStages() error {
	for i, stage := range r.Stage {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(stage), &parsed); err != nil {
			return fmt.Errorf("stage %d is not valid JSON: %w", i, err)
		}

		if len(parsed) != 1 {
			return fmt.Errorf("stage %d must contain exactly one aggregation operator", i)
		}
	}

	return nil
}

func (r *HealthEventsAnalyzerRule) validateRecovery() error {
	if r.Recovery == nil {
		return nil
	}

	recovery := r.Recovery
	recovery.SourceAgent = strings.TrimSpace(recovery.SourceAgent)
	recovery.SourceCheckName = strings.TrimSpace(recovery.SourceCheckName)

	if recovery.SourceCheckName == "" {
		return fmt.Errorf("recovery.source_check_name is required")
	}

	if recovery.SourceAgent == analyzerAgentName {
		return fmt.Errorf("recovery.source_agent %q is excluded from analyzer input", analyzerAgentName)
	}

	switch recovery.Scope {
	case RecoveryScopeNode:
		if len(recovery.EntityTypes) != 0 {
			return fmt.Errorf("recovery.entity_types must be empty for node scope")
		}
	case RecoveryScopeEntity:
		if len(recovery.EntityTypes) == 0 {
			return fmt.Errorf("recovery.entity_types is required for entity scope")
		}
	default:
		return fmt.Errorf("recovery.scope must be %q or %q", RecoveryScopeNode, RecoveryScopeEntity)
	}

	if err := validateUniqueNonEmpty("recovery.entity_types", recovery.EntityTypes); err != nil {
		return err
	}

	return validateUniqueNonEmpty("recovery.source_error_codes", recovery.SourceErrorCodes)
}

func validateUniqueNonEmpty(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))

	for i := range values {
		values[i] = strings.TrimSpace(values[i])
		if values[i] == "" {
			return fmt.Errorf("%s must not contain empty values", field)
		}

		if _, exists := seen[values[i]]; exists {
			return fmt.Errorf("%s contains duplicate value %q", field, values[i])
		}

		seen[values[i]] = struct{}{}
	}

	return nil
}
