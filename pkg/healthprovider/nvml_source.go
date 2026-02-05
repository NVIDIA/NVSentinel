//go:build nvml

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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

const (
	// NVMLSourceName is the identifier for the NVML health source.
	NVMLSourceName = "nvml"

	// EventTimeout is the timeout for NVML event wait (milliseconds).
	EventTimeout = 5000
)

// NVMLSourceConfig holds configuration for the NVML health source.
type NVMLSourceConfig struct {
	// DriverRoot is the path where NVIDIA driver libraries are located.
	DriverRoot string

	// IgnoredXids is a list of XID error codes to ignore.
	IgnoredXids []uint64

	// HealthCheckEnabled enables XID event monitoring.
	HealthCheckEnabled bool
}

// DefaultNVMLSourceConfig returns default NVML source configuration.
func DefaultNVMLSourceConfig() NVMLSourceConfig {
	return NVMLSourceConfig{
		DriverRoot:         "/run/nvidia/driver",
		IgnoredXids:        nil,
		HealthCheckEnabled: true,
	}
}

// NVMLSource implements HealthSource for NVML-based GPU monitoring.
type NVMLSource struct {
	config  NVMLSourceConfig
	logger  klog.Logger
	handler HealthEventHandler

	nvmllib  nvml.Interface
	eventSet nvml.EventSet

	mu             sync.Mutex
	initialized    bool
	running        bool
	gpuUUIDs       []string
	ignoredXidsMap map[uint64]bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewNVMLSource creates a new NVML health source.
func NewNVMLSource(cfg NVMLSourceConfig, logger klog.Logger) *NVMLSource {
	// Build ignored XIDs map
	ignoredMap := make(map[uint64]bool)
	for _, xid := range DefaultIgnoredXids {
		ignoredMap[xid] = true
	}
	for _, xid := range cfg.IgnoredXids {
		ignoredMap[xid] = true
	}

	return &NVMLSource{
		config:         cfg,
		logger:         logger.WithName("nvml-source"),
		ignoredXidsMap: ignoredMap,
	}
}

// Name returns the source identifier.
func (s *NVMLSource) Name() string {
	return NVMLSourceName
}

// Start initializes NVML and begins health monitoring.
func (s *NVMLSource) Start(ctx context.Context, handler HealthEventHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized {
		return fmt.Errorf("NVML source already started")
	}

	s.handler = handler
	s.ctx, s.cancel = context.WithCancel(ctx)

	// Initialize NVML
	if err := s.initNVML(); err != nil {
		return err
	}

	// Enumerate and register GPUs
	if err := s.enumerateGPUs(); err != nil {
		s.shutdownNVML()
		return err
	}

	// Start health monitoring if enabled and GPUs are present
	if s.config.HealthCheckEnabled && len(s.gpuUUIDs) > 0 {
		s.wg.Add(1)
		go s.runHealthMonitor()
	}

	s.initialized = true
	s.running = true
	s.logger.Info("NVML source started", "gpuCount", len(s.gpuUUIDs))

	return nil
}

// Stop shuts down the NVML source.
func (s *NVMLSource) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.initialized {
		return
	}

	s.logger.Info("Stopping NVML source")

	if s.cancel != nil {
		s.cancel()
	}

	s.wg.Wait()
	s.shutdownNVML()

	s.initialized = false
	s.running = false
	s.logger.Info("NVML source stopped")
}

// IsRunning returns true if the source is actively monitoring.
func (s *NVMLSource) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// initNVML initializes the NVML library.
func (s *NVMLSource) initNVML() error {
	libraryPath := s.findDriverLibrary()
	if libraryPath != "" {
		s.logger.V(2).Info("Using NVML library", "path", libraryPath)
		s.nvmllib = nvml.New(nvml.WithLibraryPath(libraryPath))
	} else {
		s.logger.V(2).Info("Using system default NVML library")
		s.nvmllib = nvml.New()
	}

	ret := s.nvmllib.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("NVML init failed: %v", nvml.ErrorString(ret))
	}

	if version, ret := s.nvmllib.SystemGetDriverVersion(); ret == nvml.SUCCESS {
		s.logger.Info("NVML initialized", "driverVersion", version)
	}

	return nil
}

