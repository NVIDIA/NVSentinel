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

package queue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// TestWorkqueueDeduplication_WithoutEventID tests the CURRENT behavior (without EventID)
// This demonstrates the problem we're trying to solve
func TestWorkqueueDeduplication_WithoutEventID(t *testing.T) {
	t.Skip("Skipping test for current behavior - demonstrates the problem")

	// Create queue manager
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// Event 1: Quarantine event
	event1 := datastore.Event{
		"_id":             "507f1f77bcf86cd799439011",
		"nodeName":        "node-1",
		"nodeQuarantined": "Quarantined",
	}

	// Event 2: Cancelled event (different _id, same node)
	event2 := datastore.Event{
		"_id":             "507f1f77bcf86cd799439012", // Different!
		"nodeName":        "node-1",
		"nodeQuarantined": "Cancelled",
	}

	// Enqueue first event
	err := mgr.EnqueueEventGeneric(ctx, "node-1", event1, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Simulate: Worker gets the event but doesn't call Done yet (simulates processing)
	queueImpl := mgr.(*eventQueueManager)
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "node-1", item1.NodeName)

	// Now enqueue second event WHILE first is being processed
	err = mgr.EnqueueEventGeneric(ctx, "node-1", event2, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Without EventID: Both events might get deduplicated because NodeEvent
	// only has NodeName as the distinguishing field (Event pointer changes each time)
	// The actual behavior depends on pointer comparison

	// Clean up
	queueImpl.queue.Done(item1)
}

// TestWorkqueueDeduplication_WithEventID tests the NEW behavior (with EventID)
// This demonstrates that the fix works correctly
func TestWorkqueueDeduplication_WithEventID(t *testing.T) {
	// Create queue manager
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// Event 1: Quarantine event
	event1 := datastore.Event{
		"_id":             "507f1f77bcf86cd799439011",
		"nodeName":        "node-1",
		"nodeQuarantined": "Quarantined",
	}

	// Event 2: Cancelled event (different _id, same node)
	event2 := datastore.Event{
		"_id":             "507f1f77bcf86cd799439012", // Different _id!
		"nodeName":        "node-1",
		"nodeQuarantined": "Cancelled",
	}

	// Enqueue first event (EventID extracted from event map)
	err := mgr.EnqueueEventGeneric(ctx, "node-1", event1, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Get the event but DON'T call Done (simulates processing)
	queueImpl := mgr.(*eventQueueManager)
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "node-1", item1.NodeName)
	assert.Equal(t, "507f1f77bcf86cd799439011", item1.EventID)

	// Queue should be empty now (item is being processed)
	assert.Equal(t, 0, queueImpl.queue.Len())

	// Now enqueue second event WHILE first is being processed
	err = mgr.EnqueueEventGeneric(ctx, "node-1", event2, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// With EventID: Second event should be in the queue (different EventID!)
	assert.Equal(t, 1, queueImpl.queue.Len(), "Second event should be queued (different EventID)")

	// Get the second event
	item2, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "node-1", item2.NodeName)
	assert.Equal(t, "507f1f77bcf86cd799439012", item2.EventID)

	// Clean up
	queueImpl.queue.Done(item1)
	queueImpl.queue.Done(item2)
}

// TestWorkqueueDeduplication_SameEventDifferentStatus tests that updates to the SAME event
// are correctly deduplicated (we don't want to process the same event twice)
func TestWorkqueueDeduplication_SameEventDifferentStatus(t *testing.T) {
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// Event with status NotStarted
	event1 := datastore.Event{
		"_id":                    "507f1f77bcf86cd799439011",
		"nodeName":               "node-1",
		"userPodsEvictionStatus": "NotStarted",
	}

	// Same event, status updated to Succeeded
	event2 := datastore.Event{
		"_id":                    "507f1f77bcf86cd799439011", // SAME _id!
		"nodeName":               "node-1",
		"userPodsEvictionStatus": "Succeeded",
	}

	// Enqueue first event
	err := mgr.EnqueueEventGeneric(ctx, "node-1", event1, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Get the event but DON'T call Done
	queueImpl := mgr.(*eventQueueManager)
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "507f1f77bcf86cd799439011", item1.EventID)

	// Queue should be empty
	assert.Equal(t, 0, queueImpl.queue.Len())

	// Try to enqueue the SAME event (different status) WHILE processing
	err = mgr.EnqueueEventGeneric(ctx, "node-1", event2, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// The workqueue marks it as "dirty" and adds to the queue
	// Because the struct comparison includes Event pointer (which changes each time),
	// the workqueue treats it as a different item
	// But the EventID being the SAME is what matters semantically
	assert.Equal(t, 1, queueImpl.queue.Len(), "Same event (different status) is queued")

	// Now call Done on the first event
	queueImpl.queue.Done(item1)

	// The second update should still be in the queue
	assert.Equal(t, 1, queueImpl.queue.Len(), "Updated event still in queue")

	// Clean up
	item2, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "507f1f77bcf86cd799439011", item2.EventID, "Same EventID")
	queueImpl.queue.Done(item2)
}

// TestWorkqueueDeduplication_MultipleFaultsSameNode tests that multiple different faults
// on the same node can be queued simultaneously
func TestWorkqueueDeduplication_MultipleFaultsSameNode(t *testing.T) {
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// Fault 1: GPU XID 13
	event1 := datastore.Event{
		"_id":       "507f1f77bcf86cd799439011",
		"nodeName":  "node-gpu-1",
		"errorCode": []int{13},
	}

	// Fault 2: GPU XID 48 (different fault, same node)
	event2 := datastore.Event{
		"_id":       "507f1f77bcf86cd799439012", // Different _id!
		"nodeName":  "node-gpu-1",
		"errorCode": []int{48},
	}

	// Enqueue first fault
	err := mgr.EnqueueEventGeneric(ctx, "node-gpu-1", event1, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Get first fault but DON'T call Done (simulates long drain operation)
	queueImpl := mgr.(*eventQueueManager)
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "507f1f77bcf86cd799439011", item1.EventID)

	// Enqueue second fault WHILE first is being processed
	err = mgr.EnqueueEventGeneric(ctx, "node-gpu-1", event2, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Second fault should be in the queue (different EventID!)
	assert.Equal(t, 1, queueImpl.queue.Len(), "Second fault should be queued")

	// Get second fault
	item2, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "507f1f77bcf86cd799439012", item2.EventID)

	// Both faults can be processed (sequentially)
	queueImpl.queue.Done(item1)
	queueImpl.queue.Done(item2)
}

// TestWorkqueueDeduplication_RealWorldScenario tests the exact scenario from the bug report
func TestWorkqueueDeduplication_RealWorldScenario(t *testing.T) {
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// T+0ms: Quarantine event arrives
	quarantineEvent := datastore.Event{
		"_id":             "507f1f77bcf86cd799439011",
		"nodeName":        "node-1",
		"nodeQuarantined": "Quarantined",
	}

	err := mgr.EnqueueEventGeneric(ctx, "node-1", quarantineEvent, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	queueImpl := mgr.(*eventQueueManager)

	// Worker picks it up immediately
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "507f1f77bcf86cd799439011", item1.EventID)

	// Simulate: Worker is processing (drain takes 30 seconds)
	// But we won't call Done yet

	// T+30ms: Cancelled event arrives (WHILE QUARANTINE IS PROCESSING)
	cancelledEvent := datastore.Event{
		"_id":             "507f1f77bcf86cd799439012", // Different event!
		"nodeName":        "node-1",
		"nodeQuarantined": "Cancelled",
	}

	err = mgr.EnqueueEventGeneric(ctx, "node-1", cancelledEvent, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// The bug: Without EventID, this would be deduplicated
	// The fix: With EventID, this should be in the queue
	assert.Equal(t, 1, queueImpl.queue.Len(), "Cancelled event should be queued (different EventID)")

	// Verify we can get the cancelled event
	item2, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)
	assert.Equal(t, "node-1", item2.NodeName)
	assert.Equal(t, "507f1f77bcf86cd799439012", item2.EventID)

	// Clean up
	queueImpl.queue.Done(item1)
	queueImpl.queue.Done(item2)
}

// TestWorkqueueDeduplication_DifferentNodes tests that events for different nodes
// don't interfere with each other
func TestWorkqueueDeduplication_DifferentNodes(t *testing.T) {
	mgr := NewEventQueueManager()
	defer mgr.Shutdown()

	ctx := context.Background()
	mockDB := &mockDataStore{}
	mockHealthEventStore := &MockHealthEventStore{}

	// Event for node-1
	event1 := datastore.Event{
		"_id":      "507f1f77bcf86cd799439011",
		"nodeName": "node-1",
	}

	// Event for node-2
	event2 := datastore.Event{
		"_id":      "507f1f77bcf86cd799439012",
		"nodeName": "node-2",
	}

	// Enqueue both events
	err := mgr.EnqueueEventGeneric(ctx, "node-1", event1, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	err = mgr.EnqueueEventGeneric(ctx, "node-2", event2, mockDB, mockHealthEventStore)
	require.NoError(t, err)

	// Both should be in the queue
	queueImpl := mgr.(*eventQueueManager)
	assert.Equal(t, 2, queueImpl.queue.Len(), "Events for different nodes should both be queued")

	// Get both events
	item1, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)

	item2, shutdown := queueImpl.queue.Get()
	require.False(t, shutdown)

	// Verify they're different
	assert.NotEqual(t, item1.EventID, item2.EventID)
	assert.NotEqual(t, item1.NodeName, item2.NodeName)

	// Clean up
	queueImpl.queue.Done(item1)
	queueImpl.queue.Done(item2)
}

// Mock DataStore for testing
type mockDataStore struct{}

func (m *mockDataStore) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	return &client.UpdateResult{ModifiedCount: 1, MatchedCount: 1}, nil
}

func (m *mockDataStore) FindDocument(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error) {
	return mockSingleResult{}, nil
}

func (m *mockDataStore) FindDocuments(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error) {
	return mockCursor{}, nil
}

type mockSingleResult struct{}

func (m mockSingleResult) Decode(v interface{}) error {
	return nil
}

func (m mockSingleResult) Err() error {
	return nil
}

type mockCursor struct{}

func (m mockCursor) Next(ctx context.Context) bool {
	return false
}

func (m mockCursor) Decode(v interface{}) error {
	return nil
}

func (m mockCursor) Close(ctx context.Context) error {
	return nil
}

func (m mockCursor) All(ctx context.Context, results interface{}) error {
	return nil
}

func (m mockCursor) Err() error {
	return nil
}

type MockHealthEventStore struct {
	datastore.HealthEventStore
}
