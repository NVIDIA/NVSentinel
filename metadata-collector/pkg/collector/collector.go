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
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"

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
	nvml        nvmlClient
	nicTopo     NICTopoCollector
	skippedGPUs prometheus.Counter
}

// WithSkippedGPUCounter records GPUs omitted because their identity cannot be queried.
func WithSkippedGPUCounter(counter prometheus.Counter) func(*Collector) {
	return func(c *Collector) { c.skippedGPUs = counter }
}

func NewCollector(nvmlWrapper nvmlClient, topo NICTopoCollector, options ...func(*Collector)) *Collector {
	c := &Collector{nvml: nvmlWrapper, nicTopo: topo}
	for _, option := range options {
		option(c)
	}

	return c
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

	c.populateNICTopology(ctx, metadata, count)

	return metadata, nil
}

// populateNICTopology attaches the raw nvidia-smi topo -m matrix to
// metadata.NICTopology. Failures degrade to an empty map rather than
// aborting the collector — other consumers of gpu_metadata.json still
// receive the NVML-derived fields.
func (c *Collector) populateNICTopology(ctx context.Context, metadata *model.GPUMetadata, nvmlCount int) {
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

	columns, err := alignTopologyToGPUs(matrix, metadata, nvmlCount)
	if err != nil {
		slog.Warn("Dropping nic_topology and gpu numa_node to avoid misaligned data",
			"error", err,
			"nvml_gpu_count", len(metadata.GPUs),
			"topo_gpu_count", len(matrix.GPUs),
			"topo_gpus", matrix.GPUs,
		)

		return
	}

	populateGPUNUMANodes(metadata, matrix, columns)

	if len(matrix.NICs) == 0 {
		slog.Info("No InfiniBand NICs reported by nvidia-smi topo -m, nic_topology will be empty")
		return
	}

	// The published contract aligns each relationship list with metadata.GPUs, so
	// NICTopology[nic][i] describes GPUs[i]. Project by label: a skipped GPU's
	// column is left out and every other entry keeps its own GPU.
	metadata.NICTopology = make(map[string][]string, len(matrix.NICs))
	for _, name := range matrix.NICs {
		metadata.NICTopology[name] = projectRelationships(matrix.Relationships[name], metadata, columns)
	}

	slog.Info("Populated NIC topology matrix",
		"gpus", len(matrix.GPUs),
		"nics", len(matrix.NICs),
	)
}

// projectRelationships returns one NIC's relationship levels in metadata.GPUs
// order, taking each GPU's entry from its labeled matrix column.
func projectRelationships(levels []string, metadata *model.GPUMetadata, columns map[int]int) []string {
	projected := make([]string, len(metadata.GPUs))

	for i, gpu := range metadata.GPUs {
		if col, ok := columns[gpu.GPUID]; ok && col < len(levels) {
			projected[i] = levels[col]
		}
	}

	return projected
}

// populateGPUNUMANodes copies each GPU's NUMA Affinity value from the
// topo matrix column whose label matches its NVML index.
func populateGPUNUMANodes(metadata *model.GPUMetadata, matrix *nic.TopoMatrix, columns map[int]int) {
	for i := range metadata.GPUs {
		col, ok := columns[metadata.GPUs[i].GPUID]
		if ok && col < len(matrix.GPUNUMANodes) {
			metadata.GPUs[i].NUMANode = matrix.GPUNUMANodes[col]
		}
	}
}

// topoColumnsByGPUID maps the GPU index in each topo matrix column label
// (GPU0, GPU1, ...) to that column's position.
func topoColumnsByGPUID(matrix *nic.TopoMatrix) (map[int]int, error) {
	columns := make(map[int]int, len(matrix.GPUs))

	for col, label := range matrix.GPUs {
		id, err := strconv.Atoi(strings.TrimPrefix(label, "GPU"))
		if err != nil || !strings.HasPrefix(label, "GPU") {
			return nil, fmt.Errorf("topo matrix GPU column %q is not of the form GPU<n>", label)
		}

		if _, dup := columns[id]; dup {
			return nil, fmt.Errorf("topo matrix lists %s more than once", label)
		}

		columns[id] = col
	}

	return columns, nil
}

// alignTopologyToGPUs maps each GPU index to its topo matrix column, using
// the column labels (GPU0, GPU1, ...) rather than positions. Every collected
// GPU must have a column. An extra column is accepted only for a GPU that NVML
// reports but whose identity could not be read, so the NIC relationships stay
// valid when one GPU has fallen off the bus. Anything else means the matrix
// and NVML disagree, and the caller drops the topology.
func alignTopologyToGPUs(matrix *nic.TopoMatrix, metadata *model.GPUMetadata, nvmlCount int) (map[int]int, error) {
	columns, err := topoColumnsByGPUID(matrix)
	if err != nil {
		return nil, err
	}

	collected := make(map[int]struct{}, len(metadata.GPUs))
	for _, gpu := range metadata.GPUs {
		if _, ok := columns[gpu.GPUID]; !ok {
			return nil, fmt.Errorf("topo matrix has no column for GPU%d", gpu.GPUID)
		}

		collected[gpu.GPUID] = struct{}{}
	}

	for id := range columns {
		if _, ok := collected[id]; !ok && (id < 0 || id >= nvmlCount) {
			return nil, fmt.Errorf("topo matrix column GPU%d is not a GPU NVML reports", id)
		}
	}

	return columns, nil
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
	nvswitchSet := make(map[string]struct{})

	var chassisSerial *string

	collectChassisSerial := supportsChassisSerial(metadata.DriverVersion)

	if !collectChassisSerial {
		slog.Info("Skipping chassis serial collection because driver does not support platform info",
			"driver_version", metadata.DriverVersion)
	}

	for i := range count {
		nvmlGPUInfo, err := c.nvml.GetGPUInfo(i)
		if err != nil {
			c.recordSkippedGPU(i, err)

			continue
		}

		if len(metadata.GPUs) == 0 && collectChassisSerial {
			chassisSerial = c.nvml.GetChassisSerial(i)
		}

		nvswitches, err := c.nvml.CollectNVLinkTopology(nvmlGPUInfo, i, deviceMap, parsedTopology)
		if err != nil {
			slog.Warn("Failed to collect NVLink topology for GPU", "gpu_id", i, "error", err)
		} else {
			maps.Copy(nvswitchSet, nvswitches)
		}

		metadata.GPUs = append(metadata.GPUs, *nvmlGPUInfo)
	}

	if count > 0 && len(metadata.GPUs) == 0 {
		return fmt.Errorf("could not query identity for any of %d GPUs", count)
	}

	metadata.ChassisSerial = chassisSerial

	metadata.NVSwitches = make([]string, 0, len(nvswitchSet))
	for pci := range nvswitchSet {
		metadata.NVSwitches = append(metadata.NVSwitches, pci)
	}

	return nil
}

func (c *Collector) recordSkippedGPU(index int, err error) {
	slog.Warn("Skipping GPU because identity collection failed", "gpu_id", index, "error", err)

	if c.skippedGPUs != nil {
		c.skippedGPUs.Inc()
	}
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
