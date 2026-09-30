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
	// draGPUDriverName is the DRA driver GPU Operator GPUCluster mode allocates GPUs through. It is also the
	// key the resolved UUIDs are written under in the devices annotation.
	draGPUDriverName = "gpu.nvidia.com"
	// draGPUDeviceNameFormat is how the NVIDIA DRA driver names a full GPU from its minor number, the N in
	// /dev/nvidiaN. It mirrors GpuInfo.CanonicalName() in cmd/gpu-kubelet-plugin/deviceinfo.go
	// (https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/495bf4c59b9423080aa1fe2163955f44a495012c/cmd/gpu-kubelet-plugin/deviceinfo.go#L122),
	// which the driver keeps on purpose because the minor is fixed for as long as the GPU stays on the bus.
	draGPUDeviceNameFormat = "gpu-%d"
)

// draDeviceResolver maps a DRA device name to a GPU UUID. A nil resolver leaves DRA allocations unmapped.
type draDeviceResolver func(deviceName string) (string, bool)

// newMinorNumberResolver resolves DRA device names through NVML, the source of truth on the node, with no API
// server access: each GPU's minor number is formatted the way the driver formats it and compared to the
// allocated name, so anything the driver would not have produced fails closed. uuidsByMinor runs on every
// lookup, so a renumbered node is picked up on the next poll without a restart.
func newMinorNumberResolver(uuidsByMinor func() (map[int]string, error)) draDeviceResolver {
	return func(deviceName string) (string, bool) {
		uuids, err := uuidsByMinor()
		if err != nil {
			slog.Warn("Could not read GPU minor numbers from NVML", "error", err)

			return "", false
		}

		for minor, uuid := range uuids {
			if fmt.Sprintf(draGPUDeviceNameFormat, minor) == deviceName {
				return uuid, true
			}
		}

		return "", false
	}
}
