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

// Package celevent provides the shared CEL vocabulary for HealthEvent expressions.
//
// It exists so every component that lets an operator write CEL over a health event binds
// the same field names to the same types. The platform connector's override transformer
// and the event exporter's filter both use it, so an expression learned for one works in
// the other.
package celevent

import (
	"fmt"
	"maps"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// NewEnv returns a CEL environment with the "event" variable bound.
func NewEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	return env, nil
}

// CompileBool compiles an expression expected to evaluate to a boolean.
//
// Static typing only goes so far here. "event" is bound as map[string]dyn, so any
// expression reading a field, including a genuinely boolean one such as `event.isFatal`,
// has static type dyn rather than bool. Rejecting dyn would refuse valid filters, so dyn
// is accepted and EvaluateBool enforces the boolean at evaluation time. Syntax errors and
// expressions with a concrete non-boolean type are still caught here, at startup.
func CompileBool(env *cel.Env, expression string) (cel.Program, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compilation failed: %w", issues.Err())
	}

	if outputType := ast.OutputType(); outputType != cel.BoolType && outputType != cel.DynType {
		return nil, fmt.Errorf("expression must return boolean, got %v", outputType)
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	return program, nil
}

// EvaluateBool evaluates a program compiled by CompileBool against one health event.
func EvaluateBool(program cel.Program, event *pb.HealthEvent) (bool, error) {
	result, _, err := program.Eval(map[string]any{
		"event": BuildEventMap(event),
	})
	if err != nil {
		return false, fmt.Errorf("evaluation failed: %w", err)
	}

	switch result {
	case types.False:
		return false, nil
	case types.True:
		return true, nil
	}

	if boolVal, ok := result.Value().(bool); ok {
		return boolVal, nil
	}

	return false, fmt.Errorf("expression returned non-boolean: %T", result.Value())
}

// BuildEventMap binds a health event's fields for CEL evaluation.
//
// Note errorCode is a repeated field, so it binds as a list: match it with
// `'45' in event.errorCode`, not `event.errorCode == '45'`.
func BuildEventMap(event *pb.HealthEvent) map[string]any {
	return map[string]any{
		"agent":             event.GetAgent(),
		"checkName":         event.GetCheckName(),
		"componentClass":    event.GetComponentClass(),
		"errorCode":         event.GetErrorCode(),
		"isFatal":           event.GetIsFatal(),
		"isHealthy":         event.GetIsHealthy(),
		"recommendedAction": event.GetRecommendedAction().String(),
		"nodeName":          event.GetNodeName(),
		"metadata":          maps.Clone(event.GetMetadata()),
		"message":           event.GetMessage(),
	}
}
