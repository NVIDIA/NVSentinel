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
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nic"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
)

// NICTopoCollector produces the raw nvidia-smi topo -m matrix
type NICTopoCollector interface {
	Collect(ctx context.Context) (*nic.TopoMatrix, error)
}

// defaultNICTopoCollector runs nvidia-smi and parses the output.
//
// runTopo, snapshot and sleep are fields rather than direct calls so
// tests can drive the retry loop without a real nvidia-smi or sysfs:
// runTopo fakes the topo command, snapshot fakes the PCI address
// snapshots, and sleep fakes the settle waits.
type defaultNICTopoCollector struct {
	ibClassPath string
	runTopo     func(ctx context.Context) (string, error)
	snapshot    func(path string) (map[string]string, error)
	sleep       func(ctx context.Context, d time.Duration) bool
}

// maxTopoCollectAttempts bounds the topo runs when an RDMA rename lands
// while nvidia-smi runs. A rename moves every device name to another
// device in well under a second, so one retry lands after it in
// practice; the bound only guards against a name set that never
// settles, where the matrix is published without PCI addresses —
// pairing its rows with addresses from a snapshot taken while names
// were still moving can bind a row to the wrong device for the life
// of the file.
const maxTopoCollectAttempts = 3

// snapshotSettleInterval is the wait between the snapshot reads
// that follow a run whose names moved. After such a run the
// collector re-reads the snapshot every second until two reads in
// a row agree, so the retry starts from a name set that has stopped
// moving; the retry is then accepted on its own two snapshots
// alone, with no comparison against the earlier run.
const snapshotSettleInterval = time.Second

// maxSettleSnapshotReads bounds the settle wait after a run whose
// names moved. A rename moves every device name in well under a
// second, so two agreeing reads normally come back at once; five
// reads cover a slower renamer without letting a name set that
// keeps flapping hold the collection open. Past the bound the
// matrix is published without PCI addresses.
const maxSettleSnapshotReads = 5

// sleepContext waits for d unless ctx ends first, reporting whether
// the full wait elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c defaultNICTopoCollector) Collect(ctx context.Context) (*nic.TopoMatrix, error) {
	for attempt := 1; ; attempt++ {
		matrix, before, after, err := c.collectOnce(ctx)
		if err != nil {
			return nil, err
		}

		// Without two readable snapshots there is nothing to check the
		// run against; publish the matrix without PCI addresses, which
		// is the behaviour of collectors before this field existed.
		if before == nil || after == nil {
			return matrix, nil
		}

		// A run whose own two snapshots agree is safe to publish:
		// the names were still while nvidia-smi ran, and consumers
		// match the rows by address.
		if !nic.AddressesChangedForNames(before, after, matrix.NICs) {
			matrix.NICPCIAddresses = nic.AddressesForNames(after, matrix.NICs)

			return matrix, nil
		}

		if attempt >= maxTopoCollectAttempts {
			slog.Warn("RDMA device names kept changing while nvidia-smi topo -m ran, "+
				"publishing the topology without PCI addresses",
				"attempts", attempt)

			return matrix, nil
		}

		slog.Warn("RDMA device names changed while nvidia-smi topo -m ran, "+
			"waiting for the names to settle before collecting the NIC topology again",
			"attempt", attempt)

		settled, err := c.waitForNamesToSettle(ctx, after, matrix.NICs)
		if err != nil {
			return nil, err
		}

		if !settled {
			slog.Warn("RDMA device names did not settle, " +
				"publishing the topology without PCI addresses")

			return matrix, nil
		}
	}
}

// waitForNamesToSettle re-reads the PCI address snapshot every
// snapshotSettleInterval until two reads in a row agree on the
// watched names, starting from the last snapshot of the run that saw
// the names move. It reports whether the names settled. The wait is
// bounded by maxSettleSnapshotReads reads; a snapshot that cannot be
// read ends the wait unsettled, because stability cannot be shown
// without one.
func (c defaultNICTopoCollector) waitForNamesToSettle(
	ctx context.Context, last map[string]string, names []string,
) (bool, error) {
	for range maxSettleSnapshotReads {
		if !c.sleep(ctx, snapshotSettleInterval) {
			return false, ctx.Err()
		}

		current, err := c.snapshot(c.ibClassPath)
		if err != nil {
			slog.Warn("Could not snapshot IB device PCI addresses while waiting for the names to settle, "+
				"nic_topology_pci_addresses will be empty", "error", err)

			return false, nil
		}

		if current == nil {
			return false, nil
		}

		if !nic.AddressesChangedForNames(last, current, names) {
			return true, nil
		}

		last = current
	}

	return false, nil
}

