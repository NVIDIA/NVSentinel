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
	"context"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const (
	resourceSlicesVariable = "resourceSlices"
	// resourceSliceNodeNameField is both the manager cache index name and an API server field selector for
	// ResourceSlices, so listResourceSlicesForNode works against the cached client and a direct client.
	resourceSliceNodeNameField = "spec.nodeName"
)

func buildCELEnvironment() (*cel.Env, error) {
	env, err := cel.NewEnv(cel.Variable("node", cel.AnyType),
		cel.Variable(resourceSlicesVariable, cel.ListType(cel.DynType)), ext.Strings(),
		cel.CrossTypeNumericComparisons(true),
		cel.Function("quantity",
			cel.Overload("quantity_string", []*cel.Type{cel.StringType}, cel.DoubleType,
				cel.UnaryBinding(quantityToDouble),
			),
		),
		cel.Function("now",
			cel.Overload("now_", nil, cel.TimestampType,
				cel.FunctionBinding(func(_ ...ref.Val) ref.Val {
					return types.Timestamp{Time: time.Now()}
				}),
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

// evaluateCriteria looks up the ResourceSlices of the node, if readsResourceSlices is set, and evaluates the criteria
// against the node. This is shared by the ValidationRequestReconciler and NodeValidationReconciler, which each maintain
// their own compiled program map.
func evaluateCriteria(ctx context.Context, c client.Reader, node *corev1.Node, criteria []v1alpha1.CriteriaSpec,
	programs map[string]cel.Program, readsResourceSlices bool) (string, error) {
	var resourceSlices []resourcev1.ResourceSlice

	if readsResourceSlices {
		var err error

		resourceSlices, err = listResourceSlicesForNode(ctx, c, node.Name)
		if err != nil {
			return "", err
		}
	}

	return evaluateCriteriaWithSlices(node, resourceSlices, criteria, programs)
}

// evaluateCriteriaWithSlices evaluates each CEL criterion against the given node and its ResourceSlices in order. It
// returns the name of the first criterion that fails, or an empty string if all criteria evaluate to true.
func evaluateCriteriaWithSlices(node *corev1.Node, resourceSlices []resourcev1.ResourceSlice,
	criteria []v1alpha1.CriteriaSpec, programs map[string]cel.Program) (string, error) {
	nodeMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return "", fmt.Errorf("convert node %s to unstructured: %w", node.Name, err)
	}

	resourceSliceMaps := make([]map[string]any, 0, len(resourceSlices))

	for i := range resourceSlices {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&resourceSlices[i])
		if err != nil {
			return "", fmt.Errorf("convert ResourceSlice %s to unstructured: %w", resourceSlices[i].Name, err)
		}

		resourceSliceMaps = append(resourceSliceMaps, m)
	}

	vars := map[string]any{"node": nodeMap, resourceSlicesVariable: resourceSliceMaps}

	for _, c := range criteria {
		ok, err := evalCriterion(programs, c.Expression, vars)
		if err != nil {
			return c.Name, fmt.Errorf("criterion %q: %w", c.Name, err)
		}

		if !ok {
			return c.Name, nil
		}
	}

	return "", nil
}

func evalCriterion(programs map[string]cel.Program, expr string, vars map[string]any) (bool, error) {
	prg, ok := programs[expr]
	if !ok {
		return false, fmt.Errorf("no compiled program for expression %q ", expr)
	}

	out, _, err := prg.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("eval: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression must return bool, got %T", out.Value())
	}

	return result, nil
}

// buildReadinessPrograms compiles the criteria, keyed by expression, and reports whether any of them references the
// resourceSlices variable.
func buildReadinessPrograms(criteria []v1alpha1.CriteriaSpec) (map[string]cel.Program, bool, error) {
	if len(criteria) == 0 {
		return nil, false, nil
	}

	env, err := buildCELEnvironment()
	if err != nil {
		return nil, false, err
	}

	programs := make(map[string]cel.Program, len(criteria))
	readsResourceSlices := false

	for _, c := range criteria {
		if _, ok := programs[c.Expression]; ok {
			continue
		}

		ast, issues := env.Parse(c.Expression)
		if issues != nil && issues.Err() != nil {
			return nil, false, fmt.Errorf("criterion %q: parse: %w", c.Name, issues.Err())
		}

		checkedAST, issues := env.Check(ast)
		if issues != nil && issues.Err() != nil {
			return nil, false, fmt.Errorf("criterion %q: check: %w", c.Name, issues.Err())
		}

		prg, err := env.Program(checkedAST)
		if err != nil {
			return nil, false, fmt.Errorf("criterion %q: program: %w", c.Name, err)
		}

		programs[c.Expression] = prg
		readsResourceSlices = readsResourceSlices || referencesVariable(checkedAST, resourceSlicesVariable)
	}

	return programs, readsResourceSlices, nil
}

// referencesVariable reports whether the checked expression resolves an identifier to the variable name. The checker
// only records identifiers it resolved, so the name inside a comment or a string literal does not count.
func referencesVariable(checkedAST *cel.Ast, name string) bool {
	for _, ref := range checkedAST.NativeRep().ReferenceMap() {
		if ref.Name == name {
			return true
		}
	}

	return false
}

// SetupResourceSliceIndex registers the cache index that readiness and new node criteria use to look up the
// ResourceSlices of a node. Callers register it only when a criterion references resourceSlices.
func SetupResourceSliceIndex(ctx context.Context, indexer client.FieldIndexer) error {
	err := indexer.IndexField(ctx, &resourcev1.ResourceSlice{}, resourceSliceNodeNameField,
		func(obj client.Object) []string {
			if nodeName := resourceSliceNodeName(obj); len(nodeName) != 0 {
				return []string{nodeName}
			}

			return nil
		})
	if err != nil {
		return fmt.Errorf("index ResourceSlices by %s: %w", resourceSliceNodeNameField, err)
	}

	return nil
}

func listResourceSlicesForNode(ctx context.Context, c client.Reader,
	nodeName string) ([]resourcev1.ResourceSlice, error) {
	var list resourcev1.ResourceSliceList
	if err := c.List(ctx, &list, client.MatchingFields{resourceSliceNodeNameField: nodeName}); err != nil {
		return nil, fmt.Errorf("list ResourceSlices for node %q: %w", nodeName, err)
	}

	return list.Items, nil
}

func resourceSliceNodeName(obj client.Object) string {
	resourceSlice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok || resourceSlice.Spec.NodeName == nil {
		return ""
	}

	return *resourceSlice.Spec.NodeName
}

// gpuResourceSlicePredicate admits only gpu.nvidia.com ResourceSlice events, so slices of unrelated DRA drivers
// (for example ComputeDomain IMEX channels, which churn with workloads) do not trigger reconciles. The cache and the
// resourceSlices CEL variable are not filtered.
func gpuResourceSlicePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		resourceSlice, ok := obj.(*resourcev1.ResourceSlice)

		return ok && resourceSlice.Spec.Driver == model.GPUDRADriverName
	})
}
