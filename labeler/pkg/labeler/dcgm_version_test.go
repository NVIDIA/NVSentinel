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
		{"nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04", dcgmVersion4_8_0Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.8.0-ubuntu24.04@sha256:1bc401768b79d7e70a8c42bd5d33bd74031b35b4b00ccc9d200545cfca9dd71d", dcgmVersion4_8_0Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.10.0-ubuntu24.04", dcgmVersion4_8_0Plus},
		{"nvcr.io/nvidia/cloud-native/dcgm:4.7-ubuntu24.04", dcgmVersion4},
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
		"4.8.0-plus":   dcgmVersion4_8_0Plus,
		" 4.8.0-plus ": dcgmVersion4_8_0Plus,
		"5.x":          "",
	}

	for in, want := range tests {
		assert.Equal(t, want, normalizeDCGMVersion(in), "input %q", in)
	}
}
