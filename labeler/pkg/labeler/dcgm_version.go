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
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

// dcgmImageVersionRegex matches the version core (major, optional minor, optional patch) in a DCGM image tag. The
// rest of the tag, for example the package revision in 4.8.0-1-ubuntu24.04, is not semver and is ignored. If it
// were parsed as a semver prerelease, 4.8.0-1 would sort below 4.8.0.
var dcgmImageVersionRegex = regexp.MustCompile(`dcgm:(\d+(?:\.\d+){0,2})`)

// DCGMVersionLane sets the dcgm.version label value to Label when Expression is true. Expression is a CEL
// expression with one variable, version: the Semver from the DCGM image tag.
type DCGMVersionLane struct {
	Label      string `toml:"label"`
	Expression string `toml:"expression"`
}

// dcgmVersionLanesConfig is the TOML file format: a [[lanes]] table per lane.
type dcgmVersionLanesConfig struct {
	Lanes []DCGMVersionLane `toml:"lanes"`
}

// DCGMVersionLanes are validated lanes with compiled expressions, in configuration order.
type DCGMVersionLanes struct {
	lanes []dcgmVersionLane
}

type dcgmVersionLane struct {
	label   string
	program cel.Program
}

// LoadDCGMVersionLanes loads and validates the DCGM version lanes from a TOML file. The order of the lanes is the
// order of evaluation: the first lane whose expression is true wins.
func LoadDCGMVersionLanes(path string) (DCGMVersionLanes, error) {
	if path == "" {
		return DCGMVersionLanes{}, errors.New("DCGM version lanes config file is required")
	}

	var config dcgmVersionLanesConfig

	metadata, err := configmanager.LoadTOMLConfigWithMetadata(path, &config)
	if err != nil {
		return DCGMVersionLanes{}, fmt.Errorf("parse DCGM version lanes: %w", err)
	}

	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		return DCGMVersionLanes{}, fmt.Errorf("parse DCGM version lanes: unknown field %q", undecoded[0].String())
	}

	return newDCGMVersionLanes(config.Lanes)
}

func newDCGMVersionLanes(input []DCGMVersionLane) (DCGMVersionLanes, error) {
	if len(input) == 0 {
		return DCGMVersionLanes{}, errors.New("DCGM version lanes: at least one lane is required")
	}

	env, err := cel.NewEnv(semverLib(), cel.Variable("version", semverType))
	if err != nil {
		return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: create CEL environment: %w", err)
	}

	lanes := make([]dcgmVersionLane, 0, len(input))

	for _, lane := range input {
		if lane.Label == "" || len(validation.IsValidLabelValue(lane.Label)) > 0 {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: label %q must be a non-empty label value",
				lane.Label)
		}

		if slices.ContainsFunc(lanes, func(l dcgmVersionLane) bool { return l.label == lane.Label }) {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: duplicate label %q", lane.Label)
		}

		program, err := compileLaneExpression(env, lane.Expression)
		if err != nil {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: lane %q: %w", lane.Label, err)
		}

		lanes = append(lanes, dcgmVersionLane{label: lane.Label, program: program})
	}

	return DCGMVersionLanes{lanes: lanes}, nil
}

func compileLaneExpression(env *cel.Env, expression string) (cel.Program, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, errors.New("expression is required")
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile expression %q: %w", expression, issues.Err())
	}

	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression must return bool, got %s", out)
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("create program for expression %q: %w", expression, err)
	}

	return program, nil
}

// labelForImage returns the label of the first lane whose expression is true for the DCGM image. It
// returns "" if the image is not a DCGM image or no expression is true. A missing minor or patch version counts as 0.
// A lane whose expression fails or does not return bool is skipped.
func (l DCGMVersionLanes) labelForImage(image string) string {
	m := dcgmImageVersionRegex.FindStringSubmatch(image)
	if m == nil {
		return ""
	}

	version, err := semver.NewVersion(m[1])
	if err != nil {
		slog.Warn("Cannot parse DCGM version from image", "image", image, "error", err)
		return ""
	}

	vars := map[string]any{"version": celSemver{version}}

	for _, lane := range l.lanes {
		result, _, err := lane.program.Eval(vars)
		if err != nil || types.IsUnknownOrError(result) {
			slog.Warn("DCGM version lane expression failed; skipping lane",
				"lane", lane.label, "image", image, "error", err)

			continue
		}

		matched, ok := result.Value().(bool)
		if !ok {
			slog.Warn("DCGM version lane expression did not return bool; skipping lane",
				"lane", lane.label, "image", image, "type", result.Type())

			continue
		}

		if matched {
			return lane.label
		}
	}

	return ""
}

// isLabel reports whether value is the label of a lane.
func (l DCGMVersionLanes) isLabel(value string) bool {
	value = strings.TrimSpace(value)

	return slices.ContainsFunc(l.lanes, func(lane dcgmVersionLane) bool { return lane.label == value })
}