// collectOnce runs nvidia-smi topo -m once, bracketed by PCI address
// snapshots of the IB devices. A snapshot that cannot be read comes
// back nil instead of failing the collection.
func (c defaultNICTopoCollector) collectOnce(
	ctx context.Context,
) (matrix *nic.TopoMatrix, before, after map[string]string, err error) {
	before, snapErr := c.snapshot(c.ibClassPath)
	if snapErr != nil {
		slog.Warn("Could not snapshot IB device PCI addresses before nvidia-smi topo -m, "+
			"nic_topology_pci_addresses will be empty", "error", snapErr)

		before = nil
	}

	output, err := c.runTopo(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("run nvidia-smi topo -m: %w", err)
	}

	matrix, err = nic.ParseTopoMatrix(output)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse nvidia-smi topo -m output: %w", err)
	}

	after, snapErr = c.snapshot(c.ibClassPath)
	if snapErr != nil {
		slog.Warn("Could not snapshot IB device PCI addresses after nvidia-smi topo -m, "+
			"nic_topology_pci_addresses will be empty", "error", snapErr)

		after = nil
	}

	return matrix, before, after, nil
}

// NewDefaultTopoCollector returns the production NICTopoCollector.
func NewDefaultTopoCollector() NICTopoCollector {
	return defaultNICTopoCollector{
		ibClassPath: nic.DefaultIBClassPath,
		runTopo:     nic.RunTopoCommand,
		snapshot:    nic.SnapshotPCIAddresses,
		sleep:       sleepContext,
	}
}

type nvmlClient interface {
	GetDeviceCount() (int, error)
	GetDriverVersion() (string, error)
	BuildDeviceMap() (map[string]gonvml.Device, error)
	ParseNVLinkTopologyWithContext(context.Context) (map[int]nvml.GPUNVLinkTopology, error)
	GetGPUInfo(index int) (*model.GPUInfo, error)
	GetChassisSerial(index int) *string
	CollectNVLinkTopology(
		gpuInfo *model.GPUInfo,
		index int,
		deviceMap map[string]gonvml.Device,
		parsedTopology map[int]nvml.GPUNVLinkTopology,
	) (map[string]struct{}, error)
}

// Collector gathers GPU metadata from a node using NVML and nvidia-smi.
type Collector struct {
	nvml    nvmlClient
	nicTopo NICTopoCollector
}

func NewCollector(nvmlWrapper nvmlClient, topo NICTopoCollector) *Collector {
	return &Collector{nvml: nvmlWrapper, nicTopo: topo}
}

// Collect gathers GPU metadata via NVML (device info, NVLink topology,
// NVSwitch PCIs, and for driver ≥ R560 the chassis serial) and enriches
// it with the nvidia-smi topo -m matrix.
func (c *Collector) Collect(ctx context.Context) (*model.GPUMetadata, error) {
	count, err := c.nvml.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get GPU device count: %w", err)
	}

	nodeName, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	driverVersion, err := c.nvml.GetDriverVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get driver version: %w", err)
	}

	deviceMap, parsedTopology, err := c.prepareTopologyData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare topology data: %w", err)
	}

	metadata := &model.GPUMetadata{
		Version:       "1.0",
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		NodeName:      nodeName,
		DriverVersion: driverVersion,
		GPUs:          make([]model.GPUInfo, 0, count),
	}

	if err := c.collectGPUData(count, metadata, deviceMap, parsedTopology); err != nil {
		return nil, fmt.Errorf("failed to collect GPU data: %w", err)
	}

	for i := range metadata.GPUs {
		metadata.GPUs[i].NUMANode = -1
	}

	c.populateNICTopology(ctx, metadata)

	return metadata, nil
}

// populateNICTopology attaches the raw nvidia-smi topo -m matrix to
// metadata.NICTopology. Failures degrade to an empty map rather than
// aborting the collector — other consumers of gpu_metadata.json still
// receive the NVML-derived fields.
func (c *Collector) populateNICTopology(ctx context.Context, metadata *model.GPUMetadata) {
	matrix, err := c.nicTopo.Collect(ctx)
	if err != nil {
		slog.Warn("Failed to collect NIC topology matrix, nic_topology will be empty",
			"error", err)

		return
	}

	if matrix == nil {
		slog.Info("No topology matrix returned by nvidia-smi topo -m")
		return
	}

	if err := checkGPUCountMismatch(matrix, metadata); err != nil {
		slog.Warn("Dropping nic_topology and gpu numa_node to avoid misaligned data",
			"error", err,
			"nvml_gpu_count", len(metadata.GPUs),
			"topo_gpu_count", len(matrix.GPUs),
			"topo_gpus", matrix.GPUs,
		)

		return
	}

	populateGPUNUMANodes(metadata, matrix)

	if len(matrix.NICs) == 0 {
		slog.Info("No InfiniBand NICs reported by nvidia-smi topo -m, nic_topology will be empty")
		return
	}

	metadata.NICTopology = make(map[string][]string, len(matrix.NICs))
	for _, name := range matrix.NICs {
		metadata.NICTopology[name] = matrix.Relationships[name]
	}

	// Publish the PCI address of each topology row alongside the
	// name-keyed matrix, restricted to rows that were published.
	// Consumers use it to resolve rows by physical device when a
	// device name has moved since collection.
	if len(matrix.NICPCIAddresses) > 0 {
		addresses := make(map[string]string, len(matrix.NICPCIAddresses))
		for name, addr := range matrix.NICPCIAddresses {
			if _, ok := metadata.NICTopology[name]; ok {
				addresses[name] = addr
			}
		}

		if len(addresses) > 0 {
			metadata.NICTopologyPCIAddresses = addresses
		}
	}

	slog.Info("Populated NIC topology matrix",
		"gpus", len(matrix.GPUs),
		"nics", len(matrix.NICs),
		"nics_with_pci_address", len(metadata.NICTopologyPCIAddresses),
	)
}

