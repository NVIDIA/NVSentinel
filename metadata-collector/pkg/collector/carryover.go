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
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// CarryOver fills the fields that an abandoned nvidia-smi step left empty with their values
// from previous, the metadata file of an earlier run. Writing them empty would break the
// consumers: nic-health-monitor refuses to start without nic_topology or a GPU with a valid
// NUMA node, and syslog-health-monitor keys NVSwitch links by remote link ID.
//
// It returns false and leaves current unchanged unless previous lists the same GPUs, by UUID,
// in the same order, since nic_topology rows are aligned to the gpus slice.
func CarryOver(previous, current *model.GPUMetadata, steps []string) bool {
	if !sameGPUs(previous.GPUs, current.GPUs) {
		return false
	}

	for _, step := range steps {
		switch step {
		case StepTopo:
			current.NICTopology = previous.NICTopology

			for i := range current.GPUs {
				current.GPUs[i].NUMANode = previous.GPUs[i].NUMANode
			}
		case StepNVLink:
			for i := range current.GPUs {
				carryOverRemoteLinkIDs(current.GPUs[i].NVLinks, previous.GPUs[i].NVLinks)
			}
		}
	}

	return true
}

func sameGPUs(previous, current []model.GPUInfo) bool {
	if len(previous) != len(current) {
		return false
	}

	for i := range current {
		if previous[i].UUID != current[i].UUID {
			return false
		}
	}

	return true
}

// carryOverRemoteLinkIDs fills each unknown remote link ID from the previous link with the
// same local link and remote device. A link that is new or now goes elsewhere stays unknown.
func carryOverRemoteLinkIDs(current, previous []model.NVLink) {
	for i := range current {
		if current[i].RemoteLinkID != -1 {
			continue
		}

		for _, prev := range previous {
			if prev.LinkID == current[i].LinkID && prev.RemotePCIAddress == current[i].RemotePCIAddress {
				current[i].RemoteLinkID = prev.RemoteLinkID
				break
			}
		}
	}
}
