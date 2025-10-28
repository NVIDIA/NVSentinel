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
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// NewGPUFallenHandler creates a new GPUFallenHandler instance.
func NewGPUFallenHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName string) (*GPUFallenHandler, error) {
	return &GPUFallenHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
	}, nil
}

// ProcessLine processes a single syslog line and returns any generated health events.
func (h *GPUFallenHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	// Check if this is a GPU falling off error
	event := h.parseGPUFallenError(message)
	if event == nil {
		return nil, nil
	}

	return h.createHealthEventFromError(event), nil
}

func (h *GPUFallenHandler) parseGPUFallenError(message string) *gpuFallenErrorEvent {
	// First check if this message contains "Xid"
	// If it has XID error it should be handled by XID handler
	if common.XIDPattern.MatchString(message) {
		return nil
	}

	m := reGPUFallenPattern.FindStringSubmatch(message)
	if len(m) < 2 {
		return nil
	}

	pciAddr := m[1]

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

	// Increment metrics
	gpuFallenCounterMetric.WithLabelValues(h.nodeName, event.pciAddr).Inc()

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
		Metadata: map[string]string{
			"JOURNAL_MESSAGE": event.message,
			"PCI_ADDRESS":     event.pciAddr,
		},
	}

	if event.pciID != "" {
		healthEvent.Metadata["PCI_ID"] = event.pciID
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{healthEvent},
	}
}
