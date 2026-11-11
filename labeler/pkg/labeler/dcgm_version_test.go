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
)

func TestDCGMVersionFromImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"nvcr.io/nvidia/cloud-native/dcgm:3.3.9-1-ubuntu22.04", dcgmVersion3},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.5.2-1-ubuntu22.04", dcgmVersion4},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04", dcgmVersion4},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04@sha256:c7e042a939a5c0d4d588fff28f9371d9d101e66eece7b3f831cf5d44fa182fc8", dcgmVersion4},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7.1-ubuntu24.04", dcgmVersion4_7_1Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7.2-1-ubuntu24.04", dcgmVersion4_7_1Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04", dcgmVersion4_7_1Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.10.0-ubuntu24.04", dcgmVersion4_7_1Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7-ubuntu24.04", dcgmVersion4},
		{"nvcr.io/nvidia/cloud-native/dcgm:4-latest", ""},
		{"nvcr.io/nvidia/cloud-native/dcgm-exporter:4.7.1-4.8.0-ubuntu22.04", ""},
		{"nvcr.io/nvidia/cloud-native/dcgm@sha256:2fc0b3091e54f2d96c592b16e2fd16e326f378651a7ed01eeda909f22cbfd57c", ""},
		{"busybox:1.36", ""},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			assert.Equal(t, tt.want, dcgmVersionFromImage(tt.image))
		})
	}
}

func TestNormalizeDCGMVersion(t *testing.T) {
	tests := map[string]string{
		"3.x":          dcgmVersion3,
		"4.x":          dcgmVersion4,
		"4.7.1-plus":   dcgmVersion4_7_1Plus,
		" 4.7.1-plus ": dcgmVersion4_7_1Plus,
		"4.7.1+":       "",
		"5.x":          "",
		"":             "",
	}

	for in, want := range tests {
		assert.Equal(t, want, normalizeDCGMVersion(in), "input %q", in)
	}
}
