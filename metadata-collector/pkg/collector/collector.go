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
type defaultNICTopoCollector struct{}

func (defaultNICTopoCollector) Collect(ctx context.Context) (*nic.TopoMatrix, error) {
	output, err := nic.RunTopoCommand(ctx)
	if err != nil {
		return nil, fmt.Errorf("run nvidia-smi topo -m: %w", err)
	}

	matrix, err := nic.ParseTopoMatrix(output)
	if err != nil {
		return nil, fmt.Errorf("parse nvidia-smi topo -m output: %w", err)
	}

	return matrix, nil
}

// NewDefaultTopoCollector returns the production NICTopoCollector.
func NewDefaultTopoCollector() NICTopoCollector {
	return defaultNICTopoCollector{}
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

	slog.Info("Populated NIC topology matrix",
		"gpus", len(matrix.GPUs),
		"nics", len(matrix.NICs),
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
