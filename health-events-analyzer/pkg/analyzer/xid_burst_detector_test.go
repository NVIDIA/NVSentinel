// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package analyzer

import (
	"testing"
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Helper function to create test XID events
func createXidEvent(nodeName, xidCode string, timestamp time.Time) *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName:  nodeName,
		ErrorCode: []string{xidCode},
		GeneratedTimestamp: &timestamppb.Timestamp{
			Seconds: timestamp.Unix(),
		},
		ComponentClass: "GPU",
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
}

func TestXidBurstDetector_SingleBurst_NoTrigger(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "79"

	// Simulate 3 XID errors within 1 minute (single burst)
	events := []*protos.HealthEvent{
		createXidEvent(nodeName, xidCode, baseTime),
		createXidEvent(nodeName, xidCode, baseTime.Add(30*time.Second)),
		createXidEvent(nodeName, xidCode, baseTime.Add(60*time.Second)),
	}

	var shouldTrigger bool
	for _, event := range events {
		shouldTrigger, _ = detector.ProcessEvent(event)
	}

	assert.False(t, shouldTrigger, "Single burst should not trigger")
}

func TestXidBurstDetector_FourBursts_NoTrigger(t *testing.T) {
	// MongoDB requires 5+ bursts to trigger, so 4 bursts should not trigger
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Create 4 bursts with 4-minute gaps (> 3 minute burst window)
	var shouldTrigger bool
	var burstCount int

	for burst := 0; burst < 4; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		events := []*protos.HealthEvent{
			createXidEvent(nodeName, xidCode, burstStart),
			createXidEvent(nodeName, xidCode, burstStart.Add(30*time.Second)),
		}
		for _, event := range events {
			shouldTrigger, burstCount = detector.ProcessEvent(event)
		}
	}

	assert.False(t, shouldTrigger, "4 bursts should not trigger (need 5+)")
	assert.Equal(t, 4, burstCount, "Should detect 4 bursts")
}

func TestXidBurstDetector_FiveBursts_Trigger(t *testing.T) {
	// MongoDB requires 5+ bursts to trigger
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Create 5 bursts with 4-minute gaps (> 3 minute burst window)
	var shouldTrigger bool
	var burstCount int

	for burst := 0; burst < 5; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		events := []*protos.HealthEvent{
			createXidEvent(nodeName, xidCode, burstStart),
			createXidEvent(nodeName, xidCode, burstStart.Add(30*time.Second)),
		}
		for _, event := range events {
			shouldTrigger, burstCount = detector.ProcessEvent(event)
		}
	}

	assert.True(t, shouldTrigger, "5 bursts should trigger")
	assert.Equal(t, 5, burstCount, "Should detect 5 bursts")
}

func TestXidBurstDetector_StickyXid_ExtendsBurst(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"

	// Sticky XIDs (74, 79, 95, 109, 119) within 3 hours of each other extend bursts
	// Create events with 2-hour gaps - sticky XIDs should keep them in same burst
	events := []*protos.HealthEvent{
		createXidEvent(nodeName, "79", baseTime),                       // Sticky
		createXidEvent(nodeName, "79", baseTime.Add(2*time.Hour)),      // Sticky, within 3h
		createXidEvent(nodeName, "120", baseTime.Add(2*time.Hour+30*time.Second)), // Non-sticky, continues burst
	}

	var burstCount int
	for _, event := range events {
		_, burstCount = detector.ProcessEvent(event)
	}

	// All events should be in same burst due to sticky XID continuation
	assert.Equal(t, 1, burstCount, "Sticky XIDs within 3h should keep events in same burst")
}

func TestXidBurstDetector_DifferentXids_NoTrigger(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"

	// Create 5 bursts but with different XIDs in each - should not trigger for any single XID
	xids := []string{"79", "120", "48", "31", "13"}
	var shouldTrigger bool

	for i, xid := range xids {
		burstStart := baseTime.Add(time.Duration(i) * 5 * time.Minute)
		events := []*protos.HealthEvent{
			createXidEvent(nodeName, xid, burstStart),
			createXidEvent(nodeName, xid, burstStart.Add(30*time.Second)),
		}
		for _, event := range events {
			shouldTrigger, _ = detector.ProcessEvent(event)
		}
	}

	assert.False(t, shouldTrigger, "Different XIDs in different bursts should not trigger")
}

func TestXidBurstDetector_MultipleNodes_Independent(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	xidCode := "120" // Non-sticky XID

	// Node 1: 4 bursts (not enough to trigger)
	for burst := 0; burst < 4; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		detector.ProcessEvent(createXidEvent("node-1", xidCode, burstStart))
		detector.ProcessEvent(createXidEvent("node-1", xidCode, burstStart.Add(30*time.Second)))
	}

	// Node 2: 5 bursts (should trigger)
	var shouldTrigger bool
	var burstCount int
	for burst := 0; burst < 5; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		detector.ProcessEvent(createXidEvent("node-2", xidCode, burstStart))
		shouldTrigger, burstCount = detector.ProcessEvent(createXidEvent("node-2", xidCode, burstStart.Add(30*time.Second)))
	}

	assert.True(t, shouldTrigger, "Node-2 should trigger with 5 bursts")
	assert.Equal(t, 5, burstCount, "Node-2 should have 5 bursts")

	// Node 1: Add one more event - this creates burst 5 for node-1, so it should trigger
	// The event at 25 minutes creates a new burst (gap > 3 min from burst 4 at 15-15.5 min)
	node1Event := createXidEvent("node-1", xidCode, baseTime.Add(25*time.Minute))
	shouldTrigger, burstCount = detector.ProcessEvent(node1Event)
	assert.True(t, shouldTrigger, "Node-1 should trigger with 5 bursts now")
	assert.Equal(t, 5, burstCount, "Node-1 should have 5 bursts")
}

