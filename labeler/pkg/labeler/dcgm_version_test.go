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
		`{"minVersion":"3.0.0","label":"3.x"},` +
		`{"minVersion":"4.0.0","label":"4.x"},` +
		`{"minVersion":"4.8.0","label":"48"}]`)
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
			name:  "version below 4.8.0 maps to 4.x",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04",
			want:  "4.x",
		},
		{
			name:  "version 4.8.0 maps to 48",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04",
			want:  "48",
		},
		{
			name:  "minor versions compare as numbers",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.10.0-ubuntu24.04",
			want:  "48",
		},
		{
			name:  "version above the highest lane maps to the highest lane",
			image: "nvcr.io/nvidia/cloud-native/dcgm:5.0.0-ubuntu24.04",
			want:  "48",
		},
		{
			name:  "tag without patch version maps to the major version lane",
			image: "nvcr.io/nvidia/cloud-native/dcgm:4.8-ubuntu24.04",
			want:  "4.x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, testDCGMVersionLanes.labelForImage(tt.image))
		})
	}
}

func TestParseDCGMVersionLanes_UnsortedLanes_UsesHighestMinVersionAtOrBelowImage(t *testing.T) {
	lanes, err := ParseDCGMVersionLanes(`[
		{"minVersion": "4.8.0", "label": "48"},
		{"minVersion": "4.0.0", "label": "4.x"},
		{"minVersion": "4.9.2", "label": "492"}
	]`)
	require.NoError(t, err)

	assert.Equal(t, "4.x", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04"))
	assert.Equal(t, "48", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.9.1-ubuntu24.04"))
	assert.Equal(t, "492", lanes.labelForImage("nvcr.io/nvidia/cloud-native/dcgm:4.9.2-ubuntu24.04"))
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
			lanes:   `[{"minVersion": "4.0.0", "maxVersion": "4.8.0", "label": "4.x"}]`,
			wantErr: "unknown field",
		},
		{
			name:    "minVersion is not X.Y.Z",
			lanes:   `[{"minVersion": "4.8", "label": "48"}]`,
			wantErr: `minVersion "4.8"`,
		},
		{
			name:    "unquoted numeric label",
			lanes:   `[{"minVersion": "4.8.0", "label": 48}]`,
			wantErr: "parse DCGM version lanes",
		},
		{
			name:    "label is not a valid label value",
			lanes:   `[{"minVersion": "4.8.0", "label": "4.8+"}]`,
			wantErr: `label "4.8+"`,
		},
		{
			name:    "duplicate minVersion",
			lanes:   `[{"minVersion": "4.8.0", "label": "48"}, {"minVersion": "4.8.0", "label": "49"}]`,
			wantErr: `duplicate minVersion "4.8.0"`,
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
