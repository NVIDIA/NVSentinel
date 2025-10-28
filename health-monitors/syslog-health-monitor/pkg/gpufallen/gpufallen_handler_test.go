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
		expectEvent   bool
		validateEvent func(t *testing.T, events *pb.HealthEvents)
	}{
		{
			name: "GPU Fallen Error with PCI ID",
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

				// Should have PCI and PCI_ID entities only
				assert.Len(t, event.EntitiesImpacted, 2)

				// Find entities by type rather than assuming order
				var hasPCI, hasPCIID bool
				for _, entity := range event.EntitiesImpacted {
					switch entity.EntityType {
					case "PCI":
						hasPCI = true
						assert.Equal(t, "0000:b3:00.0", entity.EntityValue)
					case "PCI_ID":
						hasPCIID = true
						assert.Equal(t, "10de:26b5", entity.EntityValue)
					}
				}
				assert.True(t, hasPCI, "Should have PCI entity")
				assert.True(t, hasPCIID, "Should have PCI_ID entity")
			},
		},
		{
			name: "GPU Fallen Error without PCI ID",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]

				// Should have only PCI entity
				assert.Len(t, event.EntitiesImpacted, 1)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "0000:b3:00.0", event.EntitiesImpacted[0].EntityValue)
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
