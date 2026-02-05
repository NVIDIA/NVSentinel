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

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

// ReconcileState reconciles the provider's state with the device-api-server.
//
// This is called on startup and after reconnection to ensure:
// 1. GPUs that were removed while disconnected are unregistered
// 2. GPUs that were added while disconnected are registered
// 3. GPU health states are reconciled with current NVML state
//
// This handles scenarios like:
// - Provider crash and restart
// - Network partition recovery
// - GPU hotplug/removal during provider downtime
func (p *Provider) ReconcileState(ctx context.Context) error {
	p.logger.Info("Starting state reconciliation")

	// Step 1: Get current state from server
	cachedGPUs, err := p.listCachedGPUs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list cached GPUs: %w", err)
	}

	p.logger.V(1).Info("Retrieved cached GPU state", "count", len(cachedGPUs))

	// Step 2: Get current GPU UUIDs from NVML
	currentUUIDs, err := p.getCurrentGPUUUIDs()
	if err != nil {
		return fmt.Errorf("failed to get current GPU UUIDs: %w", err)
	}

	p.logger.V(1).Info("Current GPUs from NVML", "count", len(currentUUIDs))

	// Build lookup maps
	cachedUUIDSet := make(map[string]*v1alpha1.Gpu)
	for _, gpu := range cachedGPUs {
		cachedUUIDSet[gpu.GetSpec().GetUuid()] = gpu
	}

	currentUUIDSet := make(map[string]bool)
	for _, uuid := range currentUUIDs {
		currentUUIDSet[uuid] = true
	}

	// Step 3: Find and unregister removed GPUs
	for uuid := range cachedUUIDSet {
		if !currentUUIDSet[uuid] {
			p.logger.Info("GPU was removed, unregistering", "uuid", uuid)
			if err := p.unregisterGPU(ctx, uuid); err != nil {
				p.logger.Error(err, "Failed to unregister removed GPU", "uuid", uuid)
				// Continue with other GPUs
			}
		}
	}

	// Step 4: Find and register new GPUs
	for _, uuid := range currentUUIDs {
		if _, exists := cachedUUIDSet[uuid]; !exists {
			p.logger.Info("New GPU found, registering", "uuid", uuid)
			if err := p.registerNewGPU(ctx, uuid); err != nil {
				p.logger.Error(err, "Failed to register new GPU", "uuid", uuid)
				// Continue with other GPUs
			}
		}
	}

	// Step 5: Reconcile health state for existing GPUs
	for _, uuid := range currentUUIDs {
		if cachedGPU, exists := cachedUUIDSet[uuid]; exists {
			if err := p.reconcileGPUHealth(ctx, uuid, cachedGPU); err != nil {
				p.logger.Error(err, "Failed to reconcile GPU health", "uuid", uuid)
				// Continue with other GPUs
			}
		}
	}

	// Step 6: Update local GPU list
	p.mu.Lock()
	p.gpuUUIDs = currentUUIDs
	p.mu.Unlock()

	p.logger.Info("State reconciliation complete",
		"totalGPUs", len(currentUUIDs),
	)

	return nil
}

// listCachedGPUs retrieves the list of GPUs from the server cache.
//
// Note: This lists ALL GPUs, not just those from this provider.
// TODO: Add provider_id filtering to ListGpus RPC for efficiency.
func (p *Provider) listCachedGPUs(ctx context.Context) ([]*v1alpha1.Gpu, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := &v1alpha1.ListGpusRequest{}
	resp, err := p.gpuClient.ListGpus(ctx, req)
	if err != nil {
		return nil, err
	}

	// Filter to only GPUs that might belong to this provider
	// For now, we assume all GPUs belong to us since we're the only provider
	// A more robust solution would use provider_id filtering
	return resp.GetGpuList().GetItems(), nil
}

// getCurrentGPUUUIDs gets the list of GPU UUIDs currently visible to NVML.
func (p *Provider) getCurrentGPUUUIDs() ([]string, error) {
	count, ret := p.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	uuids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			continue
		}

		uuids = append(uuids, uuid)
	}

	return uuids, nil
}

