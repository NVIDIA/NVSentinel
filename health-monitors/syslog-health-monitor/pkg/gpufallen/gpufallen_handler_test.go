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
	"time"

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

func TestXIDTracking(t *testing.T) {
	handler, err := NewGPUFallenHandler(
		"test-node",
		"test-agent",
		"GPU",
		"test-check",
	)
	require.NoError(t, err)

	t.Run("XID then GPU fallen off - should suppress event", func(t *testing.T) {
		// Process XID message first
		xidMsg := "NVRM: Xid (PCI:0000:b3:00.0): 79, pid=1234, name=process"
		events, err := handler.ProcessLine(xidMsg)
		require.NoError(t, err)
		assert.Nil(t, events, "XID handler should process XID, not gpufallen handler")

		// Now process GPU fallen off message - should be suppressed
		fallenMsg := "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
			"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
			"               NVRM: fallen off the bus and is not responding to commands."
		events, err = handler.ProcessLine(fallenMsg)
		require.NoError(t, err)
		assert.Nil(t, events, "Should suppress GPU fallen off when recent XID exists")
	})

	t.Run("GPU fallen off without prior XID - should generate event", func(t *testing.T) {
		// Create fresh handler
		handler2, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)

		// Process GPU fallen off without any prior XID
		fallenMsg := "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
			"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
			"               NVRM: fallen off the bus and is not responding to commands."
		events, err := handler2.ProcessLine(fallenMsg)
		require.NoError(t, err)
		require.NotNil(t, events, "Should generate event when no recent XID")
		require.Len(t, events.Events, 1)
	})

	t.Run("XID expires after time window", func(t *testing.T) {
		// Create handler with short window for testing
		handler3, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)
		handler3.xidWindow = 100 * time.Millisecond // Very short window for testing

		// Process XID
		xidMsg := "NVRM: Xid (PCI:0000:b3:00.0): 79, pid=1234"
		_, err = handler3.ProcessLine(xidMsg)
		require.NoError(t, err)

		// Wait for XID to expire
		time.Sleep(150 * time.Millisecond)

		// Now GPU fallen off should generate event
		fallenMsg := "NVRM: The NVIDIA GPU 0000:b3:00.0 fallen off the bus and is not responding to commands."
		events, err := handler3.ProcessLine(fallenMsg)
		require.NoError(t, err)
		require.NotNil(t, events, "Should generate event after XID expires")
	})

	t.Run("Different PCI addresses tracked independently", func(t *testing.T) {
		handler4, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)

		// Process XID for GPU 1
		xidMsg1 := "NVRM: Xid (PCI:0000:b3:00.0): 79"
		_, err = handler4.ProcessLine(xidMsg1)
		require.NoError(t, err)

		// GPU fallen off for GPU 2 (different PCI) should still generate event
		fallenMsg2 := "NVRM: The NVIDIA GPU 0000:b4:00.0 fallen off the bus and is not responding to commands."
		events, err := handler4.ProcessLine(fallenMsg2)
		require.NoError(t, err)
		require.NotNil(t, events, "Should generate event for different PCI address")

		// But GPU fallen off for GPU 1 should be suppressed
		fallenMsg1 := "NVRM: The NVIDIA GPU 0000:b3:00.0 fallen off the bus and is not responding to commands."
		events, err = handler4.ProcessLine(fallenMsg1)
		require.NoError(t, err)
		assert.Nil(t, events, "Should suppress for PCI with recent XID")
	})

	t.Run("Single message with both XID and GPU fallen off - should not generate event", func(t *testing.T) {
		handler5, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)

		// Message contains both XID and "fallen off the bus"
		combinedMsg := "NVRM: Xid (PCI:0000:b3:00.0): 79, GPU has fallen off the bus and is not responding to commands."
		events, err := handler5.ProcessLine(combinedMsg)
		require.NoError(t, err)
		assert.Nil(t, events, "Should not generate event when XID is in same message - let XID handler process it")
	})
}
