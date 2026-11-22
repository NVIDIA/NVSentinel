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

package eventutil

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestFirstImpactedEntity_EntityOrder_SelectsFirstNonEmptyOfType(t *testing.T) {
	gpuA := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-aaaa"}
	gpuB := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-bbbb"}
	empty := &protos.Entity{EntityType: "GPU_UUID"}
	pci := &protos.Entity{EntityType: "PCI", EntityValue: "0000:01:00.0"}

	tests := []struct {
		name     string
		entities []*protos.Entity
		scope    string
		want     *protos.Entity
	}{
		{"two GPUs", []*protos.Entity{gpuA, gpuB}, "GPU_UUID", gpuA},
		{"reversed GPUs", []*protos.Entity{gpuB, gpuA}, "GPU_UUID", gpuB},
		{"empty then valid", []*protos.Entity{empty, gpuA}, "GPU_UUID", gpuA},
		{"valid then empty", []*protos.Entity{gpuA, empty}, "GPU_UUID", gpuA},
		{"PCI then GPU", []*protos.Entity{pci, gpuA}, "GPU_UUID", gpuA},
		{"GPU then PCI", []*protos.Entity{gpuA, pci}, "GPU_UUID", gpuA},
		{"caller selects PCI", []*protos.Entity{gpuA, pci}, "PCI", pci},
		{"nil then valid", []*protos.Entity{nil, gpuA}, "GPU_UUID", gpuA},
		{"empty GPU only", []*protos.Entity{empty}, "GPU_UUID", nil},
		{"other type only", []*protos.Entity{pci}, "GPU_UUID", nil},
		{"no entities", nil, "GPU_UUID", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &protos.HealthEvent{NodeName: "node-a", EntitiesImpacted: tt.entities}
			got, err := FirstImpactedEntity(event, tt.scope)
			if tt.want == nil {
				require.ErrorContains(t, err, "no non-empty impacted entity of type GPU_UUID")
				require.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.Same(t, tt.want, got)
		})
	}
}
