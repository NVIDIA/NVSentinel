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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDCGMVersionLanes are the chart default lanes, for tests that create a Labeler.
var testDCGMVersionLanes = func() DCGMVersionLanes {
	lanes, err := newDCGMVersionLanes([]DCGMVersionLane{
		{Label: "3.x", Expression: "version.major() == 3"},
		{Label: "4.x", Expression: "version.major() == 4 && version.isLessThan(semver('4.8.0'))"},
		{Label: "48.x", Expression: "!version.isLessThan(semver('4.8.0'))"},
	})
	if err != nil {
		panic(err)
	}

	return lanes
}()

func writeLanesFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "dcgm-version-lanes.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestDCGMVersionLanes_LabelForImage_DefaultLanes(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "version below the lowest lane gets no label",
			image: "nvcr.io/nvidia/cloud-native/dcgm:2.4.6-1-ubuntu20.04",
			want:  "",
		},
		{
			name:  "version 3.x maps to 3.x",
			image: "nvcr.io/nvidia/cloud-native/dcgm:3.3.9-1-ubuntu22.04",
			want:  "3.x",
		},
		{
			name:  "version below 4.8.0 maps to 4.x",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04",
			want:  "4.x",
		},
		{
			name:  "version 4.8.0 maps to 48.x",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04",
			want:  "48.x",
		},
		{
			name:  "package revision in the tag is not a prerelease",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8.0-1-ubuntu24.04",
			want:  "48.x",
		},
		{
			name:  "minor versions compare as numbers",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.10.0-ubuntu24.04",
			want:  "48.x",
		},
		{
			name:  "version 5.x maps to 48.x",
			image: "nvcr.io/nvidia/cloud-native/dcgm:5.0.0-ubuntu24.04",
			want:  "48.x",
		},
		{
			name:  "tag without patch version gets patch 0",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8-ubuntu24.04",
			want:  "48.x",
		},
		{
			name:  "tag with major version only gets minor and patch 0",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4-ubuntu24.04",
			want:  "4.x",
		},
		{
			name:  "image that is not DCGM gets no label",
			image: "nvcr.io/nvidia/k8s-device-plugin:v0.17.0",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, testDCGMVersionLanes.labelForImage(tt.image))
		})
	}
}

func TestDCGMVersionLanes_LabelForImage_FirstMatchingLaneWins(t *testing.T) {
	lanes, err := newDCGMVersionLanes([]DCGMVersionLane{
		{Label: "481-broken", Expression: "version.compareTo(semver('4.8.1')) == 0"},
		{Label: "48.x", Expression: "version.major() == 4 && version.minor() >= 8"},
		{Label: "4.x", Expression: "version.major() == 4"},
	})
	require.NoError(t, err)

	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04"))
	assert.Equal(t, "48.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04"))
	assert.Equal(t, "481-broken", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.1-ubuntu24.04"))
}

func TestDCGMVersionLanes_LabelForImage_EvaluationErrorSkipsLane(t *testing.T) {
	lanes, err := newDCGMVersionLanes([]DCGMVersionLane{
		{Label: "bad", Expression: "version.isLessThan(semver('not-a-version'))"},
		{Label: "4.x", Expression: "version.major() == 4"},
	})
	require.NoError(t, err)

	// semver() fails on the invalid string at runtime, so the first lane is skipped.
	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04"))
}

