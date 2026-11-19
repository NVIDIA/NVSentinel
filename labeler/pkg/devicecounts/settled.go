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

package devicecounts

import (
	"context"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
)

// compileSettledExpression compiles the settledExpression of a class. It
// returns a nil program when the class has none.
func compileSettledExpression(env *cel.Env, prefix, expression string) (cel.Program, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, nil
	}

	return compileExpression(env, prefix, "settledExpression", expression, isSettledType, "bool")
}

func isSettledType(outputType *cel.Type) bool {
	return outputType.Kind() == types.BoolKind || outputType.Kind() == types.DynKind
}

// settled reports whether the settledExpression of the class returns true for
// node. A class without one treats every node as settled.
func (class compiledClass) settled(
	ctx context.Context,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (bool, error) {
	if class.settledProgram == nil {
		return true, nil
	}

	result, err := evaluateExpression(ctx, class.settledProgram, "settledExpression", node, resourceSlices)
	if err != nil {
		return false, fmt.Errorf("evaluate settled state: %w", err)
	}

	settled, ok := result.(types.Bool)
	if !ok {
		return false, fmt.Errorf("settledExpression returned non-bool: %T", result.Value())
	}

	return bool(settled), nil
}
