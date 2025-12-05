// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package controller

import (
	"testing"

	drainv1alpha1 "github.com/nvidia/nvsentinel/plugins/slinky-drainer/api/v1alpha1"
)

func TestBuildCordonReason(t *testing.T) {
	tests := []struct {
		name     string
		dr       *drainv1alpha1.DrainRequest
		expected string
	}{
		{
			name: "Single XID error with single GPU",
			dr: &drainv1alpha1.DrainRequest{
				Spec: drainv1alpha1.DrainRequestSpec{
					ErrorCode: []string{"79"},
					EntitiesImpacted: []drainv1alpha1.EntityImpacted{
						{Type: "GPU", Value: "0"},
					},
					Reason: "GPU has fallen off the bus",
				},
			},
			expected: "[J] 79 GPU:0 - GPU has fallen off the bus",
		},
		{
			name: "Single SXID error with NVSwitch",
			dr: &drainv1alpha1.DrainRequest{
				Spec: drainv1alpha1.DrainRequestSpec{
					ErrorCode: []string{"20034"},
					EntitiesImpacted: []drainv1alpha1.EntityImpacted{
						{Type: "NVSwitch", Value: "3"},
						{Type: "NVLink", Value: "5"},
					},
					Reason: "LTSSM Fault Up",
				},
			},
			expected: "[J] 20034 NVSwitch:3,NVLink:5 - LTSSM Fault Up",
		},
		{
			name: "Non-XID error code",
			dr: &drainv1alpha1.DrainRequest{
				Spec: drainv1alpha1.DrainRequestSpec{
					ErrorCode: []string{"DCGM_FR_VOLATILE_DBE_DETECTED"},
					EntitiesImpacted: []drainv1alpha1.EntityImpacted{
						{Type: "GPU", Value: "7"},
					},
					Reason: "Detected 1 volatile double-bit ECC error(s) in GPU 7. Drain the GPU and reset it or reboot the node",
				},
			},
			expected: "[J] DCGM_FR_VOLATILE_DBE_DETECTED GPU:7 - Detected 1 volatile double-bit ECC error(s) in GPU 7. Drain the GPU and reset it or reboot the node",
		},
		{
			name: "No error code, use checkName as fallback",
			dr: &drainv1alpha1.DrainRequest{
				Spec: drainv1alpha1.DrainRequestSpec{
					CheckName: "HealthCheck",
					EntitiesImpacted: []drainv1alpha1.EntityImpacted{
						{Type: "GPU", Value: "0"},
					},
					Reason: "",
				},
			},
			expected: "[J] GPU:0 - HealthCheck",
		},
		{
			name: "Minimal info",
			dr: &drainv1alpha1.DrainRequest{
				Spec: drainv1alpha1.DrainRequestSpec{},
			},
			expected: "[J] - Health check failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildCordonReason(tt.dr)
			if result != tt.expected {
				t.Errorf("buildCordonReason() = %q, want %q", result, tt.expected)
			}
		})
	}
}
