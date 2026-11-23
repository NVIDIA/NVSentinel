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

package labeler

import (
	"fmt"
	"reflect"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"github.com/Masterminds/semver/v3"
)

// semverType is the CEL type of a semantic version. The functions follow the Kubernetes CEL semver library:
// semver(string), <Semver>.major(), .minor(), .patch(), .isLessThan(<Semver>), .isGreaterThan(<Semver>) and
// .compareTo(<Semver>).
var semverType = cel.ObjectType("nvsentinel.Semver")

// celSemver is the CEL value of a semantic version.
type celSemver struct {
	*semver.Version
}

func (v celSemver) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if reflect.TypeOf(v.Version).AssignableTo(typeDesc) {
		return v.Version, nil
	}

	if reflect.TypeOf("").AssignableTo(typeDesc) {
		return v.String(), nil
	}

	return nil, fmt.Errorf("type conversion error from 'Semver' to '%v'", typeDesc)
}

func (v celSemver) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case semverType:
		return v
	case types.TypeType:
		return semverType
	default:
		return types.NewErr("type conversion error from '%s' to '%s'", semverType, typeVal)
	}
}

func (v celSemver) Equal(other ref.Val) ref.Val {
	o, ok := other.(celSemver)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}

	return types.Bool(v.Version.Equal(o.Version))
}

func (v celSemver) Type() ref.Type {
	return semverType
}

func (v celSemver) Value() any {
	return v.Version
}

// semverLib returns the CEL library with the semver type and functions.
func semverLib() cel.EnvOption {
	return cel.Lib(semverLibrary{})
}

type semverLibrary struct{}

func (semverLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("semver",
			cel.Overload("string_to_semver", []*cel.Type{cel.StringType}, semverType,
				cel.UnaryBinding(stringToSemver))),
		cel.Function("major",
			cel.MemberOverload("semver_major", []*cel.Type{semverType}, cel.IntType,
				cel.UnaryBinding(semverPart(func(v *semver.Version) uint64 { return v.Major() })))),
		cel.Function("minor",
			cel.MemberOverload("semver_minor", []*cel.Type{semverType}, cel.IntType,
				cel.UnaryBinding(semverPart(func(v *semver.Version) uint64 { return v.Minor() })))),
		cel.Function("patch",
			cel.MemberOverload("semver_patch", []*cel.Type{semverType}, cel.IntType,
				cel.UnaryBinding(semverPart(func(v *semver.Version) uint64 { return v.Patch() })))),
		cel.Function("isLessThan",
			cel.MemberOverload("semver_is_less_than", []*cel.Type{semverType, semverType}, cel.BoolType,
				cel.BinaryBinding(semverCompare(func(c int) ref.Val { return types.Bool(c < 0) })))),
		cel.Function("isGreaterThan",
			cel.MemberOverload("semver_is_greater_than", []*cel.Type{semverType, semverType}, cel.BoolType,
				cel.BinaryBinding(semverCompare(func(c int) ref.Val { return types.Bool(c > 0) })))),
		cel.Function("compareTo",
			cel.MemberOverload("semver_compare_to", []*cel.Type{semverType, semverType}, cel.IntType,
				cel.BinaryBinding(semverCompare(func(c int) ref.Val { return types.Int(c) })))),
	}
}

func (semverLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

func stringToSemver(arg ref.Val) ref.Val {
	s, ok := arg.Value().(string)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	v, err := semver.NewVersion(s)
	if err != nil {
		return types.WrapErr(fmt.Errorf("semver(%q): %w", s, err))
	}

	return celSemver{v}
}

func semverPart(part func(*semver.Version) uint64) func(ref.Val) ref.Val {
	return func(arg ref.Val) ref.Val {
		v, ok := arg.(celSemver)
		if !ok {
			return types.MaybeNoSuchOverloadErr(arg)
		}

		return types.Int(part(v.Version)) //nolint:gosec // DCGM version parts are small.
	}
}

func semverCompare(result func(int) ref.Val) func(ref.Val, ref.Val) ref.Val {
	return func(lhs, rhs ref.Val) ref.Val {
		v, ok := lhs.(celSemver)
		if !ok {
			return types.MaybeNoSuchOverloadErr(lhs)
		}

		o, ok := rhs.(celSemver)
		if !ok {
			return types.MaybeNoSuchOverloadErr(rhs)
		}

		return result(v.Compare(o.Version))
	}
}
