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

package metadata

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

type Reader struct {
	path string

	mu     sync.Mutex
	loaded bool

	metadata *model.GPUMetadata

	pciToGPU      map[string]*model.GPUInfo
	uuidToInfo    map[string]*model.GPUInfo
	nvswitchLinks map[string]map[int]*gpuLinkInfo
}

type gpuLinkInfo struct {
	GPU         *model.GPUInfo
	LocalLinkID int
}

func NewReader(path string) *Reader {
	return &Reader{
		path: path,
	}
}

func (r *Reader) ensureLoaded() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.loaded {
		return nil
	}

	if err := r.load(); err != nil {
		return err
	}

	r.loaded = true

	return nil
}

func (r *Reader) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("failed to read metadata file: %w", err)
	}

	var metadata model.GPUMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to parse metadata JSON: %w", err)
	}

	r.metadata = &metadata
	r.buildMaps()

	if r.loaded {
		slog.Debug("GPU metadata reloaded",
			"gpus", len(metadata.GPUs),
			"driver_version", metadata.DriverVersion)
	} else {
		slog.Info("GPU metadata loaded",
			"gpus", len(metadata.GPUs),
			"nvswitches", len(metadata.NVSwitches),
			"chassis_serial", metadata.ChassisSerial != nil,
			"driver_version", metadata.DriverVersion)
	}

	return nil
}

func (r *Reader) buildMaps() {
	r.pciToGPU = make(map[string]*model.GPUInfo)
	r.uuidToInfo = make(map[string]*model.GPUInfo)
	r.nvswitchLinks = make(map[string]map[int]*gpuLinkInfo)

	for i := range r.metadata.GPUs {
		gpu := &r.metadata.GPUs[i]
		normPCI := normalizePCI(gpu.PCIAddress)
		r.pciToGPU[normPCI] = gpu
		r.uuidToInfo[gpu.UUID] = gpu

		for _, link := range gpu.NVLinks {
			remotePCI := normalizePCI(link.RemotePCIAddress)

			if r.nvswitchLinks[remotePCI] == nil {
				r.nvswitchLinks[remotePCI] = make(map[int]*gpuLinkInfo)
			}

			r.nvswitchLinks[remotePCI][link.RemoteLinkID] = &gpuLinkInfo{
				GPU:         gpu,
				LocalLinkID: link.LinkID,
			}
		}
	}
}

func (r *Reader) GetInfoByUUID(uuid string) (*model.GPUInfo, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, fmt.Errorf("failed to load metadata for UUID lookup %s: %w", uuid, err)
	}

	gpu, ok := r.uuidToInfo[uuid]
	if !ok {
		return nil, fmt.Errorf("GPU not found for UUID: %s", uuid)
	}

	return gpu, nil
}

func (r *Reader) GetGPUByPCI(pci string) (*model.GPUInfo, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, fmt.Errorf("failed to load metadata for PCI lookup %s: %w", pci, err)
	}

	normPCI := normalizePCI(pci)
	gpu, ok := r.pciToGPU[normPCI]

	if !ok {
		return nil, fmt.Errorf("GPU not found for PCI address: %s", pci)
	}

	return gpu, nil
}

func (r *Reader) GetGPUByNVSwitchLink(nvswitchPCI string, linkID int) (*model.GPUInfo, int, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, -1, fmt.Errorf("failed to load metadata for NVSwitch lookup %s link %d: %w", nvswitchPCI, linkID, err)
	}

	normPCI := normalizePCI(nvswitchPCI)
	links, ok := r.nvswitchLinks[normPCI]

	if !ok {
		return nil, -1, fmt.Errorf("NVSwitch not found: %s", nvswitchPCI)
	}

	info, ok := links[linkID]

	if !ok {
		return nil, -1, fmt.Errorf("link %d not found on NVSwitch %s", linkID, nvswitchPCI)
	}

	return info.GPU, info.LocalLinkID, nil
}

func (r *Reader) GetChassisSerial() *string {
	if err := r.ensureLoaded(); err != nil {
		return nil
	}

	return r.metadata.ChassisSerial
}

// GetDriverVersion returns the GPU driver version from the metadata file.
//
// Unlike the other accessors, the driver version is typically consumed early
// (e.g. when constructing the XID parser) and may still be empty if the
// metadata-collector has not finished writing the file yet. Permanently caching
// that empty value is dangerous: the XID sidecar would keep receiving an empty
// driver_version and silently fall back to the wrong (V1) NVL5 decode table for
// R575+ drivers, mis-classifying errors. To avoid that, re-attempt a load
// whenever the cached driver version is still empty. Once a non-empty value is
// observed it is cached like every other field.
func (r *Reader) GetDriverVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.loaded || r.metadata == nil || r.metadata.DriverVersion == "" {
		if err := r.load(); err != nil {
			slog.Warn("Failed to load GPU metadata for driver version",
				"path", r.path, "error", err)

			return ""
		}

		r.loaded = true
	}

	return r.metadata.DriverVersion
}

func normalizePCI(pci string) string {
	parts := strings.Split(pci, ":")
	if len(parts) != 3 {
		return strings.ToLower(pci)
	}

	domain := parts[0]
	if len(domain) > 4 {
		domain = domain[len(domain)-4:]
	}

	busDeviceFunc := parts[2]
	if idx := strings.Index(busDeviceFunc, "."); idx != -1 {
		busDeviceFunc = busDeviceFunc[:idx]
	}

	return fmt.Sprintf("%s:%s:%s",
		strings.ToLower(domain),
		strings.ToLower(parts[1]),
		strings.ToLower(busDeviceFunc))
}
