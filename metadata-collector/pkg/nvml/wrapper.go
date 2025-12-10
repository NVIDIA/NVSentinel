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

package nvml

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

type NVMLWrapper struct{}

func (w *NVMLWrapper) Init() error {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", nvml.ErrorString(ret))
	}

	return nil
}

func (w *NVMLWrapper) Shutdown() error {
	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to shutdown NVML: %v", nvml.ErrorString(ret))
	}

	return nil
}

func (w *NVMLWrapper) GetDeviceCount() (int, error) {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	return count, nil
}

func (w *NVMLWrapper) GetGPUInfo(index int) (*model.GPUInfo, error) {
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device handle for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	gpuInfo := &model.GPUInfo{
		GPUID:   index,
		NVLinks: make([]model.NVLink, 0),
	}

	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get UUID for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	gpuInfo.UUID = uuid

	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get PCI info for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	gpuInfo.PCIAddress = normalizePCIAddress(convertNVMLCString(pciInfo.BusIdLegacy))

	serial, ret := device.GetSerial()
	if ret != nvml.SUCCESS {
		gpuInfo.SerialNumber = ""
	} else {
		gpuInfo.SerialNumber = serial
	}

	name, ret := device.GetName()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get name for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	gpuInfo.DeviceName = name

	return gpuInfo, nil
}

func (w *NVMLWrapper) GetChassisSerial(index int) *string {
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		slog.Debug("Failed to get device handle for chassis serial", "index", index, "error", nvml.ErrorString(ret))
		return nil
	}

	platformInfo, ret := device.GetPlatformInfo()
	if ret != nvml.SUCCESS {
		slog.Debug("Failed to get platform info for chassis serial", "index", index, "error", nvml.ErrorString(ret))
		return nil
	}

	chassisSerial := convertNVMLCString(platformInfo.ChassisSerialNumber)
	if chassisSerial == "" || chassisSerial == "N/A" {
		slog.Debug("Chassis serial not available", "index", index)
		return nil
	}

	return &chassisSerial
}

func (w *NVMLWrapper) BuildDeviceMap() (map[string]nvml.Device, error) {
	count, err := w.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get device count: %w", err)
	}

	deviceMap := make(map[string]nvml.Device)

	for i := range count {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			slog.Warn("Failed to get device handle", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		pciInfo, ret := device.GetPciInfo()
		if ret != nvml.SUCCESS {
			slog.Warn("Failed to get PCI info for device", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		pciStr := convertNVMLCString(pciInfo.BusIdLegacy)
		normalizedPCI := normalizePCIAddress(pciStr)
		deviceMap[normalizedPCI] = device

		slog.Debug("Added device to map", "index", i, "pci", normalizedPCI)
	}

	slog.Info("Built device map", "device_count", len(deviceMap))

	return deviceMap, nil
}

func (w *NVMLWrapper) ParseNVLinkTopologyWithContext(ctx context.Context) (map[int]GPUNVLinkTopology, error) {
	return DetectNVLinkTopology(ctx)
}

func convertNVMLCString(busID [16]uint8) string {
	b := make([]byte, 0, 16)

	for _, c := range busID {
		if c == 0 {
			break
		}

		b = append(b, c)
	}

	return string(b)
}

func normalizePCIAddress(pci string) string {
	parts := strings.Split(pci, ":")
	if len(parts) != 3 {
		return strings.ToLower(pci)
	}

	domain := parts[0]
	if len(domain) > 4 {
		domain = domain[len(domain)-4:]
	}

	return fmt.Sprintf("%s:%s:%s",
		strings.ToLower(domain),
		strings.ToLower(parts[1]),
		strings.ToLower(parts[2]))
}
