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

//go:build !nvml

// Package nvml provides a built-in NVML-based health provider for the Device API Server.
//
// This stub file is used when NVML support is not compiled in (build without -tags=nvml).
package nvml

import (
	"context"
	"errors"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

// ErrNVMLNotCompiled is returned when NVML support is not compiled into the binary.
var ErrNVMLNotCompiled = errors.New("NVML support not compiled in (build with -tags=nvml)")

// Provider is the built-in NVML-based health provider (stub when not compiled).
type Provider struct{}

// Config holds configuration for the NVML provider.
type Config struct {
	DriverRoot            string
	AdditionalIgnoredXids []uint64
	HealthCheckEnabled    bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DriverRoot:            "/run/nvidia/driver",
		AdditionalIgnoredXids: nil,
		HealthCheckEnabled:    true,
	}
}

// New creates a new NVML provider (stub).
func New(cfg Config, client v1alpha1.GpuServiceClient, logger klog.Logger) *Provider {
	return &Provider{}
}

// Start initializes NVML (stub - always returns error).
func (p *Provider) Start(ctx context.Context) error {
	return ErrNVMLNotCompiled
}

// Stop shuts down the NVML provider (stub - no-op).
func (p *Provider) Stop() {}

// IsInitialized returns false (stub).
func (p *Provider) IsInitialized() bool {
	return false
}

// GPUCount returns 0 (stub).
func (p *Provider) GPUCount() int {
	return 0
}

// IsHealthMonitorRunning returns false (stub).
func (p *Provider) IsHealthMonitorRunning() bool {
	return false
}

// ParseIgnoredXids parses a comma-separated string of XID values (stub).
func ParseIgnoredXids(input string) []uint64 {
	return nil
}
