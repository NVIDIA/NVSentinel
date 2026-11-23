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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDCGMVersionLanes are the chart default lanes, for tests that create a Labeler.
var testDCGMVersionLanes = func() DCGMVersionLanes {
	lanes, err := ParseDCGMVersionLanes(`[` +
		`{"label":"3.x","expression":"version.major == 3"},` +
		`{"label":"4.x","expression":"version.major == 4 && version.minor < 8"},` +
		`{"label":"48.x","expression":"version.major > 4 || (version.major == 4 && version.minor >= 8)"}]`)
	if err != nil {
		panic(err)
	}

	return lanes
}()

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
			name:  "tag without patch version maps to the major version lane",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8-ubuntu24.04",
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
	lanes, err := ParseDCGMVersionLanes(`[
		{"label": "481-broken", "expression": "version.major == 4 && version.minor == 8 && version.patch == 1"},
		{"label": "48.x", "expression": "version.major == 4 && version.minor >= 8"},
		{"label": "4.x", "expression": "version.major == 4"}
	]`)
	require.NoError(t, err)

	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04"))
	assert.Equal(t, "48.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04"))
	assert.Equal(t, "481-broken", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.1-ubuntu24.04"))
}

func TestDCGMVersionLanes_LabelForImage_EvaluationErrorSkipsLane(t *testing.T) {
	lanes, err := ParseDCGMVersionLanes(`[
		{"label": "build", "expression": "version['build'] == 1"},
		{"label": "4.x", "expression": "version.major == 4"}
	]`)
	require.NoError(t, err)

	// version has no build key, so the first expression fails with "no such key".
	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04"))
}

func TestDCGMVersionLanes_IsLabel(t *testing.T) {
	assert.True(t, testDCGMVersionLanes.isLabel("48.x"))
	assert.True(t, testDCGMVersionLanes.isLabel(" 4.x "))
	assert.False(t, testDCGMVersionLanes.isLabel("48"))
	assert.False(t, testDCGMVersionLanes.isLabel(""))
}

func TestParseDCGMVersionLanes_InvalidConfig_ReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		lanes   string
		wantErr string
	}{
		{
			name:    "missing config",
			lanes:   "",
			wantErr: "DCGM version lanes are required",
		},
		{
			name:    "empty list",
			lanes:   `[]`,
			wantErr: "at least one lane",
		},
		{
			name:    "unknown field",
			lanes:   `[{"minVersion": "4.0.0", "label": "4.x", "expression": "true"}]`,
			wantErr: "unknown field",
		},
		{
			name:    "unquoted numeric label",
			lanes:   `[{"label": 48, "expression": "true"}]`,
			wantErr: "parse DCGM version lanes",
		},
		{
			name:    "label is not a valid label value",
			lanes:   `[{"label": "4.8+", "expression": "true"}]`,
			wantErr: `label "4.8+"`,
		},
		{
			name:    "duplicate label",
			lanes:   `[{"label": "48.x", "expression": "true"}, {"label": "48.x", "expression": "false"}]`,
			wantErr: `duplicate label "48.x"`,
		},
		{
			name:    "missing expression",
			lanes:   `[{"label": "48.x"}]`,
			wantErr: `lane "48.x": expression is required`,
		},
		{
			name:    "expression does not compile",
			lanes:   `[{"label": "48.x", "expression": "version.major >="}]`,
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression uses an unknown variable",
			lanes:   `[{"label": "48.x", "expression": "dcgm.major == 4"}]`,
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression reads the node",
			lanes:   `[{"label": "canary", "expression": "node.metadata.labels['pool'] == 'canary'"}]`,
			wantErr: `lane "canary": compile expression`,
		},
		{
			name:    "expression reads the image",
			lanes:   `[{"label": "48.x", "expression": "image.contains('ubuntu24.04')"}]`,
			wantErr: `lane "48.x": compile expression`,
		},
		{
			name:    "expression does not return bool",
			lanes:   `[{"label": "48.x", "expression": "version.major"}]`,
			wantErr: `lane "48.x": expression must return bool`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDCGMVersionLanes(tt.lanes)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
