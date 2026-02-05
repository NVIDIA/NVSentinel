//go:build !nvml

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

package healthprovider

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
)

const NVMLSourceName = "nvml"

// NVMLSourceConfig holds configuration for the NVML health source.
type NVMLSourceConfig struct {
	DriverRoot         string
	IgnoredXids        []uint64
	HealthCheckEnabled bool
}

// DefaultNVMLSourceConfig returns default NVML source configuration.
func DefaultNVMLSourceConfig() NVMLSourceConfig {
	return NVMLSourceConfig{
		DriverRoot:         "/run/nvidia/driver",
		HealthCheckEnabled: true,
	}
}

// NVMLSource is a stub implementation when NVML is not available.
type NVMLSource struct {
	logger klog.Logger
}

// NewNVMLSource creates a stub NVML source.
func NewNVMLSource(_ NVMLSourceConfig, logger klog.Logger) *NVMLSource {
	return &NVMLSource{logger: logger.WithName("nvml-source-stub")}
}

// Name returns the source identifier.
func (s *NVMLSource) Name() string {
	return NVMLSourceName
}

// Start returns an error indicating NVML is not available.
func (s *NVMLSource) Start(_ context.Context, _ HealthEventHandler) error {
	s.logger.Info("NVML source not available (build without nvml tag)")
	return fmt.Errorf("NVML not available: binary built without nvml tag")
}

// Stop is a no-op for the stub.
func (s *NVMLSource) Stop() {}

// IsRunning always returns false for the stub.
func (s *NVMLSource) IsRunning() bool {
	return false
}

// GPUUUIDs returns an empty slice for the stub.
func (s *NVMLSource) GPUUUIDs() []string {
	return nil
}

// IsCriticalXid returns false for the stub.
func IsCriticalXid(_ uint64) bool {
	return false
}

// DefaultIgnoredXids for stub.
var DefaultIgnoredXids = []uint64{}

// CriticalXids for stub.
var CriticalXids = map[uint64]bool{}
