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

package topology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// writeMetadata writes a gpu_metadata.json file to a temp path and returns it.
func writeMetadata(t *testing.T, meta *model.GPUMetadata) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "gpu_metadata.json")

	data, err := json.MarshalIndent(meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	return path
}

// readerForTest returns a MockReader primed with NUMA mappings.
//
//   - nicNUMA maps device name → NUMA node (applied to ReadIBDeviceNUMANode)
//   - pciAddr maps device name → its PCI address (for ReadPCIAddress)
func readerForTest(nicNUMA map[string]int, pciAddr map[string]string) sysfs.Reader {
	return &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			v, ok := nicNUMA[device]
			if !ok {
				return -1, nil
			}

			return v, nil
		},
		ReadPCIAddressFunc: func(device string) (string, error) {
			addr, ok := pciAddr[device]
			if !ok {
				return "", nil
			}

			return addr, nil
		},
	}
}

func TestLoadFromMetadata_MissingFile(t *testing.T) {
	_, err := LoadFromMetadata("/no/such/path/gpu_metadata.json", &sysfs.MockReader{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read gpu metadata")
}

func TestLoadFromMetadata_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{ this is not json"), 0o644))

	_, err := LoadFromMetadata(path, &sysfs.MockReader{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse gpu metadata")
}

func TestLoadFromMetadata_EmptyGPUs(t *testing.T) {
	path := writeMetadata(t, &model.GPUMetadata{
		NICTopology: map[string][]string{"mlx5_0": {"PIX"}},
	})

	_, err := LoadFromMetadata(path, &sysfs.MockReader{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty gpus[]")
}

func TestLoadFromMetadata_EmptyNICTopology(t *testing.T) {
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{{PCIAddress: "0000:01:00.0", NUMANode: 0}},
	})

	_, err := LoadFromMetadata(path, &sysfs.MockReader{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty nic_topology")
}

func TestLoadFromMetadata_SuccessPopulatesNUMASet(t *testing.T) {
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0000:0f:00.0", NUMANode: 0},
			{PCIAddress: "0000:15:00.0", NUMANode: 1},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"PIX", "SYS"},
		},
	})

	reader := readerForTest(map[string]int{}, map[string]string{})

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)
	assert.Contains(t, c.gpuNUMASet, 0)
	assert.Contains(t, c.gpuNUMASet, 1)
}

func TestClassify_H100DGX(t *testing.T) {
	// 8 GPUs on NUMAs 0 (GPUs 0-3) and 1 (GPUs 4-7).
	// mlx5_0 is compute (PIX to GPU0), mlx5_8 is storage (NODE to GPU0),
	// mlx5_mgmt is on NUMA 2 (no GPU) with all SYS → management.
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0000:0f:00.0", NUMANode: 0},
			{PCIAddress: "0000:15:00.0", NUMANode: 1},
		},
		NICTopology: map[string][]string{
			"mlx5_0":    {"PIX", "SYS"},
			"mlx5_8":    {"NODE", "NODE"},
			"mlx5_mgmt": {"SYS", "SYS"},
		},
	})

	reader := readerForTest(
		map[string]int{"mlx5_0": 0, "mlx5_8": 0, "mlx5_mgmt": 2},
		map[string]string{},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)

	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_0"))
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_8"))
	assert.Equal(t, RoleManagement, c.RoleOf("mlx5_mgmt"))
	assert.True(t, c.IsManagementNIC("mlx5_mgmt"))
	assert.False(t, c.IsManagementNIC("mlx5_0"))
}

func TestClassify_GB200AllSYSBecomesStorage(t *testing.T) {
	// All NICs share NUMA with GPUs; every GPU↔NIC cell is SYS.
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0009:01:00.0", NUMANode: 0},
			{PCIAddress: "0019:01:00.0", NUMANode: 0},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"SYS", "SYS"},
			"mlx5_1": {"SYS", "SYS"},
		},
	})

	reader := readerForTest(
		map[string]int{"mlx5_0": 0, "mlx5_1": 0},
		map[string]string{},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)

	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_0"))
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_1"))
}

