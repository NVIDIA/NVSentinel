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

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
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

// Helper function to create test XID events for a specific GPU (by UUID or ordinal).
// Uses the production "GPU_UUID" entity type emitted by the syslog health monitor.
func createXidEventForGPU(nodeName, xidCode, gpuUUID string, timestamp time.Time) *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName:  nodeName,
		ErrorCode: []string{xidCode},
		GeneratedTimestamp: &timestamppb.Timestamp{
			Seconds: timestamp.Unix(),
		},
		ComponentClass: "GPU",
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU_UUID", EntityValue: gpuUUID},
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
		createXidEvent(nodeName, "79", baseTime),                                  // Sticky
		createXidEvent(nodeName, "79", baseTime.Add(2*time.Hour)),                 // Sticky, within 3h
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

// TestXidBurstDetector_DifferentGPUs_DoNotMerge verifies that bursts on
// different GPUs on the same node are tracked in separate histories and are
// not summed together when deciding whether to trigger.
//
// Scenario (non-sticky XID so burst boundaries are unambiguous):
//   - GPU-0 has 3 bursts in 24h
//   - GPU-3 has 2 bursts in 24h
//   - Neither GPU alone meets the default threshold of 5 bursts
//   - The detector must not trigger by summing 3 + 2 = 5.
func TestXidBurstDetector_DifferentGPUs_DoNotMerge(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky: 5-minute gaps reliably create new bursts.

	const gpu0 = "GPU-0"

	const gpu3 = "GPU-3"

	// GPU-0: 3 distinct bursts (below threshold of 5).
	for burst := 0; burst < 3; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, gpu0, burstStart))
		detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, gpu0,
			burstStart.Add(30*time.Second)))
	}

	// GPU-3: 2 distinct bursts interleaved in the same 24h window (below
	// threshold). Any event here must NOT be combined with GPU-0's history.
	var shouldTrigger bool

	var burstCount int

	for burst := 0; burst < 2; burst++ {
		burstStart := baseTime.Add(time.Duration(burst+3) * 5 * time.Minute)
		detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, gpu3, burstStart))
		shouldTrigger, burstCount = detector.ProcessEvent(
			createXidEventForGPU(nodeName, xidCode, gpu3, burstStart.Add(30*time.Second)))
	}

	assert.False(t, shouldTrigger, "bursts on different GPUs must not be combined")
	assert.Equal(t, 2, burstCount, "GPU-3 alone has only 2 bursts")

	// Per-GPU bookkeeping must hold each GPU's events separately.
	perGPU := detector.GetPerGPUBurstStats()
	nodeStats := perGPU[nodeName]
	assert.Equal(t, 2, len(nodeStats), "node should have one history bucket per GPU")
	assert.Equal(t, 6, nodeStats[gpu0], "GPU-0: 3 bursts x 2 events")
	assert.Equal(t, 4, nodeStats[gpu3], "GPU-3: 2 bursts x 2 events")

	stats := detector.GetBurstStats()
	assert.Equal(t, 10, stats[nodeName], "aggregate stats should cover all events")
}

// TestXidBurstDetector_SameGPU_TriggersAcrossBursts verifies that five bursts
// on the same GPU (identified by GPU_UUID, the production entity type) DO
// trigger, even while another GPU on the same node has its own independent
// event stream.
func TestXidBurstDetector_SameGPU_TriggersAcrossBursts(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "120" // Non-sticky XID

	// Noise on GPU-3: a few events on another GPU that must not leak into GPU-0.
	for i := 0; i < 4; i++ {
		detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, "GPU-3",
			baseTime.Add(time.Duration(i)*5*time.Minute)))
	}

	var shouldTrigger bool

	var burstCount int

	for burst := 0; burst < 5; burst++ {
		burstStart := baseTime.Add(time.Duration(burst) * 5 * time.Minute)
		detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, "GPU-0", burstStart))
		shouldTrigger, burstCount = detector.ProcessEvent(
			createXidEventForGPU(nodeName, xidCode, "GPU-0", burstStart.Add(30*time.Second)))
	}

	assert.True(t, shouldTrigger, "5 bursts on the same GPU must trigger")
	assert.Equal(t, 5, burstCount, "Reported burst count should reflect GPU-0's 5 bursts")
}

// TestXidBurstDetector_ClearNodeHistory_ClearsAllGPUs verifies that clearing a
// node's history wipes per-GPU histories for every GPU on that node.
func TestXidBurstDetector_ClearNodeHistory_ClearsAllGPUs(t *testing.T) {
	detector := NewXidBurstDetector()

	baseTime := time.Now()
	nodeName := "test-node-1"
	xidCode := "79"

	detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, "GPU-0", baseTime))
	detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, "GPU-1",
		baseTime.Add(1*time.Second)))
	detector.ProcessEvent(createXidEventForGPU(nodeName, xidCode, "GPU-2",
		baseTime.Add(2*time.Second)))

	stats := detector.GetBurstStats()
	assert.Equal(t, 3, stats[nodeName],
		"Stats should aggregate events across all GPUs on the node")

	detector.ClearNodeHistory(nodeName)

	stats = detector.GetBurstStats()
	assert.Equal(t, 0, stats[nodeName], "All per-GPU histories should be cleared")
}

// TestXidBurstDetector_MixedAndDuplicateEntities_CountedOnce verifies that a
// single event carrying mixed (GPU_UUID + GPU) or duplicate GPU_UUID entries
// for the same physical GPU is counted exactly once, in a single per-GPU
// bucket keyed by the GPU_UUID. Non-UUID GPU entries are ignored to avoid
// double-counting the same device under two keys.
func TestXidBurstDetector_MixedAndDuplicateEntities_CountedOnce(t *testing.T) {
	detector := NewXidBurstDetector()

	nodeName := "test-node-1"
	xidCode := "120"
	gpuUUID := "GPU-11111111-1111-1111-1111-111111111111"

	// Mirrors what the sxid handler emits: both a GPU ordinal and a GPU_UUID
	// for the same device, plus a duplicate GPU_UUID entry.
	event := &protos.HealthEvent{
		NodeName:           nodeName,
		ErrorCode:          []string{xidCode},
		GeneratedTimestamp: &timestamppb.Timestamp{Seconds: time.Now().Unix()},
		ComponentClass:     "GPU",
		IsHealthy:          false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU_UUID", EntityValue: gpuUUID},
			{EntityType: "GPU", EntityValue: "3"},
			{EntityType: "GPU_UUID", EntityValue: gpuUUID},
			{EntityType: "PCI", EntityValue: "0001:00:00"},
		},
	}

	detector.ProcessEvent(event)

	perGPU := detector.GetPerGPUBurstStats()
	nodeStats := perGPU[nodeName]
	assert.Equal(t, 1, len(nodeStats),
		"event must land in exactly one per-GPU bucket, not be split across GPU and GPU_UUID")
	assert.Equal(t, 1, nodeStats[gpuUUID],
		"event must be counted once under the GPU_UUID key")
	assert.Equal(t, 1, detector.GetBurstStats()[nodeName],
		"aggregate count should reflect the single event")
}
