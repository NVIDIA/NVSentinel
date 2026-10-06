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

const (
	PodDeviceAnnotationName = "dgxc.nvidia.com/devices"

	// GPUDRADriverName is the name of the NVIDIA DRA driver for GPUs. The driver also gives this name to its
	// DeviceClass for full GPUs, in deployments/helm/dra-driver-nvidia-gpu/templates/deviceclass-gpu.yaml of
	// https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/495bf4c59b9423080aa1fe2163955f44a495012c
	GPUDRADriverName = "gpu.nvidia.com"
)

var (
	EntityTypeToResourceNames = map[string][]string{
		"GPU_UUID": {
			"nvidia.com/gpu",
			"nvidia.com/pgpu",
			// DRA driver name used for GPUs allocated through ResourceClaims (GPU Operator GPUCluster mode).
			GPUDRADriverName,
		},
	}
)

type DeviceAnnotation struct {
	Devices map[string][]string `json:"devices"`
}