func TestClassify_GB200BlueFieldDPUExcluded(t *testing.T) {
	// On GB200, all topo cells are SYS and all NICs share NUMA with
	// GPUs. ConnectX-7 IB NICs (MT4129) should be Compute (IB link
	// layer promotion); BlueField-3 DPUs (MT41692) should be excluded.
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0008:01:00.0", NUMANode: 0},
			{PCIAddress: "0009:01:00.0", NUMANode: 0},
			{PCIAddress: "0018:01:00.0", NUMANode: 1},
			{PCIAddress: "0019:01:00.0", NUMANode: 1},
		},
		NICTopology: map[string][]string{
			"ibp3s0":      {"SYS", "SYS", "SYS", "SYS"},
			"ibP2p3s0":    {"SYS", "SYS", "SYS", "SYS"},
			"ibP16p3s0":   {"SYS", "SYS", "SYS", "SYS"},
			"ibP18p3s0":   {"SYS", "SYS", "SYS", "SYS"},
			"roceP6p3s0":  {"SYS", "SYS", "SYS", "SYS"},
			"roceP22p3s0": {"SYS", "SYS", "SYS", "SYS"},
		},
	})

	reader := &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			switch device {
			case "ibp3s0", "ibP2p3s0", "roceP6p3s0":
				return 0, nil
			case "ibP16p3s0", "ibP18p3s0", "roceP22p3s0":
				return 1, nil
			default:
				return -1, nil
			}
		},
		ReadIBDeviceFieldFunc: func(device, field string) (string, error) {
			if field != "hca_type" {
				return "", nil
			}

			switch device {
			case "ibp3s0", "ibP2p3s0", "ibP16p3s0", "ibP18p3s0":
				return "MT4129", nil
			case "roceP6p3s0", "roceP22p3s0":
				return "MT41692", nil
			default:
				return "", nil
			}
		},
		ReadIBPortLinkLayerFunc: func(device string, port int) (string, error) {
			switch device {
			case "ibp3s0", "ibP2p3s0", "ibP16p3s0", "ibP18p3s0":
				return "InfiniBand", nil
			case "roceP6p3s0", "roceP22p3s0":
				return "Ethernet", nil
			default:
				return "", nil
			}
		},
	}

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)

	assert.Equal(t, RoleCompute, c.RoleOf("ibp3s0"))
	assert.Equal(t, RoleCompute, c.RoleOf("ibP2p3s0"))
	assert.Equal(t, RoleCompute, c.RoleOf("ibP16p3s0"))
	assert.Equal(t, RoleCompute, c.RoleOf("ibP18p3s0"))

	assert.Equal(t, RoleManagement, c.RoleOf("roceP6p3s0"))
	assert.Equal(t, RoleManagement, c.RoleOf("roceP22p3s0"))
}

func TestClassify_OnPremL40S_IBComputeEthManagement(t *testing.T) {
	// On-prem L40S: 4 IB compute fabric NICs (NODE to GPUs) + 1 Ethernet
	// management NIC (carries default route). No PIX/PXB.
	routePath := writeProcNetRoute(t, "eth0")

	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0000:16:00.0", NUMANode: 0},
			{PCIAddress: "0000:38:00.0", NUMANode: 0},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"NODE", "NODE"},
			"mlx5_1": {"NODE", "NODE"},
			"mlx5_2": {"NODE", "NODE"},
			"mlx5_3": {"NODE", "NODE"},
			"mlx5_4": {"NODE", "NODE"},
		},
	})

	reader := &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			return 0, nil
		},
		ReadIBPortLinkLayerFunc: func(device string, port int) (string, error) {
			if device == "mlx5_0" {
				return "Ethernet", nil
			}

			return "InfiniBand", nil
		},
		ListDirsFunc: func(path string) ([]string, error) {
			if strings.Contains(path, "eth0/device/infiniband") {
				return []string{"mlx5_0"}, nil
			}

			return nil, nil
		},
	}

	c, err := LoadFromMetadata(path, reader, routePath)
	require.NoError(t, err)

	assert.Equal(t, RoleManagement, c.RoleOf("mlx5_0"), "Ethernet mgmt NIC with default route → Management")
	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_1"), "IB NIC with NODE → Compute (link-layer promotion)")
	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_2"), "IB NIC with NODE → Compute")
	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_3"), "IB NIC with NODE → Compute")
	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_4"), "IB NIC with NODE → Compute")
}

