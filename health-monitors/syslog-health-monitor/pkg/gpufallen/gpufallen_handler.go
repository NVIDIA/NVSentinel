// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gpufallen

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewGPUFallenHandler creates a new GPUFallenHandler instance.
func NewGPUFallenHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName string) (*GPUFallenHandler, error) {
	ctx, cancel := context.WithCancel(context.Background())

	h := &GPUFallenHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
		recentXIDs:            make(map[string]xidRecord),
		xidWindow:             5 * time.Minute, // Remember XIDs for 5 minutes
		cancelCleanup:         cancel,
	}

	// Start background cleanup goroutine to prevent unbounded memory growth
	go h.cleanupExpiredXIDs(ctx)

	return h, nil
}

// Close stops the background cleanup goroutine and releases resources.
// Should be called when the handler is no longer needed (e.g., in tests or shutdown).
func (h *GPUFallenHandler) Close() {
	if h.cancelCleanup != nil {
		h.cancelCleanup()
	}
}

// SetXIDWindow sets the time window for tracking XID errors.
// This is primarily used for testing with shorter time windows.
func (h *GPUFallenHandler) SetXIDWindow(window time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.xidWindow = window
}

// ProcessLine processes a single syslog line and returns any generated health events.
func (h *GPUFallenHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	// First check if this is an XID message and track it
	h.trackXIDIfPresent(message)

	// Check if this is a GPU falling off error
	event := h.parseGPUFallenError(message)
	if event == nil {
		return nil, nil
	}

	return h.createHealthEventFromError(event), nil
}

// trackXIDIfPresent checks if the message contains an XID error and records it
func (h *GPUFallenHandler) trackXIDIfPresent(message string) {
	matches := common.XIDPattern.FindStringSubmatch(message)
	if len(matches) < 3 {
		return // Not an XID message
	}

	pciAddr := matches[1]
	xidCode := 0
	// Parse XID code (matches[2])
	if _, err := fmt.Sscanf(matches[2], "%d", &xidCode); err != nil {
		slog.Warn("Failed to parse XID code from message",
			"pci_address", pciAddr,
			"xid_string", matches[2],
			"error", err)

		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.recentXIDs[pciAddr] = xidRecord{
		timestamp: time.Now(),
		xidCode:   xidCode,
	}
}

// hasRecentXID checks if a PCI address has had an XID error within the time window
// and opportunistically cleans up expired entries to prevent memory leaks
func (h *GPUFallenHandler) hasRecentXID(pciAddr string) bool {
	h.mu.RLock()
	record, exists := h.recentXIDs[pciAddr]
	h.mu.RUnlock()

	if !exists {
		return false
	}

	// Check if the XID is still within the time window
	isRecent := time.Since(record.timestamp) < h.xidWindow

	// If expired, remove it from the map to prevent memory leaks
	if !isRecent {
		h.mu.Lock()
		// Double-check after acquiring write lock (entry might have been updated)
		if record, exists := h.recentXIDs[pciAddr]; exists && time.Since(record.timestamp) >= h.xidWindow {
			delete(h.recentXIDs, pciAddr)
		}
		h.mu.Unlock()
	}

	return isRecent
}

// cleanupExpiredXIDs runs periodically to remove expired XID entries.
// This prevents unbounded memory growth from XIDs on GPUs that never experience fallen-off errors.
// The goroutine stops when the context is cancelled (via Close method).
func (h *GPUFallenHandler) cleanupExpiredXIDs(ctx context.Context) {
	// Calculate cleanup interval proportional to xidWindow (xidWindow / 5)
	// This ensures tests with short windows don't wait too long for cleanup
	h.mu.RLock()
	cleanupInterval := h.xidWindow / 5
	h.mu.RUnlock()

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled, stop cleanup goroutine
			return
		case <-ticker.C:
			h.mu.Lock()
			now := time.Now()
			window := h.xidWindow // Read current window value

			for pciAddr, record := range h.recentXIDs {
				if now.Sub(record.timestamp) >= window {
					delete(h.recentXIDs, pciAddr)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *GPUFallenHandler) parseGPUFallenError(message string) *gpuFallenErrorEvent {
	// First check if this message itself contains "Xid" in same message
	// If it has XID error it should be handled by XID handler
	if common.XIDPattern.MatchString(message) {
		return nil
	}

	m := reGPUFallenPattern.FindStringSubmatch(message)
	if len(m) < 2 {
		return nil
	}

	pciAddr := m[1]

	// Check if this PCI address has had a recent XID error
	// If so, skip generating an event to avoid duplicates with XID handler
	if h.hasRecentXID(pciAddr) {
		return nil
	}

	// Try to extract PCI ID if present in the message
	pciID := ""
	pciIDMatch := rePCIIDPattern.FindStringSubmatch(message)

	if len(pciIDMatch) >= 2 {
		pciID = pciIDMatch[1]
	}

	return &gpuFallenErrorEvent{
		pciAddr: pciAddr,
		pciID:   pciID,
		message: message,
	}
}

func (h *GPUFallenHandler) createHealthEventFromError(event *gpuFallenErrorEvent) *pb.HealthEvents {
	entitiesImpacted := []*pb.Entity{
		{EntityType: "PCI", EntityValue: event.pciAddr},
	}

	// If PCI ID is there, add it as well
	if event.pciID != "" {
		entitiesImpacted = append(entitiesImpacted, &pb.Entity{
			EntityType: "PCI_ID", EntityValue: event.pciID,
		})
	}

	// Increment metrics (node-level only to avoid cardinality explosion)
	gpuFallenCounterMetric.WithLabelValues(h.nodeName).Inc()

	healthEvent := &pb.HealthEvent{
		Version:            1,
		Agent:              h.defaultAgentName,
		CheckName:          h.checkName,
		ComponentClass:     h.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entitiesImpacted,
		Message:            event.message,
		IsFatal:            true, // GPU falling off the bus is always fatal
		IsHealthy:          false,
		NodeName:           h.nodeName,
		RecommendedAction:  pb.RecommendedAction_RESTART_BM,
		ErrorCode:          []string{"GPU_FALLEN_OFF_BUS"},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{healthEvent},
	}
}
