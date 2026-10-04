// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package celfields

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/require"
)

// testResourceVar and testLookupFunc mirror what kubernetes-object-monitor
// binds, so the shipped policies in these tests read as they do in production.
const (
	testResourceVar = "resource"
	testLookupFunc  = "lookup"
	testNodeVar     = "node"
)

// newTestEnv returns an environment declaring both object variables the
// callers of this package bind, plus the lookup() function and the string
// extensions fault-quarantine enables.
func newTestEnv(t *testing.T) *cel.Env {
	t.Helper()

	env, err := cel.NewEnv(
		cel.Variable(testResourceVar, cel.DynType),
		cel.Variable(testNodeVar, cel.DynType),
		cel.Variable("now", cel.TimestampType),
		cel.Function(testLookupFunc,
			cel.Overload("lookup_string_string_string_string",
				[]*cel.Type{cel.StringType, cel.StringType, cel.StringType, cel.StringType},
				cel.DynType,
			),
		),
		ext.Strings(),
	)
	require.NoError(t, err)

	return env
}

// compile parses and checks expression, failing the test if it does not build.
func compile(t *testing.T, env *cel.Env, expression string) *cel.Ast {
	t.Helper()

	compiled, issues := env.Compile(expression)
	require.NoError(t, issues.Err())

	return compiled
}
