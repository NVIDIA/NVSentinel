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
	"time"

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

// TestCollectPopulatesNICTopologyPCIAddresses verifies that the PCI
// addresses collected with the topo matrix are published next to
// nic_topology, restricted to devices that have a topology row.
func TestCollectPopulatesNICTopologyPCIAddresses(t *testing.T) {
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
		NICPCIAddresses: map[string]string{
			"mlx5_0": "0000:0c:00.0",
			"mlx5_1": "0000:18:00.0",
			// No topology row for mlx5_9: it must not be published.
			"mlx5_9": "0000:86:00.0",
		},
	}

	collector := NewCollector(fake, &fakeNICTopoCollector{matrix: matrix})

	metadata, err := collector.Collect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"mlx5_0": "0000:0c:00.0",
		"mlx5_1": "0000:18:00.0",
	}, metadata.NICTopologyPCIAddresses)
}

// TestCollectWithoutPCIAddressesLeavesFieldEmpty verifies that a topo
// matrix collected without PCI addresses (for example when sysfs could
// not be read) leaves nic_topology_pci_addresses absent, so consumers
// keep the name-keyed behaviour.
func TestCollectWithoutPCIAddressesLeavesFieldEmpty(t *testing.T) {
	fake := &fakeNVMLClient{
		deviceCount:   2,
		driverVersion: "560.35.03",
	}

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
	assert.Nil(t, metadata.NICTopologyPCIAddresses)
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

// topoOutputForRetryLoop is a minimal nvidia-smi topo -m capture: one
// GPU and two NICs, mlx5_0 and mlx5_1.
const topoOutputForRetryLoop = "GPU0    mlx5_0  mlx5_1  CPU Affinity\n" +
	"GPU0     X      NODE    NODE     0-55\n"

// topoFunc is a test double for the topo command: it returns the same
// output (or error) on every call and counts them.
type topoFunc struct {
	output string
	err    error
	calls  int
}

func (f *topoFunc) run(context.Context) (string, error) {
	f.calls++
	return f.output, f.err
}

// snapshotFunc is a test double for the PCI address snapshots: it
// returns the next map in sequence on each call (before, after and
// settle reads, in call order) and counts them. A nil entry stands
// for an unreadable snapshot.
type snapshotFunc struct {
	maps  []map[string]string
	calls int
}

func (f *snapshotFunc) take(string) (map[string]string, error) {
	m := f.maps[f.calls]
	f.calls++
	return m, nil
}

// TestDefaultNICTopoCollectorCollect_RetryLoop drives the retry loop
// with faked topo output, snapshots and sleeps: the settle wait
// between attempts, the attempt cap and dropping the addresses when
// the names never settle.
func TestDefaultNICTopoCollectorCollect_RetryLoop(t *testing.T) {
	addrA := map[string]string{"mlx5_0": "0000:41:00.0", "mlx5_1": "0000:9a:00.0"}
	addrB := map[string]string{"mlx5_0": "0000:9a:00.0", "mlx5_1": "0000:41:00.0"}
	addrC := map[string]string{"mlx5_0": "0000:55:00.0", "mlx5_1": "0000:56:00.0"}

	tests := []struct {
		name          string
		topoErr       error
		snapshots     []map[string]string
		sleepOK       bool
		wantTopoCalls int
		wantSleeps    int
		wantSnapCalls int
		wantAddresses map[string]string
		wantErr       bool
	}{
		{
			name:          "stable first attempt publishes addresses",
			snapshots:     []map[string]string{addrA, addrA},
			sleepOK:       true,
			wantTopoCalls: 1,
			wantSleeps:    0,
			wantSnapCalls: 2,
			wantAddresses: addrA,
		},
		{
			name: "rename during a run waits for the names to settle and retries",
			// Run 1 sees the names swap mid-run. The first settle
			// read already matches the run's after snapshot, so
			// the names have settled, and run 2 is accepted on
			// its own two snapshots.
			snapshots:     []map[string]string{addrA, addrB, addrB, addrB, addrB},
			sleepOK:       true,
			wantTopoCalls: 2,
			wantSleeps:    1,
			wantSnapCalls: 5,
			wantAddresses: addrB,
		},
		{
			name: "names still moving during the settle wait delay the retry",
			// The first settle read still differs from the run's
			// after snapshot; only the second read matches the
			// one before it. The retry is then accepted on its
			// own two snapshots — no comparison against the
			// earlier run decides it.
			snapshots:     []map[string]string{addrA, addrB, addrC, addrC, addrC, addrC},
			sleepOK:       true,
			wantTopoCalls: 2,
			wantSleeps:    2,
			wantSnapCalls: 6,
			wantAddresses: addrC,
		},
		{
			name: "names that never settle drop the addresses",
			// Every settle read differs from the one before it,
			// so the bounded wait ends without two reads in a
			// row agreeing and no retry is attempted.
			snapshots:     []map[string]string{addrA, addrB, addrC, addrA, addrC, addrA, addrC},
			sleepOK:       true,
			wantTopoCalls: 1,
			wantSleeps:    5,
			wantSnapCalls: 7,
			wantAddresses: nil,
		},
		{
			name: "unstable runs up to the cap drop the addresses",
			// Each run settles during the wait but sees the
			// names move again while nvidia-smi runs, so the
			// topo attempt cap ends the collection without
			// addresses.
			snapshots:     []map[string]string{addrA, addrB, addrB, addrB, addrA, addrA, addrA, addrB},
			sleepOK:       true,
			wantTopoCalls: 3,
			wantSleeps:    2,
			wantSnapCalls: 8,
			wantAddresses: nil,
		},
		{
			name:          "topo error aborts the collection",
			topoErr:       fmt.Errorf("nvidia-smi not available"),
			snapshots:     []map[string]string{addrA, addrA},
			sleepOK:       true,
			wantTopoCalls: 1,
			wantSleeps:    0,
			wantErr:       true,
		},
		{
			name:          "unreadable snapshots publish without addresses",
			snapshots:     []map[string]string{nil, nil},
			sleepOK:       true,
			wantTopoCalls: 1,
			wantSleeps:    0,
			wantSnapCalls: 2,
			wantAddresses: nil,
		},
		{
			name: "unreadable settle read publishes without addresses",
			// Stability cannot be shown without a readable
			// snapshot, so the wait ends and the matrix goes
			// out without addresses.
			snapshots:     []map[string]string{addrA, addrB, nil},
			sleepOK:       true,
			wantTopoCalls: 1,
			wantSleeps:    1,
			wantSnapCalls: 3,
			wantAddresses: nil,
		},
		{
			name:          "cancelled sleep aborts the collection",
			snapshots:     []map[string]string{addrA, addrB},
			sleepOK:       false,
			wantTopoCalls: 1,
			wantSleeps:    1,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			topo := &topoFunc{output: topoOutputForRetryLoop, err: tc.topoErr}
			snap := &snapshotFunc{maps: tc.snapshots}

			var slept []time.Duration

			c := defaultNICTopoCollector{
				ibClassPath: "unused",
				runTopo:     topo.run,
				snapshot:    snap.take,
				sleep: func(_ context.Context, d time.Duration) bool {
					slept = append(slept, d)
					return tc.sleepOK
				},
			}

			ctx := context.Background()
			if !tc.sleepOK {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			matrix, err := c.Collect(ctx)
			assert.Equal(t, tc.wantTopoCalls, topo.calls, "topo runs")
			assert.Equal(t, tc.wantSleeps, len(slept), "settle waits")

			for _, d := range slept {
				assert.Equal(t, snapshotSettleInterval, d, "settle wait is one second")
			}

			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, matrix)
			assert.Equal(t, []string{"mlx5_0", "mlx5_1"}, matrix.NICs)
			assert.Equal(t, tc.wantAddresses, matrix.NICPCIAddresses)
			assert.Equal(t, tc.wantSnapCalls, snap.calls, "snapshot reads")
		})
	}
}
