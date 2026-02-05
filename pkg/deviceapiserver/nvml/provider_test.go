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
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"

	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/cache"
)

// testLogger returns a test logger.
func testLogger() klog.Logger {
	return klog.NewKlogr().WithName("test")
}

// TestProvider_Start_Success tests successful provider initialization.
func TestProvider_Start_Success(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.AddDevice(0, NewMockDevice("GPU-uuid-0", "NVIDIA A100"))
	mockLib.AddDevice(1, NewMockDevice("GPU-uuid-1", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Verify NVML was initialized
	if !mockLib.InitCalled {
		t.Error("Init() was not called")
	}

	// Verify GPUs were registered in cache
	if gpuCache.Count() != 2 {
		t.Errorf("Expected 2 GPUs in cache, got %d", gpuCache.Count())
	}

	// Verify provider state
	if !provider.IsInitialized() {
		t.Error("Provider should be initialized")
	}

	if provider.GPUCount() != 2 {
		t.Errorf("Expected GPUCount() = 2, got %d", provider.GPUCount())
	}
}

// TestProvider_Start_NVMLInitFails tests graceful handling of NVML init failure.
func TestProvider_Start_NVMLInitFails(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.InitReturn = nvml.ERROR_LIBRARY_NOT_FOUND

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx := context.Background()
	err := provider.Start(ctx)

	if err == nil {
		t.Fatal("Expected Start() to fail when NVML init fails")
	}

	if provider.IsInitialized() {
		t.Error("Provider should not be initialized after failure")
	}
}

// TestProvider_Start_NoGPUs tests handling of nodes without GPUs.
func TestProvider_Start_NoGPUs(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.DeviceCount = 0

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	if provider.GPUCount() != 0 {
		t.Errorf("Expected 0 GPUs, got %d", provider.GPUCount())
	}

	// Health monitor should not be running with 0 GPUs
	if provider.IsHealthMonitorRunning() {
		t.Error("Health monitor should not run with 0 GPUs")
	}
}

// TestProvider_Start_AlreadyStarted tests double-start prevention.
func TestProvider_Start_AlreadyStarted(t *testing.T) {
	mockLib := NewMockLibrary()
	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First start
	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}
	defer provider.Stop()

	// Second start should fail
	err = provider.Start(ctx)
	if err == nil {
		t.Error("Second Start() should fail")
	}
}

// TestProvider_Stop tests provider shutdown.
func TestProvider_Stop(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.AddDevice(0, NewMockDevice("GPU-uuid-0", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Stop the provider
	provider.Stop()

	// Verify state
	if provider.IsInitialized() {
		t.Error("Provider should not be initialized after Stop()")
	}

	if !mockLib.ShutdownCalled {
		t.Error("NVML Shutdown() was not called")
	}

	// Double stop should be safe
	provider.Stop()
}

// TestProvider_Stop_NotStarted tests Stop() on unstarted provider.
func TestProvider_Stop_NotStarted(t *testing.T) {
	mockLib := NewMockLibrary()
	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	// Stop should be safe even if not started
	provider.Stop()

	if mockLib.ShutdownCalled {
		t.Error("Shutdown() should not be called if provider was never started")
	}
}

// TestProvider_DeviceEnumeration tests that devices are properly enumerated.
func TestProvider_DeviceEnumeration(t *testing.T) {
	mockLib := NewMockLibrary()

	// Add devices with varying configurations
	device0 := NewMockDevice("GPU-11111111-1111-1111-1111-111111111111", "NVIDIA H100")
	device0.MemoryInfo = nvml.Memory{Total: 80 * 1024 * 1024 * 1024} // 80 GB

	device1 := NewMockDevice("GPU-22222222-2222-2222-2222-222222222222", "NVIDIA A100")
	device1.MemoryInfo = nvml.Memory{Total: 40 * 1024 * 1024 * 1024} // 40 GB

	mockLib.AddDevice(0, device0)
	mockLib.AddDevice(1, device1)

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Verify both devices are in cache
	gpus := gpuCache.List()
	if len(gpus) != 2 {
		t.Fatalf("Expected 2 GPUs, got %d", len(gpus))
	}

	// Verify GPU details
	uuids := make(map[string]bool)
	for _, gpu := range gpus {
		uuids[gpu.GetMetadata().GetName()] = true

		// Check initial condition
		if gpu.Status == nil || len(gpu.Status.Conditions) == 0 {
			t.Errorf("GPU %s has no conditions", gpu.GetMetadata().GetName())
			continue
		}

		cond := gpu.Status.Conditions[0]
		if cond.Type != ConditionTypeNVMLReady {
			t.Errorf("Expected condition type %s, got %s", ConditionTypeNVMLReady, cond.Type)
		}

		if cond.Status != ConditionStatusTrue {
			t.Errorf("Expected condition status True, got %s", cond.Status)
		}
	}

	if !uuids["GPU-11111111-1111-1111-1111-111111111111"] {
		t.Error("GPU-11111111... not found in cache")
	}

	if !uuids["GPU-22222222-2222-2222-2222-222222222222"] {
		t.Error("GPU-22222222... not found in cache")
	}
}

// TestProvider_DeviceEnumeration_PartialFailure tests handling of partial device failures.
func TestProvider_DeviceEnumeration_PartialFailure(t *testing.T) {
	mockLib := NewMockLibrary()

	// First device is fine
	mockLib.AddDevice(0, NewMockDevice("GPU-good", "NVIDIA A100"))

	// Second device fails UUID retrieval
	device1 := NewMockDevice("GPU-bad", "NVIDIA A100")
	device1.UUIDReturn = nvml.ERROR_UNKNOWN
	mockLib.AddDevice(1, device1)

	// Third device is fine
	mockLib.AddDevice(2, NewMockDevice("GPU-good-2", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	provider := &Provider{
		config:  DefaultConfig(),
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Only 2 GPUs should be in cache (one failed)
	if gpuCache.Count() != 2 {
		t.Errorf("Expected 2 GPUs (1 failed), got %d", gpuCache.Count())
	}
}

// TestProvider_HealthCheckDisabled tests that health monitoring can be disabled.
func TestProvider_HealthCheckDisabled(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.AddDevice(0, NewMockDevice("GPU-uuid-0", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	config := DefaultConfig()
	config.HealthCheckEnabled = false

	provider := &Provider{
		config:  config,
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Give a moment for any goroutines to start
	time.Sleep(10 * time.Millisecond)

	if provider.IsHealthMonitorRunning() {
		t.Error("Health monitor should not be running when disabled")
	}
}

// TestProvider_UpdateCondition tests condition updates.
func TestProvider_UpdateCondition(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.AddDevice(0, NewMockDevice("GPU-uuid-0", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	config := DefaultConfig()
	config.HealthCheckEnabled = false

	provider := &Provider{
		config:  config,
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Update condition to unhealthy
	err = provider.UpdateCondition("GPU-uuid-0", ConditionTypeNVMLReady, ConditionStatusFalse, "XidError", "Critical XID 48")
	if err != nil {
		t.Fatalf("UpdateCondition() failed: %v", err)
	}

	// Verify condition was updated
	gpu, found := gpuCache.Get("GPU-uuid-0")
	if !found {
		t.Fatal("GPU not found in cache")
	}

	var foundCondition bool

	for _, cond := range gpu.Status.Conditions {
		if cond.Type == ConditionTypeNVMLReady {
			foundCondition = true

			if cond.Status != ConditionStatusFalse {
				t.Errorf("Expected status False, got %s", cond.Status)
			}

			if cond.Reason != "XidError" {
				t.Errorf("Expected reason XidError, got %s", cond.Reason)
			}
		}
	}

	if !foundCondition {
		t.Error("NVMLReady condition not found")
	}
}

// TestProvider_UpdateCondition_GPUNotFound tests condition update for non-existent GPU.
func TestProvider_UpdateCondition_GPUNotFound(t *testing.T) {
	mockLib := NewMockLibrary()
	gpuCache := cache.New(testLogger(), nil)

	config := DefaultConfig()
	config.HealthCheckEnabled = false

	provider := &Provider{
		config:  config,
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// Try to update condition for non-existent GPU
	err = provider.UpdateCondition("GPU-nonexistent", ConditionTypeNVMLReady, ConditionStatusFalse, "XidError", "Test")
	if err == nil {
		t.Error("Expected error for non-existent GPU")
	}
}

// TestProvider_MarkHealthy tests marking a GPU as healthy.
func TestProvider_MarkHealthy(t *testing.T) {
	mockLib := NewMockLibrary()
	mockLib.AddDevice(0, NewMockDevice("GPU-uuid-0", "NVIDIA A100"))

	gpuCache := cache.New(testLogger(), nil)

	config := DefaultConfig()
	config.HealthCheckEnabled = false

	provider := &Provider{
		config:  config,
		nvmllib: mockLib,
		cache:   gpuCache,
		logger:  testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := provider.Start(ctx)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer provider.Stop()

	// First mark as unhealthy
	err = provider.UpdateCondition("GPU-uuid-0", ConditionTypeNVMLReady, ConditionStatusFalse, "XidError", "Test")
	if err != nil {
		t.Fatalf("UpdateCondition() failed: %v", err)
	}

	// Then mark as healthy
	err = provider.MarkHealthy("GPU-uuid-0")
	if err != nil {
		t.Fatalf("MarkHealthy() failed: %v", err)
	}

	// Verify it's healthy
	gpu, found := gpuCache.Get("GPU-uuid-0")
	if !found {
		t.Fatal("GPU not found")
	}

	for _, cond := range gpu.Status.Conditions {
		if cond.Type == ConditionTypeNVMLReady {
			if cond.Status != ConditionStatusTrue {
				t.Errorf("Expected status True after MarkHealthy, got %s", cond.Status)
			}

			return
		}
	}

	t.Error("NVMLReady condition not found")
}
