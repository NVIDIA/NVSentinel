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
	"bufio"
	"bytes"
	"os/exec"
	"regexp"
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

// We need to mock exec.Cmd for unit tests, so we need an interface
type command interface {
	Output() ([]byte, error)
}

type commandImpl struct {
	cmd *exec.Cmd
}

func (r *commandImpl) Output() ([]byte, error) {
	return r.cmd.Output()
}

var commandExec = func(name string, arg ...string) command {
	return &commandImpl{cmd: exec.Command(name, arg...)}
}

func GetDGXType() DGXHardwareType {
	cmd := commandExec("lspci")
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

func GetNVSwitchPCIAddresses() []string {
	cmd := commandExec("lspci", "-D", "-k")
	out, err := cmd.Output()
	if err != nil {
		klog.Errorf("failed to execute lspci: %v", err)
		return nil
	}

	// matches lines starting with a PCI address like '0000:04:00.0'
	pciRegex := regexp.MustCompile(`^([0-9a-fA-F]{4}):[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]{1,2}`)

	// matches lines like 'Kernel driver in use: nvidia-nvswitch' and captures the driver name
	driverRegex := regexp.MustCompile(`^\s*Kernel driver in use:\s*(\S+)`)

	var pciAddresses []string
	currentPCI := ""

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()

		if matches := pciRegex.FindStringSubmatch(line); matches != nil {
			currentPCI = matches[0]
			continue
		}

		if matches := driverRegex.FindStringSubmatch(line); matches != nil {
			driver := matches[1]
			// check if the driver is 'nvidia-nvswitch' and a PCI address has been captured
			if driver == "nvidia-nvswitch" && currentPCI != "" {
				pciAddresses = append(pciAddresses, currentPCI)
				currentPCI = ""
			}
		}
	}

	if err := scanner.Err(); err != nil {
		klog.Errorf("Error reading lspci output: %v", err)
		return nil
	}

	return pciAddresses
}
