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
	"time"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
)

type Collector struct {
	nvml *nvml.NVMLWrapper
}

func NewCollector(nvmlWrapper *nvml.NVMLWrapper) *Collector {
	return &Collector{
		nvml: nvmlWrapper,
	}
}

func (c *Collector) Collect(ctx context.Context) (*model.GPUMetadata, error) {
	count, err := c.nvml.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get GPU device count: %w", err)
	}

	nodeName, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	deviceMap, parsedTopology, err := c.prepareTopologyData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare topology data: %w", err)
	}

	metadata := &model.GPUMetadata{
		Version:   "1.0",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		NodeName:  nodeName,
		GPUs:      make([]model.GPUInfo, 0, count),
	}

	if err := c.collectGPUData(count, metadata, deviceMap, parsedTopology); err != nil {
		return nil, fmt.Errorf("failed to collect GPU data: %w", err)
	}

	return metadata, nil
}

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

func (c *Collector) collectGPUData(
	count int,
	metadata *model.GPUMetadata,
	deviceMap map[string]gonvml.Device,
	parsedTopology map[int]nvml.GPUNVLinkTopology,
) error {
	nvswitchSet := make(map[string]bool)

	var chassisSerial *string

	for i := range count {
		nvmlGPUInfo, err := c.nvml.GetGPUInfo(i)
		if err != nil {
			return fmt.Errorf("failed to get GPU info for GPU %d: %w", i, err)
		}

		if i == 0 {
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
