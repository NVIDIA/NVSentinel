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

// Package nvml provides a built-in NVML-based health provider for the Device API Server.
//
// This provider uses NVML (NVIDIA Management Library) to:
//   - Enumerate GPUs on the node at startup
//   - Monitor GPU health via XID error events
//   - Provide baseline device information when no external providers are connected
//
// The provider requires the NVIDIA driver to be installed and NVML libraries to be
// accessible. When running in Kubernetes, this is typically achieved by using the
// "nvidia" RuntimeClass which injects the driver libraries via the NVIDIA Container
// Toolkit, without consuming GPU resources.
package nvml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

// Provider is the built-in NVML-based health provider.
//
// It uses NVML to enumerate GPUs and monitor their health status.
// The provider is optional and gracefully degrades if NVML is unavailable.
//
// The provider communicates with the Device API Server via the gRPC client
// interface, making it a "dogfooding" client of its own API. This design:
//   - Decouples the provider from server internals
//   - Enables running the provider as a separate sidecar process
//   - Validates the API from a provider's perspective
type Provider struct {
	// Configuration
	config Config

	// NVML library interface (uses our wrapper for testability)
	nvmllib Library

	// gRPC client to communicate with Device API Server
	client v1alpha1.GpuServiceClient

	// Logger
	logger klog.Logger

	// Health monitoring
	eventSet       EventSet
	healthMonitor  *HealthMonitor
	monitorRunning bool

	// Lifecycle management
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// State
	initialized bool
	gpuCount    int

	// Tracked GPU UUIDs for health monitoring
	gpuUUIDs []string
}

// Config holds configuration for the NVML provider.
type Config struct {
	// DriverRoot is the root path where NVIDIA driver libraries are located.
	// Common values:
	//   - "/run/nvidia/driver" (container with CDI/RuntimeClass)
	//   - "/" (bare metal or host path mount)
	DriverRoot string

	// AdditionalIgnoredXids is a list of additional XID error codes to ignore.
	// These are added to the default list of ignored XIDs (application errors).
	AdditionalIgnoredXids []uint64

	// HealthCheckEnabled enables XID event monitoring for health checks.
	// When disabled, only device enumeration is performed.
	HealthCheckEnabled bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DriverRoot:            "/run/nvidia/driver",
		AdditionalIgnoredXids: nil,
		HealthCheckEnabled:    true,
	}
}

// New creates a new NVML provider.
//
// The provider is not started until Start() is called. If NVML cannot be
// initialized (e.g., no driver installed), Start() will return an error
// but the server can continue without NVML support.
//
// The client parameter is a GpuServiceClient used to communicate with the
// Device API Server. This enables the provider to be either:
//   - Co-located with the server (using a loopback connection)
//   - Running as a separate sidecar process (using a network connection)
func New(cfg Config, client v1alpha1.GpuServiceClient, logger klog.Logger) *Provider {
	logger = logger.WithName("nvml-provider")

	// Find NVML library path
	libraryPath := findDriverLibrary(cfg.DriverRoot)
	logger.V(2).Info("Using NVML library path", "path", libraryPath)

	// Create NVML interface with explicit library path
	var rawLib nvml.Interface
	if libraryPath != "" {
		rawLib = nvml.New(nvml.WithLibraryPath(libraryPath))
	} else {
		// Fall back to system default
		rawLib = nvml.New()
	}

	return &Provider{
		config:  cfg,
		nvmllib: NewLibraryWrapper(rawLib),
		client:  client,
		logger:  logger,
	}
}

// Start initializes NVML and enumerates GPUs.
//
// If health checking is enabled, it also starts the XID event monitoring
// goroutine. Returns an error if NVML cannot be initialized.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.initialized {
		return fmt.Errorf("provider already started")
	}

	p.logger.Info("Starting NVML provider")

	// Initialize NVML
	ret := p.nvmllib.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", nvml.ErrorString(ret))
	}

	// Get driver version for logging
	driverVersion, ret := p.nvmllib.SystemGetDriverVersion()
	if ret == nvml.SUCCESS {
		p.logger.Info("NVML initialized", "driverVersion", driverVersion)
	}

	// Enumerate devices
	count, err := p.enumerateDevices()
	if err != nil {
		_ = p.nvmllib.Shutdown()

		return fmt.Errorf("failed to enumerate devices: %w", err)
	}

	p.gpuCount = count

	p.logger.Info("Enumerated GPUs", "count", count)

	// Set up context for lifecycle management
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.initialized = true

	// Start health monitoring if enabled and we have GPUs
	if p.config.HealthCheckEnabled && count > 0 {
		if err := p.startHealthMonitoring(); err != nil {
			p.logger.Error(err, "Failed to start health monitoring, continuing without it")
			// Don't fail - health monitoring is optional
		}
	}

	return nil
}

// Stop shuts down the NVML provider.
//
// It stops health monitoring (if running) and shuts down NVML.
// This method is safe to call multiple times.
func (p *Provider) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.initialized {
		return
	}

	p.logger.Info("Stopping NVML provider")

	// Cancel context to stop health monitoring
	if p.cancel != nil {
		p.cancel()
	}

	// Wait for health monitor to stop
	p.wg.Wait()

	// Clean up event set
	if p.eventSet != nil {
		if ret := p.eventSet.Free(); ret != nvml.SUCCESS {
			p.logger.V(1).Info("Failed to free event set", "error", nvml.ErrorString(ret))
		}

		p.eventSet = nil
	}

	// Shutdown NVML
	if ret := p.nvmllib.Shutdown(); ret != nvml.SUCCESS {
		p.logger.V(1).Info("Failed to shutdown NVML", "error", nvml.ErrorString(ret))
	}

	p.initialized = false
	p.monitorRunning = false
	p.logger.Info("NVML provider stopped")
}

// IsInitialized returns true if the provider has been successfully started.
func (p *Provider) IsInitialized() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initialized
}

// GPUCount returns the number of GPUs discovered.
func (p *Provider) GPUCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.gpuCount
}

// IsHealthMonitorRunning returns true if health monitoring is active.
func (p *Provider) IsHealthMonitorRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.monitorRunning
}

// findDriverLibrary locates the NVML library in the driver root.
//
// It searches common paths where libnvidia-ml.so.1 might be located.
// Returns empty string if not found (will use system default).
func findDriverLibrary(driverRoot string) string {
	if driverRoot == "" {
		return ""
	}

	// Common paths for libnvidia-ml.so.1
	searchPaths := []string{
		filepath.Join(driverRoot, "usr/lib64/libnvidia-ml.so.1"),
		filepath.Join(driverRoot, "usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"),
		filepath.Join(driverRoot, "usr/lib/libnvidia-ml.so.1"),
		filepath.Join(driverRoot, "lib64/libnvidia-ml.so.1"),
		filepath.Join(driverRoot, "lib/libnvidia-ml.so.1"),
	}

	for _, path := range searchPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}
