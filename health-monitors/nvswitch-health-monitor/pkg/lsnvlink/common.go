// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package lsnvlink

import (
	"os/exec"
	"strings"

	"k8s.io/klog"
)

const (
	A100_GPU_COUNT           = 8
	A100_NVSWITCH_COUNT      = 6
	A100_NVLINK_PER_GPU      = 12
	A100_NVLINK_PER_NVSWITCH = 16

	H100_GPU_COUNT      = 8
	H100_NVSWITCH_COUNT = 4
	H100_NVLINK_PER_GPU = 18 // 5, 4, 4, 5 for each nvswitch
	// H100_NVLINK_PER_NVSWITCH = 20 for nvswitch 0, 4
	//                            16 for nvswitch 1, 2
)

type DGXHardware interface {
	ShowGpuNVLink(gpu int) (string, error)
	ShowNVSwitchNVLink(nvswitch int) (string, error)
	GetNVSwitchFromGpuNVLink(gpu, nvlink int) (int, error)
	GetGpuFromNVSwitchNVLink(nvswitch, nvlink int) (int, error)
}

type DGXHardwareType int

const (
	DGX_TYPE_UNKNOWN DGXHardwareType = iota
	DGX_TYPE_A100
	DGX_TYPE_H100
)

func GetDGXType() DGXHardwareType {
	cmd := exec.Command("lspci")
	out, err := cmd.Output()
	if err != nil {
		klog.Errorf("failed to query lspci: %s\n", err)
	}

	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.Contains(line, "NVIDIA") {
			if strings.Contains(line, "GA100") {
				return DGX_TYPE_A100
			}

			if strings.Contains(line, "GH100") {
				return DGX_TYPE_H100
			}
		}
	}

	return DGX_TYPE_UNKNOWN
}
