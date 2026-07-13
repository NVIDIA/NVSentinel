// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package discovery

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

func TestDiscoverDevices_InclusionOverrideHasHighestPriority(t *testing.T) {
	reader := &sysfs.MockReader{
		ListDirsFunc: func(path string) ([]string, error) {
			if path == "/sys/class/infiniband" {
				return []string{"mlx5_forced", "mlx5_normal"}, nil
			}

			if strings.HasSuffix(path, "/ports") {
				return []string{}, nil
			}

			return nil, nil
		},
		IsVirtualFunctionFunc: func(device string) bool {
			return device == "mlx5_forced"
		},
	}

	result, err := DiscoverDevicesWithOverride(reader, "^mlx5_forced$", "^mlx5_forced$")
	require.NoError(t, err)
	require.Len(t, result.Devices, 1)
	assert.Equal(t, "mlx5_forced", result.Devices[0].Name)
	assert.True(t, result.Devices[0].IsVF)
	assert.True(t, result.Devices[0].IncludedByOverride)
	assert.Zero(t, result.SkippedVFs)
}

func TestDiscoverDevices_UsesNormalFiltersWithoutInclusionOverride(t *testing.T) {
	reader := &sysfs.MockReader{
		ListDirsFunc: func(path string) ([]string, error) {
			if path == "/sys/class/infiniband" {
				return []string{"mlx5_excluded", "mlx5_vf", "mlx5_normal"}, nil
			}

			if strings.HasSuffix(path, "/ports") {
				return []string{}, nil
			}

			return nil, nil
		},
		IsVirtualFunctionFunc: func(device string) bool {
			return device == "mlx5_vf"
		},
	}

	result, err := DiscoverDevices(reader, "^mlx5_excluded$")
	require.NoError(t, err)
	require.Len(t, result.Devices, 1)
	assert.Equal(t, "mlx5_normal", result.Devices[0].Name)
	assert.False(t, result.Devices[0].IncludedByOverride)
	assert.Equal(t, 1, result.SkippedVFs)
}

func TestDiscoverDevices_EmptyInclusionPatternListUsesNormalFilters(t *testing.T) {
	reader := &sysfs.MockReader{
		ListDirsFunc: func(path string) ([]string, error) {
			if path == "/sys/class/infiniband" {
				return []string{"mlx5_excluded", "mlx5_vf", "mlx5_normal"}, nil
			}

			if strings.HasSuffix(path, "/ports") {
				return []string{}, nil
			}

			return nil, nil
		},
		IsVirtualFunctionFunc: func(device string) bool {
			return device == "mlx5_vf"
		},
	}

	result, err := DiscoverDevicesWithOverride(reader, "^mlx5_excluded$", ", ,")
	require.NoError(t, err)
	require.Len(t, result.Devices, 1)
	assert.Equal(t, "mlx5_normal", result.Devices[0].Name)
	assert.False(t, result.Devices[0].IncludedByOverride)
	assert.Equal(t, 1, result.SkippedVFs)
}
