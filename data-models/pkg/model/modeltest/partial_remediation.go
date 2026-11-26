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

// Package modeltest holds fixtures for tests of the packages that use model.
package modeltest

import (
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// GPUEntityTypes returns the GPU_UUID entry that global.partialRemediationEntityTypes sets by default in
// the Helm chart. The chart is the only source of the default; components fail without the key.
func GPUEntityTypes() model.PartialRemediationEntityTypes {
	return model.PartialRemediationEntityTypes{
		model.GPUUUIDEntityType: {
			Devices: []model.PartialRemediationDevice{
				{ResourceNames: []string{"nvidia.com/gpu", "nvidia.com/pgpu"}},
				{
					Driver:               model.GPUDRADriverName,
					DeviceClasses:        []string{model.GPUDRADriverName},
					EntityValueAttribute: "uuid",
				},
			},
			ScopedActions: []string{protos.RecommendedAction_COMPONENT_RESET.String()},
		},
	}
}