func TestClassify_DeviceMissingFromTopologyUsesNUMADefault(t *testing.T) {
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{{PCIAddress: "0000:0f:00.0", NUMANode: 0}},
		NICTopology: map[string][]string{
			"mlx5_0": {"PIX"},
		},
	})

	reader := readerForTest(
		map[string]int{"mlx5_0": 0, "mlx5_ghost": 5},
		map[string]string{},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)

	// Device not in topology + NIC on non-GPU NUMA → Management.
	assert.Equal(t, RoleManagement, c.RoleOf("mlx5_ghost"))

	// Device not in topology + NIC on GPU NUMA → Storage.
	reader2 := readerForTest(
		map[string]int{"mlx5_other": 0},
		map[string]string{},
	)

	c2, err := LoadFromMetadata(path, reader2)
	require.NoError(t, err)
	assert.Equal(t, RoleStorage, c2.RoleOf("mlx5_other"))
}

func TestClassify_ComputeOverridesNUMAGate(t *testing.T) {
	// NIC on a non-GPU NUMA but has a PIX relationship → Compute.
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{{PCIAddress: "0000:0f:00.0", NUMANode: 0}},
		NICTopology: map[string][]string{
			"mlx5_edge": {"PIX"},
		},
	})

	reader := readerForTest(
		map[string]int{"mlx5_edge": 5},
		map[string]string{},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)
	assert.Equal(t, RoleCompute, c.RoleOf("mlx5_edge"))
}

func TestPCICardOfStripsFunction(t *testing.T) {
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{{PCIAddress: "0000:01:00.0", NUMANode: 0}},
		NICTopology: map[string][]string{
			"mlx5_3": {"PIX"},
			"mlx5_4": {"PIX"},
		},
	})

	reader := readerForTest(
		map[string]int{"mlx5_3": 0, "mlx5_4": 0},
		map[string]string{
			"mlx5_3": "0000:47:00.0",
			"mlx5_4": "0000:47:00.1",
		},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)
	assert.Equal(t, "0000:47:00", c.PCICardOf("mlx5_3"))
	assert.Equal(t, "0000:47:00", c.PCICardOf("mlx5_4"))
}

func TestCheckCardHomogeneity(t *testing.T) {
	// Build a minimal classifier just to exercise the algorithm.
	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{{PCIAddress: "0000:01:00.0", NUMANode: 0}},
		NICTopology: map[string][]string{
			"mlx5_0": {"PIX"},
		},
	})
	reader := readerForTest(
		map[string]int{"mlx5_0": 0},
		map[string]string{},
	)

	c, err := LoadFromMetadata(path, reader)
	require.NoError(t, err)

	// 8 compute cards all have 2 active ports (mode=2); one card has 1.
	cardActive := map[string]int{
		"0000:01:00": 2, "0000:02:00": 2, "0000:03:00": 2, "0000:04:00": 2,
		"0000:05:00": 2, "0000:06:00": 2, "0000:07:00": 2,
		"0000:08:00": 1, // anomalous
	}
	cardTotal := map[string]int{
		"0000:01:00": 2, "0000:02:00": 2, "0000:03:00": 2, "0000:04:00": 2,
		"0000:05:00": 2, "0000:06:00": 2, "0000:07:00": 2, "0000:08:00": 2,
	}
	cardRole := make(map[string]Role)

	for card := range cardTotal {
		cardRole[card] = RoleCompute
	}

	anomalies := c.CheckCardHomogeneity(cardActive, cardTotal, cardRole)
	require.Len(t, anomalies, 1)

	a, ok := anomalies["0000:08:00"]
	require.True(t, ok)
	assert.Equal(t, 1, a.ActiveSeen)
	assert.Equal(t, 2, a.ExpectedModeCount)
	assert.Equal(t, RoleCompute, a.Role)
}

