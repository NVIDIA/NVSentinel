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

package evaluator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
)

const (
	eventObjKey = "event"
	nodeObjKey  = "node"
)

type RuleEvaluator interface {
	Evaluate(healthEvent *protos.HealthEvent) (common.RuleEvaluationResult, error)
}

type HealthEventRuleEvaluator struct {
	expression string
	program    cel.Program
}

type NodeRuleEvaluator struct {
	expression string
	program    cel.Program
	nodeLister corelisters.NodeLister
}

// NewHealthEventRuleEvaluator creates a new HealthEventRuleEvaluator with dynamic declarations
func NewHealthEventRuleEvaluator(expression string) (*HealthEventRuleEvaluator, error) {
	slog.Info("Creating HealthEventRuleEvaluator", "expression", expression)

	env, err := cel.NewEnv(
		cel.Variable(eventObjKey, cel.AnyType),
		ext.Strings(),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to parse expression: %w", issues.Err())
	}

	checkedAst, issues := env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to check expression: %w", issues.Err())
	}

	program, err := env.Program(checkedAst)
	if err != nil {
		return nil, fmt.Errorf("failed to compile expression: %w", err)
	}

	return &HealthEventRuleEvaluator{
		expression: expression,
		program:    program,
	}, nil
}

// evaluates the CEL expression against the provided HealthEvent
func (he *HealthEventRuleEvaluator) Evaluate(
	event *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	obj, err := RoundTrip(event)
	if err != nil {
		return common.RuleEvaluationErroredOut, fmt.Errorf("error roundtripping event: %w", err)
	}

	out, _, err := he.program.Eval(map[string]interface{}{
		eventObjKey: obj,
	})
	if err != nil {
		return common.RuleEvaluationErroredOut, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return common.RuleEvaluationErroredOut, fmt.Errorf("expression did not return a boolean: %v", out)
	}

	if result {
		return common.RuleEvaluationSuccess, nil
	}

	return common.RuleEvaluationFailed, nil
}

// NewNodeRuleEvaluator creates a new NodeRuleEvaluator
func NewNodeRuleEvaluator(expression string, nodeLister corelisters.NodeLister) (*NodeRuleEvaluator, error) {
	slog.Info("Creating NodeRuleEvaluator", "expression", expression)

	// Create a CEL environment with declarations for node.labels and node.annotations
	env, err := cel.NewEnv(
		cel.Variable(nodeObjKey, cel.AnyType),
		ext.Strings(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to parse expression: %w", issues.Err())
	}

	checkedAst, issues := env.Check(ast)

	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to check expression: %w", issues.Err())
	}

	program, err := env.Program(checkedAst)

	if err != nil {
		return nil, fmt.Errorf("failed to compile expression: %w", err)
	}

	return &NodeRuleEvaluator{
		expression: expression,
		program:    program,
		nodeLister: nodeLister,
	}, nil
}

// Evaluate the CEL expression against node metadata (labels and annotations)
func (nm *NodeRuleEvaluator) Evaluate(event *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	slog.Info("Evaluating NodeRuleEvaluator for node", "node", event.NodeName)

	// Get node metadata
	nodeInfo, err := nm.getNode(event.NodeName)
	if err != nil {
		return common.RuleEvaluationErroredOut, fmt.Errorf("failed to get node metadata: %w", err)
	}

	// Evaluate the expression
	out, _, err := nm.program.Eval(nodeInfo)
	if err != nil {
		return common.RuleEvaluationErroredOut, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return common.RuleEvaluationErroredOut, fmt.Errorf("expression did not return a boolean: %v", out)
	}

	if result {
		return common.RuleEvaluationSuccess, nil
	}

	return common.RuleEvaluationFailed, nil
}

// getNode gets both labels and annotations from a node using the informer lister
func (nm *NodeRuleEvaluator) getNode(nodeName string) (map[string]interface{}, error) {
	node, err := nm.nodeLister.Get(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s from informer cache: %w", nodeName, err)
	}

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return nil, fmt.Errorf("failed to convert node %s to unstructured: %w", nodeName, err)
	}

	return map[string]interface{}{
		"node": unstructuredObj,
	}, nil
}

// recursively converts any Go value into a JSON-compatible structure
// with all fields present. Structs become map[string]interface{}, slices become []interface{},
// maps become map[string]interface{}. Zero-values or nil pointers appear as null in the final map
// nolint: cyclop, gocognit //fix this as part of NGCC-21793
func structToInterface(v reflect.Value) interface{} {
	if !v.IsValid() {
		return nil
	}

	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}

		return structToInterface(v.Elem())

	case reflect.Struct:
		result := make(map[string]interface{})
		typ := v.Type()

		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			// unexported
			if field.PkgPath != "" {
				continue
			}

			jsonTag := field.Tag.Get("json")
			if jsonTag == "" {
				continue
			}

			name := jsonTag
			if idx := strings.Index(name, ","); idx != -1 {
				name = name[:idx]
			}

			if name == "" {
				name = field.Name
			}

			fieldVal := structToInterface(v.Field(i))
			result[name] = fieldVal
		}

		return result
	case reflect.Slice, reflect.Array:
		if v.IsNil() {
			return nil
		}

		sliceResult := make([]interface{}, v.Len())

		for i := 0; i < v.Len(); i++ {
			sliceResult[i] = structToInterface(v.Index(i))
		}

		return sliceResult
	case reflect.Map:
		if v.IsNil() {
			return nil
		}

		mapResult := make(map[string]interface{})

		for _, key := range v.MapKeys() {
			mapResult[key.String()] = structToInterface(v.MapIndex(key))
		}

		return mapResult
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.String:
		return v.Interface()
	case reflect.Invalid:
		return nil
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}

		return structToInterface(v.Elem())
	default:
		return v.Interface()
	}
}

// uses structToInterface for recursive processing
func RoundTrip(v interface{}) (map[string]interface{}, error) {
	val := reflect.ValueOf(v)
	obj := structToInterface(val)

	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal intermediate object: %w", err)
	}

	var j interface{}
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON back to map: %w", err)
	}

	m, ok := j.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected JSON object after roundtrip")
	}

	return m, nil
}
