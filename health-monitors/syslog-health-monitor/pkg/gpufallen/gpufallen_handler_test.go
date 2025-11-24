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
		validateEvent func(t *testing.T, events *pb.HealthEvents, message string)
	}{
		{
			name: "GPU Fallen Error with PCI ID",
			message: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n" +
				"               NVRM: (PCI ID: 10de:26b5) installed in this system has\n" +
				"               NVRM: fallen off the bus and is not responding to commands.",
			expectEvent: true,
			validateEvent: func(t *testing.T, events *pb.HealthEvents, message string) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]
				assert.Equal(t, "test-agent", event.Agent)
				assert.Equal(t, "test-check", event.CheckName)
				assert.Equal(t, "GPU", event.ComponentClass)
				assert.True(t, event.IsFatal)
				assert.False(t, event.IsHealthy)
				assert.Equal(t, pb.RecommendedAction_RESTART_BM, event.RecommendedAction)
				require.Len(t, event.ErrorCode, 1)
				assert.Equal(t, "GPU_FALLEN_OFF_BUS", event.ErrorCode[0])
				assert.Equal(t, message, event.Message)

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
			validateEvent: func(t *testing.T, events *pb.HealthEvents, message string) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)

				event := events.Events[0]
				assert.Equal(t, message, event.Message)

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
			defer handler.Close()

			events, err := handler.ProcessLine(tc.message)
			require.NoError(t, err)

			if tc.expectEvent {
				require.NotNil(t, events, "Expected an event to be generated")
				if tc.validateEvent != nil {
					tc.validateEvent(t, events, tc.message)
				}
			} else {
				assert.Nil(t, events, "Expected no event to be generated")
			}
		})
	}
}

func TestXIDTracking(t *testing.T) {
	t.Run("XID then GPU fallen off - should suppress event", func(t *testing.T) {
		// Create fresh handler for this test
		handler, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)
		defer handler.Close() // Cleanup goroutine to prevent leaks

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
		defer handler2.Close()

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
		defer handler3.Close()

		handler3.SetXIDWindow(100 * time.Millisecond) // Very short window for testing

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
		defer handler4.Close()

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
		defer handler5.Close()

		// Message contains both XID and "fallen off the bus"
		combinedMsg := "NVRM: Xid (PCI:0000:b3:00.0): 79, GPU has fallen off the bus and is not responding to commands."
		events, err := handler5.ProcessLine(combinedMsg)
		require.NoError(t, err)
		assert.Nil(t, events, "Should not generate event when XID is in same message - let XID handler process it")
	})

	t.Run("Malformed XID code should not crash and GPU fallen off should still generate event", func(t *testing.T) {
		handler6, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)
		defer handler6.Close()

		// Process message with malformed XID code (not a number)
		// This should log a warning but not panic or corrupt state
		malformedXIDMsg := "NVRM: Xid (PCI:0000:b3:00.0): INVALID, pid=1234, name=process"
		events, err := handler6.ProcessLine(malformedXIDMsg)
		require.NoError(t, err)
		assert.Nil(t, events, "Should not generate event for malformed XID")

		// Now process GPU fallen off message - should generate event since malformed XID wasn't recorded
		fallenMsg := "NVRM: The NVIDIA GPU 0000:b3:00.0 fallen off the bus and is not responding to commands."
		events, err = handler6.ProcessLine(fallenMsg)
		require.NoError(t, err)
		require.NotNil(t, events, "Should generate event since malformed XID wasn't recorded")
		assert.Len(t, events.Events, 1)
	})

	t.Run("Expired entries are cleaned up from map", func(t *testing.T) {
		handler7, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)
		defer handler7.Close()

		// Set a very short duration for testing
		handler7.SetXIDWindow(10 * time.Millisecond)

		// Process XID message
		xidMsg := "NVRM: Xid (PCI:0000:b3:00.0): 79, pid=1234, name=process"
		events, err := handler7.ProcessLine(xidMsg)
		require.NoError(t, err)
		assert.Nil(t, events)

		// Verify entry exists in map
		handler7.mu.RLock()
		_, exists := handler7.recentXIDs["0000:b3:00.0"]
		handler7.mu.RUnlock()
		assert.True(t, exists, "XID should be recorded in map")

		// Wait for expiration
		time.Sleep(15 * time.Millisecond)

		// Check for recent XID - this should trigger cleanup
		hasRecent := handler7.hasRecentXID("0000:b3:00.0")
		assert.False(t, hasRecent, "XID should be expired")

		// Verify entry was removed from map
		handler7.mu.RLock()
		_, exists = handler7.recentXIDs["0000:b3:00.0"]
		handler7.mu.RUnlock()
		assert.False(t, exists, "Expired XID should be cleaned up from map")
	})

	t.Run("Background cleanup removes expired entries", func(t *testing.T) {
		handler8, err := NewGPUFallenHandler(
			"test-node",
			"test-agent",
			"GPU",
			"test-check",
		)
		require.NoError(t, err)
		defer handler8.Close()

		// Set very short window for testing
		handler8.SetXIDWindow(50 * time.Millisecond)

		// Process XID messages for multiple PCIs
		xidMsg1 := "NVRM: Xid (PCI:0000:b3:00.0): 79"
		xidMsg2 := "NVRM: Xid (PCI:0000:b4:00.0): 79"
		xidMsg3 := "NVRM: Xid (PCI:0000:b5:00.0): 79"

		_, _ = handler8.ProcessLine(xidMsg1)
		_, _ = handler8.ProcessLine(xidMsg2)
		_, _ = handler8.ProcessLine(xidMsg3)

		// Verify all entries exist
		handler8.mu.RLock()
		initialCount := len(handler8.recentXIDs)
		handler8.mu.RUnlock()
		assert.Equal(t, 3, initialCount, "Should have 3 XID entries")

		// Wait for expiration + background cleanup to run (cleanup runs every 1 minute by default)
		// For testing, we wait for expiration and rely on opportunistic cleanup via hasRecentXID
		time.Sleep(100 * time.Millisecond)

		// Trigger opportunistic cleanup by checking each PCI
		handler8.hasRecentXID("0000:b3:00.0")
		handler8.hasRecentXID("0000:b4:00.0")
		handler8.hasRecentXID("0000:b5:00.0")

		// Verify entries were cleaned up
		handler8.mu.RLock()
		finalCount := len(handler8.recentXIDs)
		handler8.mu.RUnlock()
		assert.Equal(t, 0, finalCount, "Expired entries should be cleaned up")
	})
}
