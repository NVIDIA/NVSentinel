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

package collector

import (
	"context"
	"fmt"
	"testing"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nic"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
)

type fakeNICTopoCollector struct {
	matrix *nic.TopoMatrix
	err    error
	calls  int
}

func (f *fakeNICTopoCollector) Collect(context.Context) (*nic.TopoMatrix, error) {
	f.calls++
	return f.matrix, f.err
}

// fakeNVMLClient is a test double for the nvmlClient interface.
type fakeNVMLClient struct {
	deviceCount           int
	driverVersion         string
	chassisSerial         *string
	getChassisSerialCalls int
}

// GetDeviceCount returns the configured device count.
func (f *fakeNVMLClient) GetDeviceCount() (int, error) {
	return f.deviceCount, nil
}

// GetDriverVersion returns the configured driver version string.
func (f *fakeNVMLClient) GetDriverVersion() (string, error) {
	return f.driverVersion, nil
}

// BuildDeviceMap returns an empty device map.
func (f *fakeNVMLClient) BuildDeviceMap() (map[string]gonvml.Device, error) {
	return map[string]gonvml.Device{}, nil
}

// ParseNVLinkTopologyWithContext returns an empty topology map.
func (f *fakeNVMLClient) ParseNVLinkTopologyWithContext(context.Context) (map[int]nvml.GPUNVLinkTopology, error) {
	return map[int]nvml.GPUNVLinkTopology{}, nil
}

// GetGPUInfo returns a stub GPUInfo for the given index.
func (f *fakeNVMLClient) GetGPUInfo(index int) (*model.GPUInfo, error) {
	return &model.GPUInfo{
		GPUID:      index,
		UUID:       "GPU-test",
		PCIAddress: "0000:01:00.0",
		DeviceName: "NVIDIA Test GPU",
		NVLinks:    []model.NVLink{},
	}, nil
}

// GetChassisSerial records the call and returns the configured serial.
func (f *fakeNVMLClient) GetChassisSerial(index int) *string {
	f.getChassisSerialCalls++
	return f.chassisSerial
}

// CollectNVLinkTopology returns an empty NVSwitch set.
func (f *fakeNVMLClient) CollectNVLinkTopology(
	_ *model.GPUInfo,
	_ int,
	_ map[string]gonvml.Device,
	_ map[int]nvml.GPUNVLinkTopology,
) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// TestSupportsChassisSerial validates driver version gating for chassis serial collection.
func TestSupportsChassisSerial(t *testing.T) {
	tests := []struct {
		name          string
		driverVersion string
		expected      bool
	}{
		{name: "older R550 driver", driverVersion: "550.54.15", expected: false},
		{name: "older R559 driver", driverVersion: "559.99.99", expected: false},
		{name: "minimum supported R560 driver", driverVersion: "560.0.0", expected: true},
		{name: "newer driver", driverVersion: "575.51.02", expected: true},
		{name: "major version only", driverVersion: "560", expected: true},
		{name: "driver version with whitespace", driverVersion: " 560.35.03 ", expected: true},
		{name: "empty version", driverVersion: "", expected: false},
		{name: "invalid version", driverVersion: "invalid", expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, supportsChassisSerial(tc.driverVersion))
		})
	}
}

// TestCollectSkipsChassisSerialOnUnsupportedDrivers verifies that pre-R560 drivers do not call GetChassisSerial.
func TestCollectSkipsChassisSerialOnUnsupportedDrivers(t *testing.T) {
	serial := "CHASSIS-1234"
	fake := &fakeNVMLClient{
		deviceCount:   1,
		driverVersion: "550.54.15",
		chassisSerial: &serial,
	}

	collector := NewCollector(fake, &fakeNICTopoCollector{})

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metadata.ChassisSerial)
	assert.Equal(t, 0, fake.getChassisSerialCalls)
}

// TestCollectCollectsChassisSerialOnSupportedDrivers verifies that R560+ drivers populate chassis serial.
func TestCollectCollectsChassisSerialOnSupportedDrivers(t *testing.T) {
	serial := "CHASSIS-1234"
	fake := &fakeNVMLClient{
		deviceCount:   1,
		driverVersion: "560.35.03",
		chassisSerial: &serial,
	}

	collector := NewCollector(fake, &fakeNICTopoCollector{})

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	require.NotNil(t, metadata.ChassisSerial)
	assert.Equal(t, serial, *metadata.ChassisSerial)
	assert.Equal(t, 1, fake.getChassisSerialCalls)
}

