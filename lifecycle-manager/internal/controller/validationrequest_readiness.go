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

package controller

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func buildCELEnvironment() (*cel.Env, error) {
	env, err := cel.NewEnv(cel.Variable("node", cel.AnyType), ext.Strings(),
		cel.CrossTypeNumericComparisons(true),
		cel.Function("quantity",
			cel.Overload("quantity_string", []*cel.Type{cel.StringType}, cel.DoubleType,
				cel.UnaryBinding(quantityToDouble),
			),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build CEL readiness environment: %w", err)
	}

	return env, nil
}

func quantityToDouble(val ref.Val) ref.Val {
	s, ok := val.Value().(string)
	if !ok {
		return types.NewErr("quantity: expected string, got %T", val.Value())
	}

	q, err := resource.ParseQuantity(s)
	if err != nil {
		return types.NewErr("quantity: parse %q: %v", s, err)
	}

	return types.Double(q.AsApproximateFloat64())
}

func (r *ValidationRequestReconciler) evaluateNodeReadinessCriteria(node *corev1.Node,
	criteria []v1alpha1.CriteriaSpec) (string, error) {
	nodeMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return "", fmt.Errorf("convert node %s to unstructured: %w", node.Name, err)
	}

	for _, c := range criteria {
		ok, err := r.evalReadinessCriterion(c.Expression, nodeMap)
		if err != nil {
			return c.Name, fmt.Errorf("criterion %q: %w", c.Name, err)
		}

		if !ok {
			return c.Name, nil
		}
	}

	return "", nil
}

func (r *ValidationRequestReconciler) evalReadinessCriterion(expr string, nodeMap map[string]any) (bool, error) {
	prg, ok := r.ReadinessPrograms[expr]
	if !ok {
		return false, fmt.Errorf("no compiled program for expression %q ", expr)
	}

	out, _, err := prg.Eval(map[string]any{"node": nodeMap})
	if err != nil {
		return false, fmt.Errorf("eval: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression must return bool, got %T", out.Value())
	}

	return result, nil
}

func buildReadinessPrograms(criteria []v1alpha1.CriteriaSpec) (map[string]cel.Program, error) {
	if len(criteria) == 0 {
		return nil, nil
	}

	env, err := buildCELEnvironment()
	if err != nil {
		return nil, err
	}

	programs := make(map[string]cel.Program, len(criteria))

	for _, c := range criteria {
		if _, ok := programs[c.Expression]; ok {
			continue
		}

		ast, issues := env.Parse(c.Expression)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("criterion %q: parse: %w", c.Name, issues.Err())
		}

		checkedAST, issues := env.Check(ast)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("criterion %q: check: %w", c.Name, issues.Err())
		}

		prg, err := env.Program(checkedAST)
		if err != nil {
			return nil, fmt.Errorf("criterion %q: program: %w", c.Name, err)
		}

		programs[c.Expression] = prg
	}

	return programs, nil
}
