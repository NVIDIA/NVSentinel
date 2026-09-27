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
	"log/slog"
	"strings"

	"github.com/BurntSushi/toml"
	"k8s.io/apimachinery/pkg/util/validation"
)

type RecoveryScope string

const (
	RecoveryScopeNode   RecoveryScope = "node"
	RecoveryScopeEntity RecoveryScope = "entity"
)

type RecoveryMapping struct {
	AnnotationKey string        `toml:"annotation_key"`
	Scope         RecoveryScope `toml:"scope"`
	EntityTypes   []string      `toml:"entity_types"`
}

type HealthEventsAnalyzerRule struct {
	Recovery          *RecoveryMapping `toml:"recovery"`
	Name              string           `toml:"name"`
	Description       string           `toml:"description"`
	Stage             []string         `toml:"stage"`
	RecommendedAction string           `toml:"recommended_action"`
	Message           string           `toml:"message"`
	EvaluateRule      bool             `toml:"evaluate_rule"`
	// Optional: override the module-level processing strategy for events published by this rule.
	ProcessingStrategy string `toml:"processing_strategy"`
}

type TomlConfig struct {
	// Registers rule_matched_entity_total. Off by default because entity
	// labels raise cardinality (GPU × GPC × TPC × SM per node).
	RuleMatchedEntityMetricEnabled bool                       `toml:"ruleMatchedEntityMetricEnabled"`
	Rules                          []HealthEventsAnalyzerRule `toml:"rules"`
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var cfg TomlConfig

	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	for _, key := range metadata.Undecoded() {
		slog.Warn("Ignoring unknown analyzer configuration key", "key", key.String(), "path", path)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *TomlConfig) HasAnnotationRecovery() bool {
	if c == nil {
		return false
	}

	for _, rule := range c.Rules {
		if rule.EvaluateRule && rule.Recovery != nil {
			return true
		}
	}

	return false
}

// Only the new recovery contract is validated. Existing rule validation remains
// at evaluation/publication time, as it was before annotation recovery.
func (c *TomlConfig) Validate() error {
	keys := make(map[string]string)

	for _, rule := range c.Rules {
		if rule.Recovery == nil {
			continue
		}

		if err := rule.Recovery.validate(); err != nil {
			return fmt.Errorf("rule %q: %w", rule.Name, err)
		}

		key := rule.Recovery.AnnotationKey
		if previous, exists := keys[key]; exists {
			return fmt.Errorf("rules %q and %q share recovery.annotation_key %q", previous, rule.Name, key)
		}

		keys[key] = rule.Name
	}

	return nil
}

func (r *RecoveryMapping) validate() error {
	r.AnnotationKey = strings.TrimSpace(r.AnnotationKey)
	if !strings.Contains(r.AnnotationKey, "/") || len(validation.IsQualifiedName(r.AnnotationKey)) != 0 {
		return fmt.Errorf("recovery.annotation_key must be a qualified Kubernetes annotation key")
	}

	switch r.Scope {
	case RecoveryScopeNode:
		if len(r.EntityTypes) != 0 {
			return fmt.Errorf("node recovery must not configure entity_types")
		}
	case RecoveryScopeEntity:
		if len(r.EntityTypes) == 0 {
			return fmt.Errorf("entity recovery requires entity_types")
		}
	default:
		return fmt.Errorf("recovery.scope must be node or entity")
	}

	return validateEntityTypes(r.EntityTypes)
}

func validateEntityTypes(entityTypes []string) error {
	seen := make(map[string]bool)
	for _, entityType := range entityTypes {
		if strings.TrimSpace(entityType) == "" || seen[entityType] {
			return fmt.Errorf("recovery.entity_types must be non-empty and unique")
		}

		seen[entityType] = true
	}

	return nil
}
