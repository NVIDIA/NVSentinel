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

package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
)

const namespaceRules = "[[userNamespaces]]\nname = \"*\"\nmode = \"AllowCompletion\"\n"

// renderedEntityTypes is what the node-drainer chart renders from global.partialRemediationEntityTypes with
// Helm's toToml, for the default GPU_UUID entry and one custom entity type.
const renderedEntityTypes = `
[partialRemediationEntityTypes]
  [partialRemediationEntityTypes.ACCEL_SERIAL]
    scopedActions = ["accel-reset"]

    [[partialRemediationEntityTypes.ACCEL_SERIAL.devices]]
      deviceClasses = ["accel.example.com"]
      driver = "accel.example.com"
      entityValueAttribute = "serial"
  [partialRemediationEntityTypes.GPU_UUID]
    scopedActions = ["COMPONENT_RESET"]

    [[partialRemediationEntityTypes.GPU_UUID.devices]]
      resourceNames = ["nvidia.com/gpu", "nvidia.com/pgpu"]

    [[partialRemediationEntityTypes.GPU_UUID.devices]]
      deviceClasses = ["gpu.nvidia.com"]
      driver = "gpu.nvidia.com"
      entityValueAttribute = "uuid"
`

func TestLoadTomlConfigFromString_PartialRemediationEntityTypes(t *testing.T) {
	accel := model.PartialRemediationEntityType{
		Devices: []model.PartialRemediationDevice{{
			Driver:               "accel.example.com",
			DeviceClasses:        []string{"accel.example.com"},
			EntityValueAttribute: "serial",
		}},
		ScopedActions: []string{"accel-reset"},
	}

	tests := []struct {
		name    string
		input   string
		want    model.PartialRemediationEntityTypes
		wantErr string
	}{
		{
			name:    "absent key is rejected",
			input:   namespaceRules,
			wantErr: ErrPartialRemediationEntityTypesMissing.Error(),
		},
		{
			name:  "rendered map is decoded",
			input: namespaceRules + renderedEntityTypes,
			want: model.PartialRemediationEntityTypes{
				model.GPUUUIDEntityType: modeltest.GPUEntityTypes()[model.GPUUUIDEntityType],
				"ACCEL_SERIAL":          accel,
			},
		},
		{
			name:  "empty table configures no entity type",
			input: namespaceRules + "[partialRemediationEntityTypes]\n",
			want:  model.PartialRemediationEntityTypes{},
		},
		{
			name: "invalid entry is rejected",
			input: namespaceRules + `
[partialRemediationEntityTypes.ACCEL_SERIAL]
  scopedActions = ["accel-reset"]
  [[partialRemediationEntityTypes.ACCEL_SERIAL.devices]]
    resourceNames = ["example.com/accel"]
    driver = "accel.example.com"
`,
			wantErr: "invalid partialRemediationEntityTypes: entity type ACCEL_SERIAL, devices[0]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadTomlConfigFromString(tt.input)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.PartialRemediationEntityTypes)
		})
	}
}

// loadTestConfig loads a configuration for tests that are not about partialRemediationEntityTypes. It adds the
// key, which the Helm chart always renders, as an empty table.
func loadTestConfig(doc string) (*TomlConfig, error) {
	return LoadTomlConfigFromString(doc + "\n[partialRemediationEntityTypes]\n")
}
