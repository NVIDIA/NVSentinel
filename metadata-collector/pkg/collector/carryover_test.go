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

package collector

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// previousMetadata is what a full collection wrote on an earlier run.
func previousMetadata() *model.GPUMetadata {
	return &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{UUID: "GPU-a", NUMANode: 0, NVLinks: []model.NVLink{
				{LinkID: 0, RemotePCIAddress: "0000:05:00.0", RemoteLinkID: 34},
				{LinkID: 1, RemotePCIAddress: "0000:06:00.0", RemoteLinkID: 35},
			}},
			{UUID: "GPU-b", NUMANode: 1},
		},
		NICTopology: map[string][]string{"mlx5_0": {"PIX", "SYS"}},
	}
}

// degradedMetadata is what a collection writes when both nvidia-smi steps were abandoned.
func degradedMetadata(uuids ...string) *model.GPUMetadata {
	metadata := &model.GPUMetadata{}
	for _, uuid := range uuids {
		metadata.GPUs = append(metadata.GPUs, model.GPUInfo{UUID: uuid, NUMANode: -1})
	}

	return metadata
}

func TestCarryOver(t *testing.T) {
	t.Run("topo restores nic_topology and NUMA nodes", func(t *testing.T) {
		current := degradedMetadata("GPU-a", "GPU-b")

		assert.True(t, CarryOver(previousMetadata(), current, []string{StepTopo}))
		assert.Equal(t, previousMetadata().NICTopology, current.NICTopology)
		assert.Equal(t, 0, current.GPUs[0].NUMANode)
		assert.Equal(t, 1, current.GPUs[1].NUMANode)
	})

	t.Run("nvlink fills only unknown IDs on the same link and remote device", func(t *testing.T) {
		current := degradedMetadata("GPU-a", "GPU-b")
		current.GPUs[0].NVLinks = []model.NVLink{
			{LinkID: 0, RemotePCIAddress: "0000:05:00.0", RemoteLinkID: -1},
			{LinkID: 1, RemotePCIAddress: "0000:07:00.0", RemoteLinkID: -1}, // now goes elsewhere
			{LinkID: 2, RemotePCIAddress: "0000:05:00.0", RemoteLinkID: -1}, // new
		}

		assert.True(t, CarryOver(previousMetadata(), current, []string{StepNVLink}))
		assert.Equal(t, []int{34, -1, -1}, remoteLinkIDs(current.GPUs[0].NVLinks))
		assert.Empty(t, current.NICTopology, "topo was not abandoned")
		assert.Equal(t, -1, current.GPUs[0].NUMANode, "topo was not abandoned")
	})

	for name, uuids := range map[string][]string{
		"different GPU":    {"GPU-a", "GPU-c"},
		"different order":  {"GPU-b", "GPU-a"},
		"GPU count change": {"GPU-a"},
	} {
		t.Run(name+" leaves current unchanged", func(t *testing.T) {
			current := degradedMetadata(uuids...)

			assert.False(t, CarryOver(previousMetadata(), current, []string{StepTopo, StepNVLink}))
			assert.Equal(t, degradedMetadata(uuids...), current)
		})
	}
}

func remoteLinkIDs(links []model.NVLink) []int {
	ids := make([]int, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.RemoteLinkID)
	}

	return ids
}
