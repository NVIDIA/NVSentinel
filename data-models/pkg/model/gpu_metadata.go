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
	ChassisSerial *string   `json:"chassis_serial"`
	GPUs          []GPUInfo `json:"gpus"`
	NVSwitches    []string  `json:"nvswitches"`
}

type GPUInfo struct {
	GPUID        int      `json:"gpu_id"`
	UUID         string   `json:"uuid"`
	PCIAddress   string   `json:"pci_address"`
	SerialNumber string   `json:"serial_number"`
	DeviceName   string   `json:"device_name"`
	NVLinks      []NVLink `json:"nvlinks"`
}

type NVLink struct {
	LinkID           int    `json:"link_id"`
	RemotePCIAddress string `json:"remote_pci_address"`
	RemoteLinkID     int    `json:"remote_link_id"`
}
