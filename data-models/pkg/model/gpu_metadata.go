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

package model

type GPUMetadata struct {
	Version       string    `json:"version"`
	Timestamp     string    `json:"timestamp"`
	NodeName      string    `json:"node_name"`
	DriverVersion string    `json:"driver_version"`
	ChassisSerial *string   `json:"chassis_serial"`
	GPUs          []GPUInfo `json:"gpus"`
	NVSwitches    []string  `json:"nvswitches"`

	// NICTopology publishes the raw GPU↔NIC relationship matrix from
	// `nvidia-smi topo -m` for downstream consumers. Keys are InfiniBand
	// device names (e.g., "mlx5_0"); values are a list of topology-level
	// strings aligned to the GPUs slice, so NICTopology["mlx5_0"][i] is the
	// level between GPUs[i] and "mlx5_0". Valid level strings are "X",
	// "PIX", "PXB", "PHB", "NODE", "SYS", or "NV<n>" for NVLink bonds.
	NICTopology map[string][]string `json:"nic_topology,omitempty"`
}

type GPUInfo struct {
	GPUID        int      `json:"gpu_id"`
	UUID         string   `json:"uuid"`
	PCIAddress   string   `json:"pci_address"`
	SerialNumber string   `json:"serial_number"`
	DeviceName   string   `json:"device_name"`
	NVLinks      []NVLink `json:"nvlinks"`

	// NUMANode is the NUMA Affinity of the GPU, parsed from the
	// `nvidia-smi topo -m` output. A value of -1 means the information
	// was not available (e.g., older driver or single-socket system).
	// Consumed by the NIC Health Monitor for management-NIC exclusion.
	NUMANode int `json:"numa_node"`
}

type NVLink struct {
	LinkID           int    `json:"link_id"`
	RemotePCIAddress string `json:"remote_pci_address"`
	RemoteLinkID     int    `json:"remote_link_id"`
}
