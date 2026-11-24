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

package nic

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeIBDevice creates a fake /sys/class/infiniband/<device> entry
// whose device/uevent file carries the given contents.
func writeIBDevice(t *testing.T, ibClassPath, device, uevent string) {
	t.Helper()

	deviceDir := filepath.Join(ibClassPath, device, "device")
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "uevent"), []byte(uevent), 0o644))
}

func TestSnapshotPCIAddresses(t *testing.T) {
	ibClassPath := t.TempDir()

	writeIBDevice(t, ibClassPath, "mlx5_0", "DRIVER=mlx5_core\nPCI_SLOT_NAME=0000:0c:00.0\n")
	writeIBDevice(t, ibClassPath, "mlx5_1", "DRIVER=mlx5_core\nPCI_SLOT_NAME=0000:9A:00.0\n")
	// A device whose uevent has no PCI_SLOT_NAME is skipped, not fatal.
	writeIBDevice(t, ibClassPath, "mlx5_2", "DRIVER=mlx5_core\n")

	addresses, err := SnapshotPCIAddresses(ibClassPath)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"mlx5_0": "0000:0c:00.0",
		"mlx5_1": "0000:9a:00.0",
	}, addresses)
}

func TestSnapshotPCIAddresses_MissingDirectory(t *testing.T) {
	_, err := SnapshotPCIAddresses(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
}

func TestPCIAddressOfDevice_NoSlotName(t *testing.T) {
	ibClassPath := t.TempDir()
	writeIBDevice(t, ibClassPath, "mlx5_0", "DRIVER=mlx5_core\n")

	_, err := PCIAddressOfDevice(ibClassPath, "mlx5_0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PCI_SLOT_NAME not found")
}

func TestAddressesForNames(t *testing.T) {
	snapshot := map[string]string{
		"mlx5_0": "0000:0c:00.0",
		"mlx5_1": "0000:18:00.0",
		"mlx5_2": "0000:1f:00.0",
	}

	assert.Equal(t, map[string]string{
		"mlx5_0": "0000:0c:00.0",
		"mlx5_2": "0000:1f:00.0",
	}, AddressesForNames(snapshot, []string{"mlx5_0", "mlx5_2", "mlx5_9"}))
}

func TestAddressesChangedForNames(t *testing.T) {
	before := map[string]string{
		"mlx5_7":  "0000:41:00.0",
		"mlx5_10": "0000:9a:00.0",
	}

	tests := []struct {
		name    string
		after   map[string]string
		devices []string
		want    bool
	}{
		{
			name:    "no change",
			after:   map[string]string{"mlx5_7": "0000:41:00.0", "mlx5_10": "0000:9a:00.0"},
			devices: []string{"mlx5_7", "mlx5_10"},
			want:    false,
		},
		{
			name:    "names swapped by a rename",
			after:   map[string]string{"mlx5_7": "0000:9a:00.0", "mlx5_10": "0000:41:00.0"},
			devices: []string{"mlx5_7", "mlx5_10"},
			want:    true,
		},
		{
			name:    "device appeared",
			after:   map[string]string{"mlx5_7": "0000:41:00.0", "mlx5_10": "0000:9a:00.0", "mlx5_11": "0000:a5:00.0"},
			devices: []string{"mlx5_7", "mlx5_10", "mlx5_11"},
			want:    true,
		},
		{
			name:    "device disappeared",
			after:   map[string]string{"mlx5_7": "0000:41:00.0"},
			devices: []string{"mlx5_7", "mlx5_10"},
			want:    true,
		},
		{
			name:    "change outside the watched names is ignored",
			after:   map[string]string{"mlx5_7": "0000:41:00.0", "mlx5_10": "0000:9a:00.0", "mlx5_3": "0000:2a:00.0"},
			devices: []string{"mlx5_7", "mlx5_10"},
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AddressesChangedForNames(before, tc.after, tc.devices))
		})
	}
}
