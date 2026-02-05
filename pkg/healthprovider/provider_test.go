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
	"testing"
	"time"

	"k8s.io/klog/v2"
)

// mockHealthSource is a mock implementation of HealthSource for testing.
type mockHealthSource struct {
	name    string
	running bool
	startFn func(ctx context.Context, handler HealthEventHandler) error
	stopFn  func()
}

func (m *mockHealthSource) Name() string {
	return m.name
}

func (m *mockHealthSource) Start(ctx context.Context, handler HealthEventHandler) error {
	if m.startFn != nil {
		return m.startFn(ctx, handler)
	}
	m.running = true
	return nil
}

func (m *mockHealthSource) Stop() {
	if m.stopFn != nil {
		m.stopFn()
	}
	m.running = false
}

func (m *mockHealthSource) IsRunning() bool {
	return m.running
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ServerAddress != DefaultServerAddress {
		t.Errorf("ServerAddress = %v, want %v", cfg.ServerAddress, DefaultServerAddress)
	}
	if cfg.HealthCheckPort != DefaultHealthCheckPort {
		t.Errorf("HealthCheckPort = %v, want %v", cfg.HealthCheckPort, DefaultHealthCheckPort)
	}
	if cfg.HeartbeatInterval != DefaultHeartbeatInterval {
		t.Errorf("HeartbeatInterval = %v, want %v", cfg.HeartbeatInterval, DefaultHeartbeatInterval)
	}
}

func TestProviderRegisterSource(t *testing.T) {
	logger := klog.Background()
	provider := New(DefaultConfig(), logger)

	source1 := &mockHealthSource{name: "source1"}
	source2 := &mockHealthSource{name: "source2"}

	provider.RegisterSource(source1)
	provider.RegisterSource(source2)

	if len(provider.sources) != 2 {
		t.Errorf("Expected 2 sources, got %d", len(provider.sources))
	}
}

func TestProviderIsConnected(t *testing.T) {
	logger := klog.Background()
	provider := New(DefaultConfig(), logger)

	// Initially not connected
	if provider.IsConnected() {
		t.Error("Expected not connected initially")
	}

	// Simulate connection
	provider.mu.Lock()
	provider.connected = true
	provider.mu.Unlock()

	if !provider.IsConnected() {
		t.Error("Expected connected after setting flag")
	}
}

func TestProviderIsHealthy(t *testing.T) {
	logger := klog.Background()
	provider := New(DefaultConfig(), logger)

	// Initially not healthy
	if provider.IsHealthy() {
		t.Error("Expected not healthy initially")
	}

	// Set healthy
	provider.setHealthy(true)

	if !provider.IsHealthy() {
		t.Error("Expected healthy after setting")
	}
}

func TestProviderGPUCount(t *testing.T) {
	logger := klog.Background()
	provider := New(DefaultConfig(), logger)

	if provider.GPUCount() != 0 {
		t.Error("Expected 0 GPUs initially")
	}

	// Simulate GPU registration
	provider.mu.Lock()
	provider.gpuUUIDs["GPU-123"] = true
	provider.gpuUUIDs["GPU-456"] = true
	provider.mu.Unlock()

	if provider.GPUCount() != 2 {
		t.Errorf("Expected 2 GPUs, got %d", provider.GPUCount())
	}
}

func TestGPUInfo(t *testing.T) {
	gpu := &GPUInfo{
		UUID:        "GPU-12345678",
		ProductName: "NVIDIA A100",
		MemoryBytes: 40 * 1024 * 1024 * 1024, // 40 GB
		Index:       0,
		Source:      "nvml",
	}

	if gpu.UUID != "GPU-12345678" {
		t.Errorf("UUID = %v, want GPU-12345678", gpu.UUID)
	}
	if gpu.Source != "nvml" {
		t.Errorf("Source = %v, want nvml", gpu.Source)
	}
}

func TestHealthEvent(t *testing.T) {
	event := &HealthEvent{
		GPUUID:     "GPU-12345678",
		Source:     "nvml",
		IsHealthy:  false,
		IsFatal:    true,
		ErrorCodes: []string{"79"},
		Reason:     "CriticalXIDError",
		Message:    "GPU has fallen off the bus",
		DetectedAt: time.Now(),
	}

	if event.GPUUID != "GPU-12345678" {
		t.Errorf("GPUUID = %v, want GPU-12345678", event.GPUUID)
	}
	if event.IsHealthy {
		t.Error("Expected IsHealthy to be false")
	}
	if !event.IsFatal {
		t.Error("Expected IsFatal to be true")
	}
	if len(event.ErrorCodes) != 1 || event.ErrorCodes[0] != "79" {
		t.Errorf("ErrorCodes = %v, want [79]", event.ErrorCodes)
	}
}

func TestMockHealthSource(t *testing.T) {
	started := false
	stopped := false

	source := &mockHealthSource{
		name: "test-source",
		startFn: func(ctx context.Context, handler HealthEventHandler) error {
			started = true
			return nil
		},
		stopFn: func() {
			stopped = true
		},
	}

	if source.Name() != "test-source" {
		t.Errorf("Name() = %v, want test-source", source.Name())
	}

	ctx := context.Background()
	if err := source.Start(ctx, nil); err != nil {
		t.Errorf("Start() error = %v", err)
	}
	if !started {
		t.Error("Expected startFn to be called")
	}

	source.Stop()
	if !stopped {
		t.Error("Expected stopFn to be called")
	}
}
