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

package mapper

import (
	"fmt"
	"log/slog"
)

const (
	draGPUDriverName = "gpu.nvidia.com"
	// draGPUDeviceNameFormat is how the NVIDIA DRA driver names a full GPU from its minor number, the N in
	// /dev/nvidiaN. It mirrors GpuInfo.CanonicalName() in cmd/gpu-kubelet-plugin/deviceinfo.go
	// (https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/495bf4c59b9423080aa1fe2163955f44a495012c/cmd/gpu-kubelet-plugin/deviceinfo.go#L122),
	// which the driver keeps on purpose because the minor is fixed for as long as the GPU stays on the bus.
	draGPUDeviceNameFormat = "gpu-%d"
)

// draDeviceResolver maps a DRA device name to a GPU UUID.
type draDeviceResolver func(deviceName string) (string, bool)

func newMinorNumberResolver(uuidsByMinor func() (map[int]string, error)) draDeviceResolver {
	var uuidsByName map[string]string

	return func(deviceName string) (string, bool) {
		if uuidsByName == nil {
			uuidsByName = make(map[string]string)

			uuids, err := uuidsByMinor()
			if err != nil {
				slog.Warn("Could not read GPU minor numbers from NVML", "error", err)
			}

			for minor, uuid := range uuids {
				uuidsByName[fmt.Sprintf(draGPUDeviceNameFormat, minor)] = uuid
			}
		}

		uuid, ok := uuidsByName[deviceName]

		return uuid, ok
	}
}
