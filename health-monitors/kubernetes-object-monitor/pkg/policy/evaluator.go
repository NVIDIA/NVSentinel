// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package policy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Evaluator struct {
	celEnv             *celenv.Environment
	compiledPredicates map[string]*cel.Ast
	compiledNodeAssoc  map[string]*cel.Ast
}

func NewEvaluator(env *celenv.Environment, policies []config.Policy) (*Evaluator, error) {
	e := &Evaluator{
		celEnv:             env,
		compiledPredicates: make(map[string]*cel.Ast),
		compiledNodeAssoc:  make(map[string]*cel.Ast),
	}

	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}

		ast, err := env.Compile(policy.Predicate.Expression)
		if err != nil {
			slog.Error("Failed to compile predicate", "policy", policy.Name, "error", err)
			return nil, fmt.Errorf("policy %q: failed to compile predicate: %w", policy.Name, err)
		}

		e.compiledPredicates[policy.Name] = ast

		if policy.NodeAssociation != nil {
			nodeAst, err := env.Compile(policy.NodeAssociation.Expression)
			if err != nil {
				slog.Error("Failed to compile node association", "policy", policy.Name, "error", err)
				return nil, fmt.Errorf("policy %q: failed to compile node association: %w", policy.Name, err)
			}

			e.compiledNodeAssoc[policy.Name] = nodeAst
		}
	}

	return e, nil
}

func (e *Evaluator) EvaluatePredicate(
	ctx context.Context,
	policyName string,
	resource *unstructured.Unstructured,
) (bool, error) {
	ast, ok := e.compiledPredicates[policyName]
	if !ok {
		return false, fmt.Errorf("policy %q not found or not enabled", policyName)
	}

	result, err := e.celEnv.Evaluate(ast, resource.Object, ctx)
	if err != nil {
		return false, fmt.Errorf("evaluation failed: %w", err)
	}

	boolResult, ok := result.(types.Bool)
	if !ok {
		return false, fmt.Errorf("predicate did not return boolean, got %T", result)
	}

	return bool(boolResult), nil
}

func (e *Evaluator) EvaluateNodeAssociation(
	ctx context.Context,
	policyName string,
	resource *unstructured.Unstructured,
) (string, error) {
	if resource.GetKind() == "Node" {
		return resource.GetName(), nil
	}

	ast, ok := e.compiledNodeAssoc[policyName]
	if !ok {
		return "", nil
	}

	result, err := e.celEnv.Evaluate(ast, resource.Object, ctx)
	if err != nil {
		return "", fmt.Errorf("node association evaluation failed: %w", err)
	}

	if types.IsUnknownOrError(result) {
		return "", nil
	}

	if result.Type() == types.NullType {
		return "", nil
	}

	strResult, ok := result.(types.String)
	if !ok {
		return "", fmt.Errorf("nodeAssociation did not return string, got %T", result)
	}

	return string(strResult), nil
}
