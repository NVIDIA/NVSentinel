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
	"fmt"
	"log/slog"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

func (w *NVMLWrapper) CollectNVLinkTopology(
	gpuInfo *model.GPUInfo,
	index int,
	deviceMap map[string]nvml.Device,
	parsedTopology map[int]GPUNVLinkTopology,
) (map[string]struct{}, error) {
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device handle for GPU %d: %v", index, nvml.ErrorString(ret))
	}

	slog.Debug("Collecting NVLink topology", "gpu_id", index, "pci_address", gpuInfo.PCIAddress)

	nvswitches := make(map[string]struct{})

	for linkID := range nvml.NVLINK_MAX_LINKS {
		state, ret := device.GetNvLinkState(linkID)
		if ret != nvml.SUCCESS {
			slog.Debug("Failed to get NVLink state", "gpu_id", index, "link_id", linkID, "error", nvml.ErrorString(ret))
			continue
		}

		if state != nvml.FEATURE_ENABLED {
			slog.Debug("NVLink not enabled", "gpu_id", index, "link_id", linkID)
			continue
		}

		remotePCIInfo, ret := device.GetNvLinkRemotePciInfo(linkID)
		if ret != nvml.SUCCESS {
			slog.Debug("Failed to get remote PCI info", "gpu_id", index, "link_id", linkID, "error", nvml.ErrorString(ret))
			continue
		}

		remoteDeviceType, ret := device.GetNvLinkRemoteDeviceType(linkID)
		if ret != nvml.SUCCESS {
			slog.Debug("Failed to get remote device type", "gpu_id", index, "link_id", linkID, "error", nvml.ErrorString(ret))
			continue
		}

		if remoteDeviceType != nvml.NVLINK_DEVICE_TYPE_SWITCH {
			remotePCIStr := convertNVMLCString(remotePCIInfo.BusIdLegacy)
			slog.Debug("Remote device is not an NVSwitch, skipping",
				"gpu_id", index,
				"link_id", linkID,
				"remote_pci", normalizePCIAddress(remotePCIStr),
				"device_type", remoteDeviceType)

			continue
		}

		remotePCIStr := convertNVMLCString(remotePCIInfo.BusIdLegacy)
		remotePCI := normalizePCIAddress(remotePCIStr)

		slog.Debug("Found NVSwitch connection",
			"gpu_id", index,
			"link_id", linkID,
			"nvswitch_pci", remotePCI)

		remoteLinkID := getRemoteLinkID(index, linkID, remotePCI, parsedTopology, deviceMap, gpuInfo.PCIAddress)

		gpuInfo.NVLinks = append(gpuInfo.NVLinks, model.NVLink{
			LinkID:           linkID,
			RemotePCIAddress: remotePCI,
			RemoteLinkID:     remoteLinkID,
		})

		slog.Info("Recorded NVLink connection",
			"gpu_id", index,
			"link_id", linkID,
			"remote_pci", remotePCI,
			"remote_link_id", remoteLinkID)

		nvswitches[remotePCI] = struct{}{}
	}

	slog.Info("NVLink topology collection complete",
		"gpu_id", index,
		"pci_address", gpuInfo.PCIAddress,
		"nvlink_count", len(gpuInfo.NVLinks),
		"nvswitch_count", len(nvswitches))

	return nvswitches, nil
}

func getRemoteLinkID(
	gpuID, localLinkID int,
	remotePCI string,
	parsedTopology map[int]GPUNVLinkTopology,
	deviceMap map[string]nvml.Device,
	localPCI string,
) int {
	if linkID := getRemoteLinkFromParsedTopology(gpuID, localLinkID, remotePCI, parsedTopology); linkID != -1 {
		return linkID
	}

	if linkID := getRemoteLinkViaReverseLookup(gpuID, localLinkID, remotePCI, localPCI, deviceMap); linkID != -1 {
		return linkID
	}

	slog.Debug("Could not determine remote link ID",
		"gpu_id", gpuID,
		"local_link", localLinkID,
		"remote_pci", remotePCI)

	return -1
}

func getRemoteLinkFromParsedTopology(
	gpuID, localLinkID int,
	remotePCI string,
	parsedTopology map[int]GPUNVLinkTopology,
) int {
	gpuTopology, exists := parsedTopology[gpuID]
	if !exists {
		return -1
	}

	for _, conn := range gpuTopology.Connections {
		if conn.LinkID == localLinkID && normalizePCIAddress(conn.RemotePCIAddress) == remotePCI {
			slog.Debug("Found remote link ID from parsed topology",
				"gpu_id", gpuID,
				"local_link", localLinkID,
				"remote_pci", remotePCI,
				"remote_link", conn.RemoteLinkID)

			return conn.RemoteLinkID
		}
	}

	return -1
}

func getRemoteLinkViaReverseLookup(
	gpuID, localLinkID int,
	remotePCI, localPCI string,
	deviceMap map[string]nvml.Device,
) int {
	remoteDevice, exists := deviceMap[remotePCI]
	if !exists {
		slog.Debug("Remote device not in device map, likely an NVSwitch",
			"gpu_id", gpuID,
			"local_link", localLinkID,
			"remote_pci", remotePCI)

		return -1
	}

	slog.Debug("Attempting reverse lookup via device map",
		"gpu_id", gpuID,
		"local_link", localLinkID,
		"remote_pci", remotePCI)

	for i := range nvml.NVLINK_MAX_LINKS {
		remoteState, ret := remoteDevice.GetNvLinkState(i)
		if ret != nvml.SUCCESS {
			slog.Debug("Failed to get NVLink state during reverse lookup",
				"remote_pci", remotePCI,
				"link_id", i,
				"error", nvml.ErrorString(ret))

			continue
		}

		if remoteState != nvml.FEATURE_ENABLED {
			continue
		}

		remoteRemotePCIInfo, ret := remoteDevice.GetNvLinkRemotePciInfo(i)
		if ret != nvml.SUCCESS {
			slog.Debug("Failed to get remote PCI info during reverse lookup",
				"remote_pci", remotePCI,
				"link_id", i,
				"error", nvml.ErrorString(ret))

			continue
		}

		remoteRemotePCI := normalizePCIAddress(convertNVMLCString(remoteRemotePCIInfo.BusIdLegacy))
		if remoteRemotePCI == localPCI {
			slog.Debug("Found remote link ID via reverse lookup",
				"gpu_id", gpuID,
				"local_link", localLinkID,
				"remote_pci", remotePCI,
				"remote_link", i)

			return i
		}
	}

	return -1
}
