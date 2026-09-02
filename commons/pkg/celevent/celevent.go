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
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Filter is a compiled boolean CEL expression over a health event.
//
// It deliberately hides the cel-go types, so a caller filtering health events depends on
// this package rather than on cel-go directly. cel-go moves its module path to
// cel.dev/cel-go in v0.32.0, and this keeps that migration out of the health-event filter
// path.
//
// It does not remove the dependency repo-wide: fault-quarantine, labeler, preflight and
// kubernetes-object-monitor build their own CEL environments over different inputs and
// still import cel-go directly. Those are separate vocabularies, not health events, so
// they are deliberately not routed through here.
type Filter struct {
	program cel.Program
}

// Compile compiles an expression that must statically evaluate to a boolean.
//
// "event" is bound as map[string]dyn, so a bare field read is typed dyn and is rejected
// here, including a semantically boolean one: write `event.isFatal == true`, not
// `event.isFatal`. That is stricter than it needs to be for the boolean fields, and it is
// the right trade: an expression is validated at startup rather than failing on the first
// event, which for a filter that legitimately drops most events is the difference between
// a clear error and silently exporting nothing. It also matches the behaviour the override
// transformer has always had.
func Compile(expression string) (*Filter, error) {
	env, err := cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compilation failed: %w", issues.Err())
	}

	if outputType := ast.OutputType(); outputType != cel.BoolType {
		return nil, fmt.Errorf(
			"expression must return boolean, got %v (a bare field read is untyped; "+
				"compare it, for example `event.isFatal == true`)", outputType)
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	return &Filter{program: program}, nil
}

// Matches evaluates the compiled expression against one health event.
func (f *Filter) Matches(event *pb.HealthEvent) (bool, error) {
	result, _, err := f.program.Eval(map[string]any{
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
//
// errorCode and metadata are both cloned. CEL evaluation cannot mutate them, but this is
// exported, and handing a caller the event's own slice and map would let it mutate the
// event through the returned map.
func BuildEventMap(event *pb.HealthEvent) map[string]any {
	return map[string]any{
		"agent":             event.GetAgent(),
		"checkName":         event.GetCheckName(),
		"componentClass":    event.GetComponentClass(),
		"errorCode":         slices.Clone(event.GetErrorCode()),
		"isFatal":           event.GetIsFatal(),
		"isHealthy":         event.GetIsHealthy(),
		"recommendedAction": event.GetRecommendedAction().String(),
		"nodeName":          event.GetNodeName(),
		"metadata":          maps.Clone(event.GetMetadata()),
		"message":           event.GetMessage(),
	}
}
