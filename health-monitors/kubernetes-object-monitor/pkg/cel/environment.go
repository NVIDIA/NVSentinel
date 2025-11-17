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
package cel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Environment struct {
	env    *cel.Env
	client client.Client
	evalMu sync.Mutex
	ctx    context.Context
}

func NewEnvironment(c client.Client) (*Environment, error) {
	e := &Environment{
		client: c,
	}

	env, err := cel.NewEnv(
		cel.Variable("resource", cel.DynType),
		cel.Variable("now", cel.TimestampType),
		cel.Function("lookup",
			cel.Overload("lookup_string_string_string_string",
				[]*cel.Type{cel.StringType, cel.StringType, cel.StringType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(e.lookup),
			),
		),
	)
	if err != nil {
		slog.Error("Failed to create CEL environment", "error", err)
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	e.env = env

	return e, nil
}

func (e *Environment) Compile(expression string) (*cel.Ast, error) {
	ast, issues := e.env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		slog.Error("Failed to compile CEL expression", "error", issues.Err())
		return nil, fmt.Errorf("CEL compilation failed: %w", issues.Err())
	}

	slog.Info("Successfully compiled CEL expression", "expression", expression)

	return ast, nil
}

func (e *Environment) Evaluate(ast *cel.Ast, resource any, ctx context.Context) (ref.Val, error) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()

	e.ctx = ctx

	prg, err := e.env.Program(ast)
	if err != nil {
		slog.Error("Failed to create CEL program", "error", err)
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	result, _, err := prg.ContextEval(ctx, map[string]any{
		"resource": resource,
		"now":      time.Now(),
	})
	if err != nil {
		slog.Error("Failed to evaluate CEL expression", "error", err)
		return nil, fmt.Errorf("CEL evaluation failed: %w", err)
	}

	slog.Info("Successfully evaluated CEL expression", "result", result)

	return result, nil
}

func (e *Environment) lookup(args ...ref.Val) ref.Val {
	if len(args) != 4 {
		slog.Error("Lookup requires 4 arguments: version, kind, namespace, name")
		return types.NewErr("lookup requires 4 arguments: version, kind, namespace, name")
	}

	version, ok := args[0].(types.String)
	if !ok {
		slog.Error("Lookup arg[0] (version) must be string")
		return types.NewErr("lookup arg[0] (version) must be string")
	}

	kind, ok := args[1].(types.String)
	if !ok {
		slog.Error("Lookup arg[1] (kind) must be string")
		return types.NewErr("lookup arg[1] (kind) must be string")
	}

	namespace, ok := args[2].(types.String)
	if !ok {
		slog.Error("Lookup arg[2] (namespace) must be string")
		return types.NewErr("lookup arg[2] (namespace) must be string")
	}

	name, ok := args[3].(types.String)
	if !ok {
		slog.Error("Lookup arg[3] (name) must be string")
		return types.NewErr("lookup arg[3] (name) must be string")
	}

	ctx := e.ctx

	obj := &unstructured.Unstructured{}

	obj.SetAPIVersion(string(version))
	obj.SetKind(string(kind))

	key := client.ObjectKey{
		Namespace: string(namespace),
		Name:      string(name),
	}

	if err := e.client.Get(ctx, key, obj); err != nil {
		slog.Error("Failed to get object using cached client", "error", err)
		return types.NullValue
	}

	slog.Info("Successfully got object using cached client", "object", obj.Object)

	return types.DefaultTypeAdapter.NativeToValue(obj.Object)
}
