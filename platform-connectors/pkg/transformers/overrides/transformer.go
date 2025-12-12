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

// Package overrides provides health event property override functionality
// using CEL-based rule evaluation to conditionally modify health event fields.
package overrides

import (
	"context"
	"fmt"
	"log/slog"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type Processor struct {
	enabled bool
	rules   []compiledRule
}

type originalState struct {
	isFatal           bool
	isHealthy         bool
	recommendedAction pb.RecommendedAction
}

func captureOriginalState(event *pb.HealthEvent) originalState {
	return originalState{
		isFatal:           event.IsFatal,
		isHealthy:         event.IsHealthy,
		recommendedAction: event.RecommendedAction,
	}
}

func NewProcessor(config *Config) (*Processor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	rules, err := compileRules(config)
	if err != nil {
		return nil, fmt.Errorf("failed to compile rules: %w", err)
	}

	slog.Info("Override processor initialized",
		"enabled", config.Enabled,
		"rule_count", len(rules))

	return &Processor{
		enabled: config.Enabled,
		rules:   rules,
	}, nil
}

func (p *Processor) Transform(ctx context.Context, event *pb.HealthEvent) error {
	if !p.enabled || len(p.rules) == 0 {
		return nil
	}

	originalState := captureOriginalState(event)

	for _, rule := range p.rules {
		matches, err := rule.evaluate(event)
		if err != nil {
			slog.Error("Failed to evaluate override rule",
				"rule", rule.name,
				"node", event.NodeName,
				"agent", event.Agent,
				"error", err)

			evaluationErrors.Inc()

			continue
		}

		if matches {
			p.applyOverride(event, rule, originalState)
			return nil
		}
	}

	return nil
}

func (p *Processor) Name() string {
	return "OverrideTransformer"
}

func (p *Processor) applyOverride(event *pb.HealthEvent, rule compiledRule, original originalState) {
	changed := []string{}

	if rule.override.IsFatal != nil && *rule.override.IsFatal != original.isFatal {
		event.IsFatal = *rule.override.IsFatal
		overridesApplied.WithLabelValues(rule.name, "isFatal").Inc()

		changed = append(changed, "isFatal")
	}

	if rule.override.IsHealthy != nil && *rule.override.IsHealthy != original.isHealthy {
		event.IsHealthy = *rule.override.IsHealthy
		overridesApplied.WithLabelValues(rule.name, "isHealthy").Inc()

		changed = append(changed, "isHealthy")
	}

	if rule.override.RecommendedAction != nil {
		newAction, err := rule.override.ParseRecommendedAction()
		if err != nil {
			slog.Error("Failed to parse recommended action",
				"rule", rule.name,
				"node", event.NodeName,
				"agent", event.Agent,
				"error", err)

			evaluationErrors.Inc()

			newAction = original.recommendedAction
		}

		if newAction != original.recommendedAction {
			event.RecommendedAction = newAction

			overridesApplied.WithLabelValues(rule.name, "recommendedAction").Inc()

			changed = append(changed, "recommendedAction")
		}
	}

	if len(changed) > 0 {
		slog.Info("Applied health event override",
			"rule", rule.name,
			"node", event.NodeName,
			"agent", event.Agent,
			"check", event.CheckName,
			"fields_changed", changed,
			"original_is_fatal", original.isFatal,
			"new_is_fatal", event.IsFatal,
			"original_is_healthy", original.isHealthy,
			"new_is_healthy", event.IsHealthy,
			"original_action", original.recommendedAction.String(),
			"new_action", event.RecommendedAction.String())
	}
}
