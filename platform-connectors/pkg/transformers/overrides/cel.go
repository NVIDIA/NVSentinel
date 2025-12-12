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

package overrides

import (
	"fmt"
	"log/slog"
	"maps"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type compiledRule struct {
	name     string
	program  cel.Program
	override Override
}

func buildCELEnvironment() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
	)
}

func compileRules(config *Config) ([]compiledRule, error) {
	if !config.Enabled || len(config.Rules) == 0 {
		return nil, nil
	}

	env, err := buildCELEnvironment()
	if err != nil {
		slog.Error("Failed to create CEL environment", "error", err)
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	compiled := make([]compiledRule, 0, len(config.Rules))

	for i, rule := range config.Rules {
		ast, issues := env.Compile(rule.When)
		if issues != nil && issues.Err() != nil {
			slog.Error("Failed to compile CEL expression", "rule", rule.Name, "error", issues.Err())

			return nil, fmt.Errorf("rule[%d] (%s): CEL compilation failed: %w",
				i, rule.Name, issues.Err())
		}

		if ast.OutputType() != cel.BoolType {
			slog.Error("Failed to compile CEL expression", "rule", rule.Name, "error", ast.OutputType())

			return nil, fmt.Errorf("rule[%d] (%s): expression must return boolean, got %v",
				i, rule.Name, ast.OutputType())
		}

		program, err := env.Program(ast)
		if err != nil {
			slog.Error("Failed to create CEL program", "rule", rule.Name, "error", err)

			return nil, fmt.Errorf("rule[%d] (%s): failed to create program: %w",
				i, rule.Name, err)
		}

		compiled = append(compiled, compiledRule{
			name:     rule.Name,
			program:  program,
			override: rule.Override,
		})
	}

	return compiled, nil
}

func (r *compiledRule) evaluate(event *pb.HealthEvent) (bool, error) {
	eventMap := buildEventMap(event)

	result, _, err := r.program.Eval(map[string]any{
		"event": eventMap,
	})
	if err != nil {
		slog.Error("Failed to evaluate CEL expression", "rule", r.name, "error", err)

		return false, fmt.Errorf("evaluation failed: %w", err)
	}

	if result == types.False {
		return false, nil
	}

	if result == types.True {
		return true, nil
	}

	if boolVal, ok := result.Value().(bool); ok {
		return boolVal, nil
	}

	return false, fmt.Errorf("expression returned non-boolean: %T", result.Value())
}

func buildEventMap(event *pb.HealthEvent) map[string]any {
	return map[string]any{
		"agent":             event.Agent,
		"checkName":         event.CheckName,
		"componentClass":    event.ComponentClass,
		"errorCode":         event.ErrorCode,
		"isFatal":           event.IsFatal,
		"isHealthy":         event.IsHealthy,
		"recommendedAction": event.RecommendedAction.String(),
		"nodeName":          event.NodeName,
		"metadata":          maps.Clone(event.Metadata),
		"message":           event.Message,
	}
}