// populateGPUNUMANodes copies the parsed NUMA Affinity values from
// the topo matrix into the corresponding GPUInfo entries. The caller
// guarantees the GPU counts match (mismatches cause an early return
// before this function is called).
func populateGPUNUMANodes(metadata *model.GPUMetadata, matrix *nic.TopoMatrix) {
	for i := range metadata.GPUs {
		if i < len(matrix.GPUNUMANodes) {
			metadata.GPUs[i].NUMANode = matrix.GPUNUMANodes[i]
		}
	}
}

// checkGPUCountMismatch returns an error when the parsed matrix
// disagrees with NVML's GPU count, or nil when they match.
func checkGPUCountMismatch(matrix *nic.TopoMatrix, metadata *model.GPUMetadata) error {
	topo, nvmlCount := len(matrix.GPUs), len(metadata.GPUs)
	if topo == nvmlCount {
		return nil
	}

	return fmt.Errorf("topo matrix parsed %d GPU columns but NVML reports %d GPUs", topo, nvmlCount)
}

// prepareTopologyData builds the device map and parses NVLink topology.
func (c *Collector) prepareTopologyData(
	ctx context.Context,
) (map[string]gonvml.Device, map[int]nvml.GPUNVLinkTopology, error) {
	slog.Info("Building device map for NVLink topology discovery")

	deviceMap, err := c.nvml.BuildDeviceMap()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build device map: %w", err)
	}

	slog.Info("Parsing NVLink topology from nvidia-smi")

	parsedTopology, err := c.nvml.ParseNVLinkTopologyWithContext(ctx)
	if err != nil {
		slog.Warn("Failed to parse nvidia-smi topology, remote link IDs will be -1", "error", err)

		parsedTopology = make(map[int]nvml.GPUNVLinkTopology)
	}

	return deviceMap, parsedTopology, nil
}

// collectGPUData iterates over GPUs to populate metadata, NVSwitch, and chassis serial fields.
func (c *Collector) collectGPUData(
	count int,
	metadata *model.GPUMetadata,
	deviceMap map[string]gonvml.Device,
	parsedTopology map[int]nvml.GPUNVLinkTopology,
) error {
	nvswitchSet := make(map[string]bool)

	var chassisSerial *string

	collectChassisSerial := supportsChassisSerial(metadata.DriverVersion)

	if !collectChassisSerial {
		slog.Info("Skipping chassis serial collection because driver does not support platform info",
			"driver_version", metadata.DriverVersion)
	}

	for i := range count {
		nvmlGPUInfo, err := c.nvml.GetGPUInfo(i)
		if err != nil {
			return fmt.Errorf("failed to get GPU info for GPU %d: %w", i, err)
		}

		if i == 0 && collectChassisSerial {
			chassisSerial = c.nvml.GetChassisSerial(i)
		}

		nvswitches, err := c.nvml.CollectNVLinkTopology(nvmlGPUInfo, i, deviceMap, parsedTopology)
		if err != nil {
			slog.Warn("Failed to collect NVLink topology for GPU", "gpu_id", i, "error", err)
		} else {
			for pci := range nvswitches {
				nvswitchSet[pci] = true
			}
		}

		metadata.GPUs = append(metadata.GPUs, *nvmlGPUInfo)
	}

	metadata.ChassisSerial = chassisSerial

	metadata.NVSwitches = make([]string, 0, len(nvswitchSet))
	for pci := range nvswitchSet {
		metadata.NVSwitches = append(metadata.NVSwitches, pci)
	}

	return nil
}

// supportsChassisSerial reports whether the driver version supports platform info queries (>= R560).
func supportsChassisSerial(driverVersion string) bool {
	const minSupportedMajorVersion = 560

	majorVersionString := strings.TrimSpace(strings.SplitN(driverVersion, ".", 2)[0])
	if majorVersionString == "" {
		slog.Warn("Failed to parse driver major version for chassis serial support",
			"driver_version", driverVersion,
			"error", "missing major version")

		return false
	}

	majorVersion, err := strconv.Atoi(majorVersionString)
	if err != nil {
		slog.Warn("Failed to parse driver major version for chassis serial support",
			"driver_version", driverVersion,
			"error", err)

		return false
	}

	return majorVersion >= minSupportedMajorVersion
}