// unregisterGPU removes a GPU from the server using DeleteGpu.
func (p *Provider) unregisterGPU(ctx context.Context, uuid string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &v1alpha1.DeleteGpuRequest{
		Name: uuid,
	}

	_, err := p.gpuClient.DeleteGpu(ctx, req)
	return err
}

// registerNewGPU registers a newly discovered GPU.
func (p *Provider) registerNewGPU(ctx context.Context, uuid string) error {
	// Get device info from NVML
	productName := "Unknown"
	var memoryBytes uint64

	// Find the device by UUID
	count, ret := p.nvmllib.DeviceGetCount()
	if ret == nvml.SUCCESS {
		for i := 0; i < count; i++ {
			device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
			if ret != nvml.SUCCESS {
				continue
			}
			deviceUUID, ret := device.GetUUID()
			if ret != nvml.SUCCESS || deviceUUID != uuid {
				continue
			}

			// Found the device
			if name, ret := device.GetName(); ret == nvml.SUCCESS {
				productName = name
			}
			if memInfo, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
				memoryBytes = memInfo.Total
			}
			break
		}
	}

	return p.registerGPU(uuid, productName, memoryBytes)
}

// reconcileGPUHealth compares cached health state with current NVML state.
//
// If the GPU was marked as Unknown (due to provider timeout) but is now
// healthy per NVML, we update it back to healthy.
func (p *Provider) reconcileGPUHealth(ctx context.Context, uuid string, cachedGPU *v1alpha1.Gpu) error {
	// Check if the cached state shows Unknown (from heartbeat timeout)
	var cachedCondition *v1alpha1.Condition
	for _, cond := range cachedGPU.GetStatus().GetConditions() {
		if cond.GetType() == "Ready" || cond.GetType() == ConditionTypeNVMLReady {
			cachedCondition = cond
			break
		}
	}

	// If the condition is Unknown, query NVML and update if healthy
	if cachedCondition != nil && cachedCondition.GetStatus() == ConditionStatusUnknown {
		p.logger.Info("GPU has Unknown status, checking current NVML state", "uuid", uuid)

		// For now, if we can enumerate the GPU via NVML, consider it healthy
		// A more sophisticated check would query specific health indicators
		healthy, err := p.isGPUHealthy(uuid)
		if err != nil {
			return fmt.Errorf("failed to check GPU health: %w", err)
		}

		if healthy {
			p.logger.Info("GPU is healthy per NVML, updating status", "uuid", uuid)
			return p.updateGPUCondition(ctx, uuid, ConditionStatusTrue, "Recovered", "GPU recovered after provider reconnection")
		}
	}

	return nil
}

// isGPUHealthy checks if a GPU is healthy via NVML.
func (p *Provider) isGPUHealthy(uuid string) (bool, error) {
	// Find device by UUID
	count, ret := p.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return false, fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	for i := 0; i < count; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		deviceUUID, ret := device.GetUUID()
		if ret != nvml.SUCCESS || deviceUUID != uuid {
			continue
		}

		// Device found - check basic health indicators
		// 1. Can we get memory info? (basic liveness check)
		if _, ret := device.GetMemoryInfo(); ret != nvml.SUCCESS {
			return false, nil
		}

		// 2. Check for pending page retirements (ECC errors)
		if pending, ret := device.GetRetiredPagesPendingStatus(); ret == nvml.SUCCESS {
			if pending == nvml.FEATURE_ENABLED {
				p.logger.V(1).Info("GPU has pending page retirements", "uuid", uuid)
				return false, nil
			}
		}

		// Device is accessible and no pending issues
		return true, nil
	}

	// Device not found - not healthy
	return false, nil
}

// updateGPUCondition updates a GPU's status via UpdateGpuStatus.
func (p *Provider) updateGPUCondition(ctx context.Context, uuid, status, reason, message string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &v1alpha1.UpdateGpuStatusRequest{
		Name: uuid,
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{
					Type:               ConditionTypeNVMLReady,
					Status:             status,
					Reason:             reason,
					Message:            message,
					LastTransitionTime: timestamppb.Now(),
				},
			},
		},
	}

	_, err := p.gpuClient.UpdateGpuStatus(ctx, req)
	return err
}
