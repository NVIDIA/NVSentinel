// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

//go:build nvml

package nvml

import (
	"fmt"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

// Condition status values (string-based per proto definition).
const (
	ConditionStatusTrue    = "True"
	ConditionStatusFalse   = "False"
	ConditionStatusUnknown = "Unknown"
)

// enumerateDevices discovers all GPUs via NVML and registers them via gRPC.
//
// For each GPU found, it extracts device information and creates a GPU entry
// via the GpuService API with an initial "NVMLReady" condition set to True.
//
// Returns the number of GPUs discovered.
func (p *Provider) enumerateDevices() (int, error) {
	count, ret := p.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return 0, fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	if count == 0 {
		p.logger.Info("No GPUs found on this node")
		return 0, nil
	}

	p.logger.V(1).Info("Enumerating GPUs", "count", count)

	successCount := 0
	p.gpuUUIDs = make([]string, 0, count)

	for i := 0; i < count; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			p.logger.Error(nil, "Failed to get device handle", "index", i, "error", nvml.ErrorString(ret))

			continue
		}

		gpu, productName, memoryBytes, err := p.deviceToGpu(i, device)
		if err != nil {
			p.logger.Error(err, "Failed to get GPU info", "index", i)

			continue
		}

		// Register GPU via gRPC (CreateGpu is idempotent)
		resp, err := p.client.CreateGpu(p.ctx, &v1alpha1.CreateGpuRequest{Gpu: gpu})
		if err != nil {
			p.logger.Error(err, "Failed to create GPU via gRPC", "uuid", gpu.GetMetadata().GetName())

			continue
		}

		// Track UUID for health monitoring
		p.gpuUUIDs = append(p.gpuUUIDs, gpu.GetMetadata().GetName())

		if resp.Created {
			p.logger.Info("Created GPU",
				"uuid", gpu.GetMetadata().GetName(),
				"productName", productName,
				"memory", formatBytes(memoryBytes),
			)
		} else {
			p.logger.V(1).Info("GPU already exists",
				"uuid", gpu.GetMetadata().GetName(),
			)
		}

		successCount++
	}

	return successCount, nil
}

// deviceToGpu extracts GPU information from an NVML device handle.
// Returns the GPU proto, product name, and memory bytes (for logging).
func (p *Provider) deviceToGpu(index int, device Device) (*v1alpha1.Gpu, string, uint64, error) {
	// Get UUID (required)
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, "", 0, fmt.Errorf("failed to get UUID: %v", nvml.ErrorString(ret))
	}

	// Get memory info (for logging)
	var memoryBytes uint64

	memInfo, ret := device.GetMemoryInfo()
	if ret == nvml.SUCCESS {
		memoryBytes = memInfo.Total
	}

	// Get product name (for logging)
	productName, ret := device.GetName()
	if ret != nvml.SUCCESS {
		productName = "Unknown"
	}

	// Build GPU proto using available fields
	// Note: Current proto only has uuid in GpuSpec, additional info stored in status message
	now := timestamppb.Now()
	gpu := &v1alpha1.Gpu{
		Metadata: &v1alpha1.ObjectMeta{Name: uuid},
		Spec: &v1alpha1.GpuSpec{
			Uuid: uuid,
		},
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{
					Type:               ConditionTypeNVMLReady,
					Status:             ConditionStatusTrue,
					Reason:             "Initialized",
					Message:            fmt.Sprintf("GPU enumerated via NVML: %s (%s)", productName, formatBytes(memoryBytes)),
					LastTransitionTime: now,
				},
			},
		},
	}

	return gpu, productName, memoryBytes, nil
}

// formatBytes formats bytes to human-readable string.
func formatBytes(bytes uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// Condition constants for NVML provider.
const (
	// ConditionTypeNVMLReady is the condition type for NVML health status.
	ConditionTypeNVMLReady = "NVMLReady"

	// ConditionSourceNVML is the source identifier for conditions set by NVML provider.
	ConditionSourceNVML = "nvml-provider"
)

// UpdateCondition updates a condition on a GPU via the gRPC API.
//
// This completely replaces the GPU's status with a single condition.
// The condition's LastTransitionTime is set to the current time.
func (p *Provider) UpdateCondition(
	uuid string,
	conditionType string,
	status string,
	reason, message string,
) error {
	condition := &v1alpha1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: timestamppb.New(time.Now()),
	}

	_, err := p.client.UpdateGpuStatus(p.ctx, &v1alpha1.UpdateGpuStatusRequest{
		Name: uuid,
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{condition},
		},
	})

	return err
}
