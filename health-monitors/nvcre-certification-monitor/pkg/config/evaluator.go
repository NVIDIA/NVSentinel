// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package config loads and evaluates policy configuration for the
// nvcre-certification-monitor.
package config

import (
	"fmt"
	"log/slog"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

// EvalContext carries the CEL variables available to a policy match expression.
type EvalContext struct {
	// FailedNode exposes failedNode.name, failedNode.reason, failedNode.message.
	FailedNode map[string]string
	// Category exposes category.domain, category.variant.
	Category map[string]string
}

// Evaluator compiles and evaluates the configured policy match expressions.
type Evaluator struct {
	env      *cel.Env
	compiled []compiledPolicy
}

type compiledPolicy struct {
	name    string
	program cel.Program
}

// NewEvaluator builds an Evaluator from the configured policies.
func NewEvaluator(policies []Policy) (*Evaluator, error) {
	env, err := cel.NewEnv(
		cel.Variable("failedNode", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("category", cel.MapType(cel.StringType, cel.StringType)),
	)
	if err != nil {
		slog.Error("Failed to create CEL environment", "error", err)

		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	e := &Evaluator{env: env}

	for _, p := range policies {
		ast, issues := env.Compile(p.Match)
		if issues != nil && issues.Err() != nil {
			slog.Error("Failed to compile match expression", "policy", p.Name, "error", issues.Err())

			return nil, fmt.Errorf("policy %q: failed to compile match expression: %w", p.Name, issues.Err())
		}

		if out := ast.OutputType(); !out.IsExactType(cel.BoolType) {
			slog.Error("Match expression does not evaluate to bool", "policy", p.Name, "type", out.String())

			return nil, fmt.Errorf("policy %q: match expression must evaluate to bool, got %s", p.Name, out.String())
		}

		prg, err := env.Program(ast)
		if err != nil {
			slog.Error("Failed to build CEL program", "policy", p.Name, "error", err)

			return nil, fmt.Errorf("policy %q: failed to build CEL program: %w", p.Name, err)
		}

		e.compiled = append(e.compiled, compiledPolicy{name: p.Name, program: prg})
		slog.Info("Compiled policy", "policy", p.Name, "match", p.Match)
	}

	return e, nil
}

// Matches returns true and the matching policy name if any configured policy's
// match expression evaluates to true for the given context.
func (e *Evaluator) Matches(c EvalContext) (bool, string) {
	vars := map[string]any{
		"failedNode": toAnyMap(c.FailedNode),
		"category":   toAnyMap(c.Category),
	}

	for _, p := range e.compiled {
		out, _, err := p.program.Eval(vars)
		if err != nil {
			slog.Error("Policy evaluation failed", "policy", p.name, "error", err)

			continue
		}

		b, ok := out.(types.Bool)

		if !ok {
			slog.Error("Policy match did not return bool", "policy", p.name, "type", fmt.Sprintf("%T", out))

			continue
		}

		if bool(b) {
			return true, p.name
		}
	}

	return false, ""
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))

	for k, v := range in {
		out[k] = v
	}

	return out
}