func TestXidBurstDetector_CleanupOldEvents(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "79"

	// Add event from 25 hours ago (should be cleaned up - lookback is 24h)
	oldEvent := createXidEvent(nodeName, xidCode, baseTime.Add(-25*time.Hour))
	detector.ProcessEvent(oldEvent)

	// Add recent event
	recentEvent := createXidEvent(nodeName, xidCode, baseTime)
	detector.ProcessEvent(recentEvent)

	// Check that only recent event remains
	stats := detector.GetBurstStats()
	assert.Equal(t, 1, stats[nodeName], "Should only keep events within 24h lookback window")
}

func TestXidBurstDetector_NoEvents_NoTrigger(t *testing.T) {
	detector := NewXidBurstDetector()

	// No events processed
	stats := detector.GetBurstStats()
	assert.Empty(t, stats, "No events should result in empty stats")
}

func TestXidBurstDetector_EmptyErrorCode_NoTrigger(t *testing.T) {
	detector := NewXidBurstDetector()

	event := &protos.HealthEvent{
		NodeName:           "test-node",
		ErrorCode:          []string{}, // Empty error code
		GeneratedTimestamp: timestamppb.Now(),
		ComponentClass:     "GPU",
		IsHealthy:          false,
	}

	shouldTrigger, burstCount := detector.ProcessEvent(event)
	assert.False(t, shouldTrigger, "Empty error code should not trigger")
	assert.Equal(t, 0, burstCount)
}

func TestXidBurstDetector_ClearNodeHistory(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Create 5 bursts to trigger
	for burst := 0; burst < 5; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart))
		detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart.Add(30*time.Second)))
	}

	// Verify we have events in history
	stats := detector.GetBurstStats()
	assert.Equal(t, 10, stats[nodeName], "Node should have 10 events in history")

	// Clear node history (simulating healthy event received)
	detector.ClearNodeHistory(nodeName)

	// Verify node history is cleared
	stats = detector.GetBurstStats()
	assert.Equal(t, 0, stats[nodeName], "Node should have no events after clear")

	// Create new bursts after clearing - should not trigger until 5 bursts again
	var shouldTrigger bool
	for burst := 0; burst < 4; burst++ {
		burstStart := baseTime.Add(time.Duration(burst+5) * 5 * time.Minute)
		detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart))
		shouldTrigger, _ = detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart.Add(30*time.Second)))
	}

	assert.False(t, shouldTrigger, "Should not trigger after history clear - only 4 bursts exist")

	// 5th burst should trigger
	burstStart := baseTime.Add(9 * 5 * time.Minute)
	detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart))
	shouldTrigger, _ = detector.ProcessEvent(createXidEvent(nodeName, xidCode, burstStart.Add(30*time.Second)))

	assert.True(t, shouldTrigger, "Should trigger after 5 new bursts")
}

func TestXidBurstDetector_ClearNodeHistory_NonExistentNode(t *testing.T) {
	detector := NewXidBurstDetector()

	// Clear history for a node that doesn't exist - should not panic
	detector.ClearNodeHistory("non-existent-node")

	stats := detector.GetBurstStats()
	assert.Equal(t, 0, len(stats), "Stats should be empty")
}

func TestXidBurstDetector_BurstWindowThreeMinutes(t *testing.T) {
	// Verify that events within 3 minutes are in the same burst
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Create events with 2.5 minute gaps - should all be in same burst
	events := []*protos.HealthEvent{
		createXidEvent(nodeName, xidCode, baseTime),
		createXidEvent(nodeName, xidCode, baseTime.Add(150*time.Second)), // 2.5 min
		createXidEvent(nodeName, xidCode, baseTime.Add(300*time.Second)), // 5 min total (2.5 min gap)
	}

	var burstCount int
	for _, event := range events {
		_, burstCount = detector.ProcessEvent(event)
	}

	assert.Equal(t, 1, burstCount, "Events within 3 minute gaps should be in same burst")
}

func TestXidBurstDetector_BurstWindowExceeded(t *testing.T) {
	// Verify that events with > 3 minute gaps create new bursts
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Create events with 4 minute gaps - should create separate bursts
	events := []*protos.HealthEvent{
		createXidEvent(nodeName, xidCode, baseTime),
		createXidEvent(nodeName, xidCode, baseTime.Add(4*time.Minute)), // New burst
		createXidEvent(nodeName, xidCode, baseTime.Add(8*time.Minute)), // New burst
	}

	var burstCount int
	for _, event := range events {
		_, burstCount = detector.ProcessEvent(event)
	}

	assert.Equal(t, 3, burstCount, "Events with >3 minute gaps should create separate bursts")
}
