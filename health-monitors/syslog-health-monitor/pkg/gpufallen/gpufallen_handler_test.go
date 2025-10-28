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
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNVRMGPUMapLine(t *testing.T) {
	handler := &GPUFallenHandler{
		pciToGPUUUID: make(map[string]string),
	}

	testCases := []struct {
		name      string
		line      string
		expectPCI string
		expectGPU string
	}{
		{
			name:      "Valid GPU Map Line",
			line:      "NVRM: GPU at PCI:0000:b3:00.0: GPU-12345678-1234-1234-1234-123456789012",
			expectPCI: "0000:b3:00.0",
			expectGPU: "GPU-12345678-1234-1234-1234-123456789012",
		},
		{
			name:      "Invalid Line",
			line:      "Some other log message",
			expectPCI: "",
			expectGPU: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pciID, gpuUUID := handler.parseNVRMGPUMapLine(tc.line)
			assert.Equal(t, tc.expectPCI, pciID)
			assert.Equal(t, tc.expectGPU, gpuUUID)
		})
	}
}

func TestNormalizePCI(t *testing.T) {
	handler := &GPUFallenHandler{}

	testCases := []struct {
		name     string
		pci      string
		expected string
	}{
		{
			name:     "PCI with function",
			pci:      "0000:b3:00.0",
			expected: "0000:b3:00",
		},
		{
			name:     "PCI without function",
			pci:      "0000:b3:00",
			expected: "0000:b3:00",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := handler.normalizePCI(tc.pci)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestParseGPUFallenError(t *testing.T) {
	handler := &GPUFallenHandler{}

	testCases := []struct {
		name        string
		message     string
		expectEvent bool
		expectPCI   string
		expectPCIID string
	}{
		{
			name: "Complete GPU Fallen Error with PCI ID",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: true,
			expectPCI:   "0000:b3:00.0",
			expectPCIID: "10de:26b5",
		},
		{
			name: "GPU Fallen Error without PCI ID",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: true,
			expectPCI:   "0000:b3:00.0",
			expectPCIID: "",
		},
		{
			name: "GPU Fallen Error with XID following - Should NOT match",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.\n" +
				"NVRM: Xid (PCI:0000:b3:00.0): 79, pid=12345, GPU has fallen off the bus.",
			expectEvent: false,
		},
		{
			name: "GPU Fallen Error with Xid in middle - Should NOT match",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: Xid (PCI:0000:b3:00.0): 79\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: false,
		},
		{
			name:        "Non-matching message",
			message:     "Some other NVRM message",
			expectEvent: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			event := handler.parseGPUFallenError(tc.message)
			if tc.expectEvent {
				require.NotNil(t, event, "Expected to parse an event")
				assert.Equal(t, tc.expectPCI, event.pciAddr)
				assert.Equal(t, tc.expectPCIID, event.pciID)
				assert.Equal(t, tc.message, event.message)
			} else {
				assert.Nil(t, event, "Expected no event to be parsed")
			}
		})
	}
}

func TestProcessLine(t *testing.T) {
	testCases := []struct {
		name          string
		message       string
		priorMapping  map[string]string
		expectEvent   bool
		validateEvent func(t *testing.T, events *pb.HealthEvents)
	}{
		{
			name:        "GPU Map Line - Should not generate event",
			message:     "NVRM: GPU at PCI:0000:b3:00.0: GPU-12345678-1234-1234-1234-123456789012",
			expectEvent: false,
		},
		{
			name: "GPU Fallen Error - Without GPU UUID",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]
				assert.Equal(t, "test-agent", event.Agent)
				assert.Equal(t, "test-check", event.CheckName)
				assert.Equal(t, "GPU", event.ComponentClass)
				assert.True(t, event.IsFatal)
				assert.False(t, event.IsHealthy)
				assert.Equal(t, pb.RecommendedAction_RESTART_BM, event.RecommendedAction)
				assert.Contains(t, event.ErrorCode, "GPU_FALLEN_OFF_BUS")

				// Should have PCI and PCI_ID entities, but no GPU UUID
				assert.Len(t, event.EntitiesImpacted, 2)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "0000:b3:00.0", event.EntitiesImpacted[0].EntityValue)
				assert.Equal(t, "PCI_ID", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "10de:26b5", event.EntitiesImpacted[1].EntityValue)
			},
		},
		{
			name: "GPU Fallen Error - With GPU UUID from prior mapping",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			priorMapping: map[string]string{
				"0000:b3:00": "GPU-12345678-1234-1234-1234-123456789012",
			},
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]

				// Should have PCI, PCI_ID, and GPU entities
				assert.Len(t, event.EntitiesImpacted, 3)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "PCI_ID", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU", event.EntitiesImpacted[2].EntityType)
				assert.Equal(t, "GPU-12345678-1234-1234-1234-123456789012", event.EntitiesImpacted[2].EntityValue)
			},
		},
		{
			name:        "Non-matching message",
			message:     "Some other log message",
			expectEvent: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewGPUFallenHandler(
				"test-node",
				"test-agent",
				"GPU",
				"test-check",
			)
			require.NoError(t, err)

			// Set up prior mappings if any
			if tc.priorMapping != nil {
				handler.pciToGPUUUID = tc.priorMapping
			}

			events, err := handler.ProcessLine(tc.message)
			require.NoError(t, err)

			if tc.expectEvent {
				require.NotNil(t, events, "Expected an event to be generated")
				if tc.validateEvent != nil {
					tc.validateEvent(t, events)
				}
			} else {
				assert.Nil(t, events, "Expected no event to be generated")
			}
		})
	}
}

func TestProcessLine_BuildsGPUMapping(t *testing.T) {
	handler, err := NewGPUFallenHandler(
		"test-node",
		"test-agent",
		"GPU",
		"test-check",
	)
	require.NoError(t, err)

	// Process a GPU mapping line first
	mapLine := "NVRM: GPU at PCI:0000:b3:00.0: GPU-12345678-1234-1234-1234-123456789012"
	events, err := handler.ProcessLine(mapLine)
	require.NoError(t, err)
	assert.Nil(t, events)

	// Verify the mapping was stored
	assert.Equal(t, "GPU-12345678-1234-1234-1234-123456789012", handler.pciToGPUUUID["0000:b3:00"])

	// Now process a GPU fallen error
	fallenLine := "NVRM: The NVIDIA GPU 0000:b3:00.0 fallen off the bus and is not responding to commands."
	events, err = handler.ProcessLine(fallenLine)
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events, 1)

	// Verify the event includes the GPU UUID
	event := events.Events[0]
	hasGPUEntity := false
	for _, entity := range event.EntitiesImpacted {
		if entity.EntityType == "GPU" && entity.EntityValue == "GPU-12345678-1234-1234-1234-123456789012" {
			hasGPUEntity = true
			break
		}
	}
	assert.True(t, hasGPUEntity, "Event should include GPU UUID entity")
}