// TestCollectPopulatesNICTopology verifies that the NIC topo matrix is
// copied verbatim into GPUMetadata.NICTopology and that GPU NUMA nodes
// are populated from the topo matrix.
func TestCollectPopulatesNICTopology(t *testing.T) {
	fake := &fakeNVMLClient{
		deviceCount:   2,
		driverVersion: "560.35.03",
	}

	matrix := &nic.TopoMatrix{
		GPUs:         []string{"GPU0", "GPU1"},
		NICs:         []string{"mlx5_0", "mlx5_1"},
		GPUNUMANodes: []int{0, 1},
		Relationships: map[string][]string{
			"mlx5_0": {"PIX", "SYS"},
			"mlx5_1": {"SYS", "PIX"},
		},
	}

	topo := &fakeNICTopoCollector{matrix: matrix}

	collector := NewCollector(fake, topo)

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, topo.calls)
	assert.Equal(t, matrix.Relationships, metadata.NICTopology)
	assert.Equal(t, 0, metadata.GPUs[0].NUMANode)
	assert.Equal(t, 1, metadata.GPUs[1].NUMANode)
}

// TestCollectTopoFailureLeavesTopologyEmpty verifies the collector does
// not abort when `nvidia-smi topo -m` fails.
func TestCollectTopoFailureLeavesTopologyEmpty(t *testing.T) {
	fake := &fakeNVMLClient{deviceCount: 1, driverVersion: "560.35.03"}
	topo := &fakeNICTopoCollector{err: fmt.Errorf("nvidia-smi not available")}

	collector := NewCollector(fake, topo)

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metadata.NICTopology)
}

// TestCollectNoNICsLeavesTopologyEmptyButPopulatesNUMA verifies that a
// topo matrix with GPUs but no NICs still populates gpus[].numa_node
// while leaving nic_topology empty.
func TestCollectNoNICsLeavesTopologyEmptyButPopulatesNUMA(t *testing.T) {
	fake := &fakeNVMLClient{deviceCount: 1, driverVersion: "560.35.03"}
	topo := &fakeNICTopoCollector{matrix: &nic.TopoMatrix{
		GPUs:         []string{"GPU0"},
		GPUNUMANodes: []int{0},
	}}

	collector := NewCollector(fake, topo)

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metadata.NICTopology)
	assert.Equal(t, 0, metadata.GPUs[0].NUMANode, "NUMA must be populated even without NICs")
}

// TestCollectEmptyTopoCollectorLeavesFieldsEmpty verifies that a topo
// collector returning nil matrix leaves topology fields empty.
func TestCollectEmptyTopoCollectorLeavesFieldsEmpty(t *testing.T) {
	fake := &fakeNVMLClient{deviceCount: 1, driverVersion: "560.35.03"}

	collector := NewCollector(fake, &fakeNICTopoCollector{})

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metadata.NICTopology)
	assert.Equal(t, -1, metadata.GPUs[0].NUMANode)
}

// TestCollectGPUCountMismatchDropsTopology verifies that when the topo
// matrix reports a different GPU count than NVML, nic_topology and
// numa_node are dropped rather than silently misaligned.
func TestCollectGPUCountMismatchDropsTopology(t *testing.T) {
	fake := &fakeNVMLClient{deviceCount: 1, driverVersion: "560.35.03"}
	matrix := &nic.TopoMatrix{
		GPUs:         []string{"GPU0", "GPU1"},
		NICs:         []string{"mlx5_0"},
		GPUNUMANodes: []int{0, 1},
		Relationships: map[string][]string{
			"mlx5_0": {"PIX", "SYS"},
		},
	}

	collector := NewCollector(fake, &fakeNICTopoCollector{matrix: matrix})

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metadata.NICTopology, "topology must be dropped on GPU count mismatch")
	assert.Equal(t, -1, metadata.GPUs[0].NUMANode, "NUMA must stay -1 when topology is dropped")
}

func TestCheckGPUCountMismatch(t *testing.T) {
	err := checkGPUCountMismatch(
		&nic.TopoMatrix{GPUs: []string{"GPU0", "GPU1"}},
		&model.GPUMetadata{GPUs: []model.GPUInfo{{GPUID: 0}, {GPUID: 1}}},
	)
	assert.NoError(t, err, "equal counts must produce no error")

	err = checkGPUCountMismatch(
		&nic.TopoMatrix{GPUs: []string{"GPU0", "GPU1"}},
		&model.GPUMetadata{GPUs: []model.GPUInfo{{GPUID: 0}, {GPUID: 1}, {GPUID: 2}}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 GPU columns")
	assert.Contains(t, err.Error(), "3 GPUs")

	err = checkGPUCountMismatch(
		&nic.TopoMatrix{GPUs: []string{"GPU0", "GPU1", "GPU2"}},
		&model.GPUMetadata{GPUs: []model.GPUInfo{{GPUID: 0}}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "3 GPU columns")
	assert.Contains(t, err.Error(), "1 GPUs")
}