func TestSummarizeLevels(t *testing.T) {
	hasCompute, hasStorage, allSYS := summarizeLevels([]string{"PIX", "SYS"})
	assert.True(t, hasCompute)
	assert.False(t, hasStorage)
	assert.False(t, allSYS)

	hasCompute, hasStorage, allSYS = summarizeLevels([]string{"NODE", "SYS"})
	assert.False(t, hasCompute)
	assert.True(t, hasStorage)
	assert.False(t, allSYS)

	hasCompute, hasStorage, allSYS = summarizeLevels([]string{"SYS", "SYS", "SYS"})
	assert.False(t, hasCompute)
	assert.False(t, hasStorage)
	assert.True(t, allSYS)

	hasCompute, hasStorage, allSYS = summarizeLevels([]string{})
	assert.False(t, hasCompute)
	assert.False(t, hasStorage)
	assert.False(t, allSYS)
}

// writeProcNetRoute creates a fake /proc/net/route file.
func writeProcNetRoute(t *testing.T, defaultIface string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "route")

	content := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
	if defaultIface != "" {
		content += defaultIface + "\t00000000\t01000A0A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	}

	content += "rdma0\t0000100A\t00000000\t0001\t0\t0\t0\tF0FF0000\t0\t0\t0\n"

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	return path
}

func TestClassify_DefaultRouteExcluded(t *testing.T) {
	// Azure A100-style: mlx5_8 is Ethernet management NIC carrying the
	// default route (interface eth0). Without default-route exclusion it
	// would be Storage (NODE to GPUs, on GPU NUMA). With it, Management.
	routePath := writeProcNetRoute(t, "eth0")

	reader := &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			return 0, nil
		},
		ListDirsFunc: func(path string) ([]string, error) {
			if strings.Contains(path, "eth0/device/infiniband") {
				return []string{"mlx5_8"}, nil
			}

			return nil, nil
		},
	}

	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0001:00:00.0", NUMANode: 0},
			{PCIAddress: "0002:00:00.0", NUMANode: 0},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"NODE", "NODE"},
			"mlx5_1": {"NODE", "NODE"},
			"mlx5_8": {"NODE", "NODE"},
		},
	})

	c, err := LoadFromMetadata(path, reader, routePath)
	require.NoError(t, err)

	assert.Equal(t, "mlx5_8", c.defaultRouteDevice)
	assert.Equal(t, RoleManagement, c.RoleOf("mlx5_8"), "default-route NIC must be Management")
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_0"), "non-default-route NIC must be Storage")
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_1"), "non-default-route NIC must be Storage")
}

func TestClassify_DefaultRouteNoMatchDoesNotExclude(t *testing.T) {
	// The default route interface (eth0) has no IB device backing it
	// (e.g., it's a virtio NIC). No device should be excluded.
	routePath := writeProcNetRoute(t, "eth0")

	reader := &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			return 0, nil
		},
		ListDirsFunc: func(path string) ([]string, error) {
			return nil, nil
		},
	}

	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0001:00:00.0", NUMANode: 0},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"NODE"},
		},
	})

	c, err := LoadFromMetadata(path, reader, routePath)
	require.NoError(t, err)

	assert.Empty(t, c.defaultRouteDevice)
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_0"))
}

func TestClassify_NoProcRouteDisablesExclusion(t *testing.T) {
	reader := &sysfs.MockReader{
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			return 0, nil
		},
	}

	path := writeMetadata(t, &model.GPUMetadata{
		GPUs: []model.GPUInfo{
			{PCIAddress: "0001:00:00.0", NUMANode: 0},
		},
		NICTopology: map[string][]string{
			"mlx5_0": {"NODE"},
		},
	})

	c, err := LoadFromMetadata(path, reader, "/no/such/file")
	require.NoError(t, err)

	assert.Empty(t, c.defaultRouteDevice)
	assert.Equal(t, RoleStorage, c.RoleOf("mlx5_0"))
}
