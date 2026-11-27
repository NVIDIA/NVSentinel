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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nic"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/writer"
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
	gpuErrors             map[int]error
	gpuInfoCalls          []int
	topologyCalls         []int
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
	f.gpuInfoCalls = append(f.gpuInfoCalls, index)
	if err := f.gpuErrors[index]; err != nil {
		return nil, err
	}

	return &model.GPUInfo{
		GPUID:      index,
		UUID:       fmt.Sprintf("GPU-test-%d", index),
		PCIAddress: fmt.Sprintf("0000:%02x:00.0", index+1),
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
	index int,
	_ map[string]gonvml.Device,
	_ map[int]nvml.GPUNVLinkTopology,
) (map[string]struct{}, error) {
	f.topologyCalls = append(f.topologyCalls, index)

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

func TestAlignTopologyToGPUs(t *testing.T) {
	gpus := func(ids ...int) *model.GPUMetadata {
		m := &model.GPUMetadata{}
		for _, id := range ids {
			m.GPUs = append(m.GPUs, model.GPUInfo{GPUID: id})
		}

		return m
	}

	tests := []struct {
		name        string
		labels      []string
		collected   *model.GPUMetadata
		deviceCount int
		want        map[int]int
		wantErr     string
	}{
		{name: "full match", labels: []string{"GPU0", "GPU1"}, collected: gpus(0, 1), deviceCount: 2,
			want: map[int]int{0: 0, 1: 1}},
		{name: "column for a skipped GPU is kept", labels: []string{"GPU0", "GPU1", "GPU2", "GPU3"},
			collected: gpus(0, 2, 3), deviceCount: 4, want: map[int]int{0: 0, 1: 1, 2: 2, 3: 3}},
		{name: "equal counts but different labels", labels: []string{"GPU0", "GPU1", "GPU2"},
			collected: gpus(0, 2, 3), deviceCount: 4, wantErr: "no column for GPU3"},
		{name: "column NVML does not report", labels: []string{"GPU0", "GPU1"}, collected: gpus(0), deviceCount: 1,
			wantErr: "not a GPU NVML reports"},
		{name: "collected GPU missing from matrix", labels: []string{"GPU0"}, collected: gpus(0, 1), deviceCount: 2,
			wantErr: "no column for GPU1"},
		{name: "malformed label", labels: []string{"GPU0", "NIC1"}, collected: gpus(0), deviceCount: 2,
			wantErr: "not of the form GPU<n>"},
		{name: "duplicate label", labels: []string{"GPU0", "GPU0"}, collected: gpus(0), deviceCount: 2,
			wantErr: "more than once"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := alignTopologyToGPUs(&nic.TopoMatrix{GPUs: tc.labels}, tc.collected, tc.deviceCount)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCollect_GPUIdentityFailures_KeepsHealthyGPUs(t *testing.T) {
	tests := []struct {
		name    string
		errors  map[int]error
		wantIDs []int
	}{
		{name: "happy path", wantIDs: []int{0, 1, 2, 3}},
		{name: "lost GPU", errors: map[int]error{1: gonvml.ERROR_GPU_IS_LOST}, wantIDs: []int{0, 2, 3}},
		{name: "insufficient power", errors: map[int]error{1: gonvml.ERROR_INSUFFICIENT_POWER}, wantIDs: []int{0, 2, 3}},
		{name: "IRQ issue", errors: map[int]error{1: gonvml.ERROR_IRQ_ISSUE}, wantIDs: []int{0, 2, 3}},
		{name: "first GPU lost", errors: map[int]error{0: gonvml.ERROR_GPU_IS_LOST}, wantIDs: []int{1, 2, 3}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serial := "CHASSIS-1234"
			fake := &fakeNVMLClient{
				deviceCount: 4, driverVersion: "560.35.03", chassisSerial: &serial, gpuErrors: tc.errors,
			}
			counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_gpus_skipped_total"})
			topo := &fakeNICTopoCollector{matrix: &nic.TopoMatrix{
				GPUs:          []string{"GPU0", "GPU1", "GPU2", "GPU3"},
				GPUNUMANodes:  []int{0, 1, 2, 3},
				NICs:          []string{"mlx5_0"},
				Relationships: map[string][]string{"mlx5_0": {"PIX", "PXB", "NODE", "SYS"}},
			}}

			metadata, err := NewCollector(fake, topo, WithSkippedGPUCounter(counter)).Collect(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []int{0, 1, 2, 3}, fake.gpuInfoCalls)
			assert.Equal(t, tc.wantIDs, fake.topologyCalls)
			assert.Equal(t, float64(len(tc.errors)), testutil.ToFloat64(counter))
			assert.Equal(t, &serial, metadata.ChassisSerial)
			assert.Equal(t, 1, fake.getChassisSerialCalls)

			path := filepath.Join(t.TempDir(), "gpu_metadata.json")
			w, err := writer.NewWriter(path)
			require.NoError(t, err)
			require.NoError(t, w.Write(metadata))

			data, err := os.ReadFile(path)
			require.NoError(t, err)

			var written model.GPUMetadata
			require.NoError(t, json.Unmarshal(data, &written))
			require.Len(t, written.GPUs, len(tc.wantIDs))

			for i, id := range tc.wantIDs {
				assert.Equal(t, id, written.GPUs[i].GPUID)
				assert.Equal(t, fmt.Sprintf("GPU-test-%d", id), written.GPUs[i].UUID)
				assert.Equal(t, fmt.Sprintf("0000:%02x:00.0", id+1), written.GPUs[i].PCIAddress)
			}

			// A skipped GPU keeps its matrix column, so the NIC relationships stay intact
			// and each surviving GPU gets the NUMA value of its own labeled column.
			// Each relationship list is aligned with written.GPUs, per the shared model.
			levels := topo.matrix.Relationships["mlx5_0"]
			want := make([]string, 0, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				want = append(want, levels[id])
			}
			assert.Equal(t, map[string][]string{"mlx5_0": want}, written.NICTopology)

			for _, gpu := range written.GPUs {
				assert.Equal(t, gpu.GPUID, gpu.NUMANode)
			}
		})
	}
}

func TestCollect_AllGPUsLost_ReturnsError(t *testing.T) {
	fake := &fakeNVMLClient{
		deviceCount: 2, driverVersion: "560.35.03",
		gpuErrors: map[int]error{0: gonvml.ERROR_GPU_IS_LOST, 1: gonvml.ERROR_GPU_IS_LOST},
	}
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_gpus_skipped_total"})
	topo := &fakeNICTopoCollector{}

	metadata, err := NewCollector(fake, topo, WithSkippedGPUCounter(counter)).Collect(t.Context())
	require.ErrorContains(t, err, "could not query identity for any of 2 GPUs")
	assert.Nil(t, metadata)
	assert.Equal(t, []int{0, 1}, fake.gpuInfoCalls)
	assert.Empty(t, fake.topologyCalls)
	assert.Equal(t, 0, topo.calls)
	assert.Equal(t, float64(2), testutil.ToFloat64(counter))
}

func TestCollect_ZeroGPUs_ReturnsEmptyInventory(t *testing.T) {
	fake := &fakeNVMLClient{driverVersion: "560.35.03"}

	metadata, err := NewCollector(fake, &fakeNICTopoCollector{}).Collect(t.Context())
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.Empty(t, metadata.GPUs)
	assert.Empty(t, fake.gpuInfoCalls)
}