func TestDCGMVersionLanes_SemverFunctions(t *testing.T) {
	tests := []struct {
		expression string
		want       string
	}{
		{expression: "version.major() == 4 && version.minor() == 8 && version.patch() == 1", want: "match"},
		{expression: "version.isGreaterThan(semver('4.8.0'))", want: "match"},
		{expression: "version.isGreaterThan(semver('4.8.1'))", want: ""},
		{expression: "version.isLessThan(semver('4.10.0'))", want: "match"},
		{expression: "version.compareTo(semver('4.8.1')) == 0", want: "match"},
		{expression: "version.compareTo(semver('4.9')) == -1", want: "match"},
		{expression: "version.compareTo(semver('4.8.0')) == 1", want: "match"},
		{expression: "version == semver('4.8.1')", want: "match"},
	}

	for _, tt := range tests {
		t.Run(tt.expression, func(t *testing.T) {
			lanes, err := newDCGMVersionLanes([]DCGMVersionLane{{Label: "match", Expression: tt.expression}})
			require.NoError(t, err)

			assert.Equal(t, tt.want, lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.1-1-ubuntu24.04"))
		})
	}
}

func TestDCGMVersionLanes_IsLabel(t *testing.T) {
	assert.True(t, testDCGMVersionLanes.isLabel("48.x"))
	assert.True(t, testDCGMVersionLanes.isLabel(" 4.x "))
	assert.False(t, testDCGMVersionLanes.isLabel("48"))
	assert.False(t, testDCGMVersionLanes.isLabel(""))
}

func TestLoadDCGMVersionLanes_ValidFile_ReturnsLanesInOrder(t *testing.T) {
	path := writeLanesFile(t, `
[[lanes]]
label = "4.x"
expression = '''
version.major() == 4 && version.isLessThan(semver('4.8.0'))
'''

[[lanes]]
label = "48.x"
expression = '''
!version.isLessThan(semver('4.8.0'))
'''
`)

	lanes, err := LoadDCGMVersionLanes(path)
	require.NoError(t, err)

	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04"))
	assert.Equal(t, "48.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04"))
}

func TestLoadDCGMVersionLanes_InvalidFile_ReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "empty file",
			content: "",
			wantErr: "at least one lane",
		},
		{
			name:    "not TOML",
			content: "[[lanes]\n",
			wantErr: "parse DCGM version lanes",
		},
		{
			name:    "unknown field",
			content: "[[lanes]]\nminVersion = \"4.0.0\"\nlabel = \"4.x\"\nexpression = \"true\"\n",
			wantErr: `unknown field "lanes.minVersion"`,
		},
		{
			name:    "unquoted numeric label",
			content: "[[lanes]]\nlabel = 48\nexpression = \"true\"\n",
			wantErr: "parse DCGM version lanes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadDCGMVersionLanes(writeLanesFile(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadDCGMVersionLanes_MissingPath_ReturnsError(t *testing.T) {
	_, err := LoadDCGMVersionLanes("")
	require.ErrorContains(t, err, "DCGM version lanes config file is required")

	_, err = LoadDCGMVersionLanes(filepath.Join(t.TempDir(), "missing.toml"))
	require.ErrorContains(t, err, "parse DCGM version lanes")
}

func TestNewDCGMVersionLanes_InvalidLanes_ReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		lanes   []DCGMVersionLane
		wantErr string
	}{
		{
			name:    "no lanes",
			lanes:   nil,
			wantErr: "at least one lane",
		},
		{
			name:    "label is not a valid label value",
			lanes:   []DCGMVersionLane{{Label: "4.8+", Expression: "true"}},
			wantErr: `label "4.8+"`,
		},
		{
			name:    "duplicate label",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "true"}, {Label: "48.x", Expression: "false"}},
			wantErr: `duplicate label "48.x"`,
		},
		{
			name:    "missing expression",
			lanes:   []DCGMVersionLane{{Label: "48.x"}},
			wantErr: `lane "48.x": expression is required`,
		},
		{
			name:    "expression does not compile",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "version.major() >="}},
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression uses the old map fields",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "version.major == 4"}},
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression compares a version with a string",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "version.isLessThan('4.8.0')"}},
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression uses an unknown variable",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "dcgm.major() == 4"}},
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression reads the node",
			lanes:   []DCGMVersionLane{{Label: "canary", Expression: "node.metadata.labels['pool'] == 'canary'"}},
			wantErr: `lane "canary": compile expression`,
		},
		{
			name:    "expression reads the image",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "image.contains('ubuntu24.04')"}},
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression does not return bool",
			lanes:   []DCGMVersionLane{{Label: "48.x", Expression: "version.major()"}},
			wantErr: `lane "48.x": expression must return bool`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newDCGMVersionLanes(tt.lanes)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
