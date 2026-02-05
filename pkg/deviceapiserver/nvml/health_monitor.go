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
)

// HealthMonitor monitors GPU health via NVML events.
type HealthMonitor struct {
	provider *Provider
}

// EventTimeout is the timeout for NVML event wait (in milliseconds).
const EventTimeout = 5000

// unknownUUID is used when UUID cannot be retrieved.
const unknownUUID = "unknown"

// startHealthMonitoring initializes and starts XID event monitoring.
func (p *Provider) startHealthMonitoring() error {
	// Create event set
	eventSet, ret := p.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %v", nvml.ErrorString(ret))
	}

	p.eventSet = eventSet

	// Register for health events on all GPUs
	eventMask := uint64(
		nvml.EventTypeXidCriticalError |
			nvml.EventTypeDoubleBitEccError |
			nvml.EventTypeSingleBitEccError,
	)

	count, _ := p.nvmllib.DeviceGetCount()
	registeredCount := 0

	for i := 0; i < count; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}

		uuid, _ := device.GetUUID()

		// Get supported events for this device
		supportedEvents, ret := device.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			p.logger.V(1).Info("Device does not support event queries",
				"index", i,
				"uuid", uuid,
				"error", nvml.ErrorString(ret),
			)

			continue
		}

		// Register only supported events
		eventsToRegister := eventMask & supportedEvents
		if eventsToRegister == 0 {
			p.logger.V(1).Info("Device does not support any health events",
				"index", i,
				"uuid", uuid,
			)

			continue
		}

		ret = device.RegisterEvents(eventsToRegister, p.eventSet.Raw())
		if ret == nvml.ERROR_NOT_SUPPORTED {
			p.logger.V(1).Info("Device too old for health monitoring",
				"index", i,
				"uuid", uuid,
			)

			continue
		}

		if ret != nvml.SUCCESS {
			p.logger.Error(nil, "Failed to register events",
				"index", i,
				"uuid", uuid,
				"error", nvml.ErrorString(ret),
			)

			continue
		}

		registeredCount++

		p.logger.V(2).Info("Registered health events",
			"index", i,
			"uuid", uuid,
			"events", eventsToRegister,
		)
	}

	if registeredCount == 0 {
		_ = p.eventSet.Free()
		p.eventSet = nil

		return fmt.Errorf("no devices support health event monitoring")
	}

	p.logger.Info("Starting health monitoring", "devices", registeredCount)

	// Create health monitor
	p.healthMonitor = &HealthMonitor{provider: p}

	// Start monitoring goroutine
	p.wg.Add(1)

	go p.runHealthMonitor()

	p.monitorRunning = true

	return nil
}

// runHealthMonitor is the main health monitoring loop.
//
// The loop checks for context cancellation before each iteration to ensure
// prompt shutdown when requested. The processEvents() call blocks for up to
// EventTimeout milliseconds waiting for NVML events.
func (p *Provider) runHealthMonitor() {
	defer p.wg.Done()

	p.logger.V(1).Info("Health monitor started")

	for {
		// Check for shutdown before processing events.
		// This ensures we respond promptly to cancellation rather than
		// waiting for the next event timeout cycle.
		select {
		case <-p.ctx.Done():
			p.logger.V(1).Info("Health monitor stopping")
			return
		default:
		}

		p.processEvents()
	}
}

// processEvents waits for and processes NVML events.
func (p *Provider) processEvents() {
	event, ret := p.eventSet.Wait(EventTimeout)

	if ret == nvml.ERROR_TIMEOUT {
		// Normal timeout, continue
		return
	}

	if ret != nvml.SUCCESS {
		if ret == nvml.ERROR_GPU_IS_LOST {
			p.logger.Error(nil, "GPU lost detected, marking all GPUs unhealthy")
			p.markAllUnhealthy("GPULost", "GPU is lost error detected")

			return
		}

		p.logger.V(2).Info("Error waiting for event",
			"error", nvml.ErrorString(ret),
		)

		// Brief sleep to avoid tight loop on persistent errors
		time.Sleep(100 * time.Millisecond)

		return
	}

	// Process the event
	p.handleEvent(event)
}

// handleEvent processes a single NVML event.
func (p *Provider) handleEvent(event nvml.EventData) {
	eventType := event.EventType
	xid := event.EventData
	gpuInstanceID := event.GpuInstanceId
	computeInstanceID := event.ComputeInstanceId

	// Get UUID for logging
	uuid := unknownUUID

	if event.Device != nil {
		if u, ret := event.Device.GetUUID(); ret == nvml.SUCCESS {
			uuid = u
		}
	}

	// Only process XID critical errors for health changes
	if eventType != nvml.EventTypeXidCriticalError {
		p.logger.V(2).Info("Non-critical event received",
			"uuid", uuid,
			"eventType", eventType,
			"xid", xid,
		)

		return
	}

	// Check if this XID should be ignored
	if isIgnoredXid(xid, p.config.AdditionalIgnoredXids) {
		p.logger.V(2).Info("Ignoring non-critical XID",
			"uuid", uuid,
			"xid", xid,
			"gpuInstanceId", gpuInstanceID,
			"computeInstanceId", computeInstanceID,
		)

		return
	}

	// Critical XID - mark GPU unhealthy
	p.logger.Info("Critical XID error detected",
		"uuid", uuid,
		"xid", xid,
		"xidName", xidToString(xid),
		"gpuInstanceId", gpuInstanceID,
		"computeInstanceId", computeInstanceID,
	)

	message := fmt.Sprintf("Critical XID error %d (%s) detected", xid, xidToString(xid))
	if err := p.UpdateCondition(uuid, ConditionTypeNVMLReady, ConditionStatusFalse, "XidError", message); err != nil {
		p.logger.Error(err, "Failed to update GPU condition", "uuid", uuid)
	}
}

// markAllUnhealthy marks all tracked GPUs as unhealthy.
func (p *Provider) markAllUnhealthy(reason, message string) {
	for _, uuid := range p.gpuUUIDs {
		err := p.UpdateCondition(uuid, ConditionTypeNVMLReady, ConditionStatusFalse, reason, message)
		if err != nil {
			p.logger.Error(err, "Failed to mark GPU unhealthy", "uuid", uuid)
		}
	}
}

// MarkHealthy marks a specific GPU as healthy.
//
// This can be called to restore a GPU's health status after recovery.
func (p *Provider) MarkHealthy(uuid string) error {
	return p.UpdateCondition(uuid, ConditionTypeNVMLReady, ConditionStatusTrue, "Healthy", "GPU is healthy")
}