// shutdownNVML shuts down the NVML library.
func (s *NVMLSource) shutdownNVML() {
	if s.eventSet != nil {
		s.eventSet.Free()
		s.eventSet = nil
	}

	if s.nvmllib != nil {
		s.nvmllib.Shutdown()
	}
}

// findDriverLibrary locates the NVML library.
func (s *NVMLSource) findDriverLibrary() string {
	if s.config.DriverRoot == "" {
		return ""
	}

	paths := []string{
		filepath.Join(s.config.DriverRoot, "usr/lib64/libnvidia-ml.so.1"),
		filepath.Join(s.config.DriverRoot, "usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"),
		filepath.Join(s.config.DriverRoot, "usr/lib/libnvidia-ml.so.1"),
		filepath.Join(s.config.DriverRoot, "lib64/libnvidia-ml.so.1"),
		filepath.Join(s.config.DriverRoot, "lib/libnvidia-ml.so.1"),
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// enumerateGPUs discovers GPUs and registers them with the handler.
func (s *NVMLSource) enumerateGPUs() error {
	count, ret := s.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	if count == 0 {
		s.logger.Info("No GPUs found on this node")
		return nil
	}

	s.logger.Info("Enumerating GPUs", "count", count)
	s.gpuUUIDs = make([]string, 0, count)

	for i := 0; i < count; i++ {
		device, ret := s.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			s.logger.Error(nil, "Failed to get device handle", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			s.logger.Error(nil, "Failed to get device UUID", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		productName, _ := device.GetName()
		var memoryBytes uint64
		if memInfo, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
			memoryBytes = memInfo.Total
		}

		gpu := &GPUInfo{
			UUID:        uuid,
			ProductName: productName,
			MemoryBytes: memoryBytes,
			Index:       i,
			Source:      NVMLSourceName,
		}

		if err := s.handler.OnGPUDiscovered(s.ctx, gpu); err != nil {
			s.logger.Error(err, "Failed to register GPU", "uuid", uuid)
			continue
		}

		s.gpuUUIDs = append(s.gpuUUIDs, uuid)
		s.logger.Info("GPU enumerated",
			"uuid", uuid,
			"productName", productName,
			"memory", formatBytes(memoryBytes),
		)
	}

	s.logger.Info("GPU enumeration complete", "registered", len(s.gpuUUIDs))
	return nil
}

// runHealthMonitor monitors NVML events for GPU health changes.
func (s *NVMLSource) runHealthMonitor() {
	defer s.wg.Done()

	// Create event set
	eventSet, ret := s.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		s.logger.Error(nil, "Failed to create event set", "error", nvml.ErrorString(ret))
		return
	}
	s.eventSet = eventSet

	// Register devices for XID events
	deviceCount, ret := s.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		s.logger.Error(nil, "Failed to get device count", "error", nvml.ErrorString(ret))
		return
	}

	registeredDevices := 0
	for i := 0; i < deviceCount; i++ {
		device, ret := s.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}

		// Check supported event types
		supportedTypes, ret := device.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			s.logger.V(2).Info("Cannot get supported event types", "index", i)
			continue
		}

		// Register for all supported error event types
		eventTypes := uint64(0)
		if supportedTypes&nvml.EventTypeXidCriticalError != 0 {
			eventTypes |= nvml.EventTypeXidCriticalError
		}
		if supportedTypes&nvml.EventTypeSingleBitEccError != 0 {
			eventTypes |= nvml.EventTypeSingleBitEccError
		}
		if supportedTypes&nvml.EventTypeDoubleBitEccError != 0 {
			eventTypes |= nvml.EventTypeDoubleBitEccError
		}

		if eventTypes == 0 {
			continue
		}

		ret = device.RegisterEvents(eventTypes, eventSet)
		if ret != nvml.SUCCESS {
			s.logger.V(1).Info("Failed to register events", "index", i, "error", nvml.ErrorString(ret))
			continue
		}
		registeredDevices++
	}

	s.logger.Info("Health monitor started", "registeredDevices", registeredDevices)

	// Event loop
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		data, ret := eventSet.Wait(EventTimeout)
		if ret == nvml.ERROR_TIMEOUT {
			continue
		}
		if ret != nvml.SUCCESS {
			s.logger.V(2).Info("Event wait error", "error", nvml.ErrorString(ret))
			continue
		}

		s.handleXIDEvent(data)
	}
}

