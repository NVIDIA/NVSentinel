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

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
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
		return common.RuleEvaluationFailed, fmt.Errorf("error roundtripping event: %w", err)
	}

	out, _, err := he.program.Eval(map[string]interface{}{
		eventObjKey: obj,
	})
	if err != nil {
		return common.RuleEvaluationFailed, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return common.RuleEvaluationFailed, fmt.Errorf("expression did not return a boolean: %v", out)
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

	nodeInfo, err := nm.getNode(event.NodeName)
	if err != nil {
		return common.RuleEvaluationFailed, fmt.Errorf("failed to get node metadata: %w", err)
	}

	out, _, err := nm.program.Eval(nodeInfo)
	if err != nil {
		return common.RuleEvaluationFailed, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return common.RuleEvaluationFailed, fmt.Errorf("expression did not return a boolean: %v", out)
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

var primitiveKinds = map[reflect.Kind]bool{
	reflect.Bool:       true,
	reflect.Int:        true,
	reflect.Int8:       true,
	reflect.Int16:      true,
	reflect.Int32:      true,
	reflect.Int64:      true,
	reflect.Uint:       true,
	reflect.Uint8:      true,
	reflect.Uint16:     true,
	reflect.Uint32:     true,
	reflect.Uint64:     true,
	reflect.Uintptr:    true,
	reflect.Float32:    true,
	reflect.Float64:    true,
	reflect.Complex64:  true,
	reflect.Complex128: true,
	reflect.String:     true,
}

// recursively converts any Go value into a JSON-compatible structure
// with all fields present. Structs become map[string]interface{}, slices become []interface{},
// maps become map[string]interface{}. Zero-values or nil pointers appear as null in the final map
func structToInterface(v reflect.Value) interface{} {
	if !v.IsValid() {
		return nil
	}

	kind := v.Kind()

	if primitiveKinds[kind] {
		return v.Interface()
	}

	return handleComplexType(v, kind)
}

func handleComplexType(v reflect.Value, kind reflect.Kind) interface{} {
	switch kind {
	case reflect.Ptr:
		return handlePointer(v)
	case reflect.Struct:
		return handleStruct(v)
	case reflect.Slice, reflect.Array:
		return handleSliceOrArray(v)
	case reflect.Map:
		return handleMap(v)
	case reflect.Interface:
		return handleInterface(v)
	case reflect.Invalid, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.String:
		return v.Interface()
	default:
		return v.Interface()
	}
}

func handlePointer(v reflect.Value) interface{} {
	if v.IsNil() {
		return nil
	}

	return structToInterface(v.Elem())
}

func handleStruct(v reflect.Value) interface{} {
	result := make(map[string]interface{})
	typ := v.Type()

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag == "" {
			continue
		}

		name := extractJSONFieldName(jsonTag, field.Name)

		fieldVal := structToInterface(v.Field(i))
		result[name] = fieldVal
	}

	return result
}

func extractJSONFieldName(jsonTag, fieldName string) string {
	name := jsonTag
	if idx := strings.Index(name, ","); idx != -1 {
		name = name[:idx]
	}

	if name == "" {
		name = fieldName
	}

	return name
}

func handleSliceOrArray(v reflect.Value) interface{} {
	if v.Kind() == reflect.Slice && v.IsNil() {
		return nil
	}

	sliceResult := make([]interface{}, v.Len())

	for i := 0; i < v.Len(); i++ {
		sliceResult[i] = structToInterface(v.Index(i))
	}

	return sliceResult
}

func handleMap(v reflect.Value) interface{} {
	if v.IsNil() {
		return nil
	}

	mapResult := make(map[string]interface{})

	for _, key := range v.MapKeys() {
		mapResult[key.String()] = structToInterface(v.MapIndex(key))
	}

	return mapResult
}

func handleInterface(v reflect.Value) interface{} {
	if v.IsNil() {
		return nil
	}

	return structToInterface(v.Elem())
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
