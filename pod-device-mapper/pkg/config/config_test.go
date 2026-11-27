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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// The chart renders this from the default global.partialRemediationEntityTypes.
const gpuDefaultConfig = `
[partialRemediationEntityTypes]
[partialRemediationEntityTypes.GPU_UUID]
scopedActions = ["COMPONENT_RESET"]

[[partialRemediationEntityTypes.GPU_UUID.devices]]
resourceNames = ["nvidia.com/gpu", "nvidia.com/pgpu"]

[[partialRemediationEntityTypes.GPU_UUID.devices]]
deviceClasses = ["gpu.nvidia.com"]
driver = "gpu.nvidia.com"
entityValueAttribute = "uuid"
`

func TestLoadPartialRemediationEntityTypes_ChartDefault_ReturnsTheGPUEntry(t *testing.T) {
	entityTypes, err := LoadPartialRemediationEntityTypes(writeConfig(t, gpuDefaultConfig))
	require.NoError(t, err)
	assert.Equal(t, modeltest.GPUEntityTypes(), entityTypes)
}

func TestLoadPartialRemediationEntityTypes_InvalidInput_ReturnsError(t *testing.T) {
	for _, tt := range []struct {
		name, content, want string
	}{
		{"missing key", "otherKey = 1\n", ErrPartialRemediationEntityTypesMissing.Error()},
		{"invalid entry", "[partialRemediationEntityTypes.GPU_UUID]\nscopedActions = []\n", "has no devices"},
		{"invalid TOML", "[partialRemediationEntityTypes\n", "failed to decode TOML config"},
		{"driver with two attributes", `
[[partialRemediationEntityTypes.GPU_UUID.devices]]
driver = "gpu.nvidia.com"
deviceClasses = ["gpu.nvidia.com"]
entityValueAttribute = "uuid"

[[partialRemediationEntityTypes.GPU_UUID.devices]]
driver = "gpu.nvidia.com"
deviceClasses = ["mig.nvidia.com"]
entityValueAttribute = "serial"
`, "lists driver gpu.nvidia.com with entityValueAttribute uuid and serial"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadPartialRemediationEntityTypes(writeConfig(t, tt.content))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadPartialRemediationEntityTypes_NoPath_ReturnsError(t *testing.T) {
	_, err := LoadPartialRemediationEntityTypes("")
	require.ErrorContains(t, err, "--config is not set")
}

func TestLoadPartialRemediationEntityTypes_EmptyMap_IsValid(t *testing.T) {
	entityTypes, err := LoadPartialRemediationEntityTypes(writeConfig(t, "[partialRemediationEntityTypes]\n"))
	require.NoError(t, err)
	assert.Empty(t, entityTypes)
	assert.NotNil(t, entityTypes)
}