// handleXIDEvent processes an XID error event.
func (s *NVMLSource) handleXIDEvent(data nvml.EventData) {
	uuid, ret := data.Device.GetUUID()
	if ret != nvml.SUCCESS {
		s.logger.Error(nil, "Failed to get device UUID from event")
		return
	}

	xid := data.EventData
	eventType := data.EventType

	s.logger.Info("XID event received",
		"uuid", uuid,
		"xid", xid,
		"eventType", eventType,
	)

	// Check if XID should be ignored
	if s.isIgnoredXid(xid) {
		s.logger.V(1).Info("Ignoring XID event", "uuid", uuid, "xid", xid)
		return
	}

	// Determine severity
	isFatal := IsCriticalXid(xid)
	isHealthy := !isFatal

	event := &HealthEvent{
		GPUUID:     uuid,
		Source:     NVMLSourceName,
		IsHealthy:  isHealthy,
		IsFatal:    isFatal,
		ErrorCodes: []string{fmt.Sprintf("%d", xid)},
		Reason:     xidToReason(xid),
		Message:    xidToMessage(xid),
		DetectedAt: time.Now(),
	}

	if err := s.handler.OnGPUHealthChanged(s.ctx, event); err != nil {
		s.logger.Error(err, "Failed to report health event", "uuid", uuid)
	}
}

// isIgnoredXid checks if an XID should be ignored.
func (s *NVMLSource) isIgnoredXid(xid uint64) bool {
	return s.ignoredXidsMap[xid]
}

// GPUUUIDs returns the list of discovered GPU UUIDs.
func (s *NVMLSource) GPUUUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, len(s.gpuUUIDs))
	copy(result, s.gpuUUIDs)
	return result
}

// XID classification utilities

// DefaultIgnoredXids are XID codes that are typically application errors.
var DefaultIgnoredXids = []uint64{
	13, // Graphics Engine Exception
	31, // GPU memory page fault
	43, // GPU stopped processing
	45, // Preemptive cleanup, due to previous errors
}

// CriticalXids are XID codes that indicate hardware failures.
var CriticalXids = map[uint64]bool{
	48:  true, // Double bit ECC error
	63:  true, // ECC page retirement or row remapping recording event
	64:  true, // ECC page retirement or row remapping threshold exceeded
	74:  true, // NVLink error
	79:  true, // GPU has fallen off the bus
	92:  true, // High single-bit ECC error rate
	94:  true, // Contained ECC error
	95:  true, // Uncontained ECC error
	119: true, // GSP error
	120: true, // GSP error
}

// IsCriticalXid returns true if the XID indicates a critical hardware failure.
func IsCriticalXid(xid uint64) bool {
	return CriticalXids[xid]
}

func xidToReason(xid uint64) string {
	if IsCriticalXid(xid) {
		return "CriticalXIDError"
	}
	return "XIDError"
}

func xidToMessage(xid uint64) string {
	descriptions := map[uint64]string{
		13:  "Graphics Engine Exception",
		31:  "GPU memory page fault",
		43:  "GPU stopped processing",
		45:  "Preemptive cleanup due to previous errors",
		48:  "Double bit ECC error",
		63:  "ECC page retirement recording event",
		64:  "ECC page retirement threshold exceeded",
		74:  "NVLink error",
		79:  "GPU has fallen off the bus",
		92:  "High single-bit ECC error rate",
		94:  "Contained ECC error",
		95:  "Uncontained ECC error",
		119: "GSP RPC timeout",
		120: "GSP error",
	}

	if desc, ok := descriptions[xid]; ok {
		return fmt.Sprintf("XID %d: %s", xid, desc)
	}
	return fmt.Sprintf("XID %d error", xid)
}

func formatBytes(bytes uint64) string {
	const GB = 1024 * 1024 * 1024
	const MB = 1024 * 1024
	const KB = 1024

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
