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

package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type nodeTestEvent struct {
	id       string
	nodeName string
	token    []byte
}

func newNodeTestEvent(id, nodeName string) *nodeTestEvent {
	return &nodeTestEvent{
		id:       id,
		nodeName: nodeName,
		token:    []byte(id),
	}
}

func (e *nodeTestEvent) GetDocumentID() (string, error) { return e.id, nil }
func (e *nodeTestEvent) GetRecordUUID() (string, error) { return e.id, nil }
func (e *nodeTestEvent) GetNodeName() (string, error)   { return e.nodeName, nil }
func (e *nodeTestEvent) GetResumeToken() []byte         { return e.token }

func (e *nodeTestEvent) UnmarshalDocument(value any) error {
	event, ok := value.(*model.HealthEventWithStatus)
	if !ok {
		return fmt.Errorf("unexpected document type %T", value)
	}

	event.HealthEvent = &protos.HealthEvent{
		Id:       e.id,
		NodeName: e.nodeName,
	}

	return nil
}

func TestNewEventProcessor_Factory(t *testing.T) {
	watcher := newEventProcessorTestWatcher()

	// Workers <= 1 returns DefaultEventProcessor
	p1 := NewEventProcessor(watcher, nil, EventProcessorConfig{Workers: 0})
	assert.IsType(t, &DefaultEventProcessor{}, p1)

	p2 := NewEventProcessor(watcher, nil, EventProcessorConfig{Workers: 1})
	assert.IsType(t, &DefaultEventProcessor{}, p2)

	// Workers > 1 returns PartitionedEventProcessor
	p3 := NewEventProcessor(watcher, nil, EventProcessorConfig{Workers: 4})
	assert.IsType(t, &PartitionedEventProcessor{}, p3)
}

func TestPartitionedEventProcessor_NodeOrdering(t *testing.T) {
	// Create multiple events for node-a and node-b
	const eventsPerNode = 20
	events := make([]Event, 0, eventsPerNode*2)

	for i := range eventsPerNode {
		events = append(events, newNodeTestEvent(fmt.Sprintf("node-a-%02d", i), "node-a"))
		events = append(events, newNodeTestEvent(fmt.Sprintf("node-b-%02d", i), "node-b"))
	}

	watcher := newEventProcessorTestWatcher(events...)

	processor := NewPartitionedEventProcessor(watcher, nil, EventProcessorConfig{
		Workers:              4,
		MarkProcessedOnError: true,
	})

	var mu sync.Mutex
	nodeAHistory := make([]string, 0, eventsPerNode)
	nodeBHistory := make([]string, 0, eventsPerNode)

	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, e *model.HealthEventWithStatus) error {
		mu.Lock()
		defer mu.Unlock()

		if e.HealthEvent.NodeName == "node-a" {
			nodeAHistory = append(nodeAHistory, e.HealthEvent.Id)
		} else if e.HealthEvent.NodeName == "node-b" {
			nodeBHistory = append(nodeBHistory, e.HealthEvent.Id)
		}

		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := processor.Start(ctx)
	require.NoError(t, err)

	// Verify that events for node-a were processed in strict ascending order
	require.Len(t, nodeAHistory, eventsPerNode)
	for i := range eventsPerNode {
		expectedID := fmt.Sprintf("node-a-%02d", i)
		assert.Equal(t, expectedID, nodeAHistory[i], "node-a events must remain strictly ordered")
	}

	// Verify that events for node-b were processed in strict ascending order
	require.Len(t, nodeBHistory, eventsPerNode)
	for i := range eventsPerNode {
		expectedID := fmt.Sprintf("node-b-%02d", i)
		assert.Equal(t, expectedID, nodeBHistory[i], "node-b events must remain strictly ordered")
	}
}

func TestPartitionedEventProcessor_ConcurrencyAcrossNodes(t *testing.T) {
	// Event 1 (node-a) will block on slowProcessing channel
	// Event 2 (node-b) will complete immediately
	event1 := newNodeTestEvent("event-1", "node-a")
	event2 := newNodeTestEvent("event-2", "node-b")

	watcher := newEventProcessorTestWatcher(event1, event2)

	processor := NewPartitionedEventProcessor(watcher, nil, EventProcessorConfig{
		Workers:              4,
		MarkProcessedOnError: true,
	})

	nodeBCompleted := make(chan struct{})
	unblockNodeA := make(chan struct{})

	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, e *model.HealthEventWithStatus) error {
		if e.HealthEvent.NodeName == "node-a" {
			<-unblockNodeA
		} else if e.HealthEvent.NodeName == "node-b" {
			close(nodeBCompleted)
		}

		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- processor.Start(ctx)
	}()

	// Wait for Node B to complete while Node A is still blocked
	select {
	case <-nodeBCompleted:
		// Node B completed concurrently while Node A was blocked
	case <-time.After(2 * time.Second):
		t.Fatal("Node B was blocked by Node A - concurrency across nodes failed")
	}

	// Low-water mark must NOT have checkpointed event-2 yet because event-1 is still unresolved
	assert.Empty(t, watcher.markedTokens, "event-2 must not be checkpointed before event-1 finishes")

	// Now unblock Node A
	close(unblockNodeA)

	require.NoError(t, <-errCh)

	// After both finish, the checkpoint should have advanced to event-2
	require.NotEmpty(t, watcher.markedTokens)
	assert.Equal(t, "event-2", watcher.markedTokens[len(watcher.markedTokens)-1])
}

func TestPartitionedEventProcessor_PoisonPillHandling(t *testing.T) {
	// With MarkProcessedOnError: true, failing an event should not halt stream consumption
	event1 := newNodeTestEvent("poison-event", "node-a")
	event2 := newNodeTestEvent("good-event", "node-a")

	watcher := newEventProcessorTestWatcher(event1, event2)

	processor := NewPartitionedEventProcessor(watcher, nil, EventProcessorConfig{
		Workers:              2,
		MarkProcessedOnError: true,
	})

	var goodProcessed atomic.Bool

	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, e *model.HealthEventWithStatus) error {
		if e.HealthEvent.Id == "poison-event" {
			return fmt.Errorf("simulated error")
		}
		if e.HealthEvent.Id == "good-event" {
			goodProcessed.Store(true)
		}

		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := processor.Start(ctx)
	require.NoError(t, err)

	assert.True(t, goodProcessed.Load(), "good-event should be processed after poison-event")
}

func TestPartitionedEventProcessor_TimeoutNotCheckpointed(t *testing.T) {
	// A timeout (context.DeadlineExceeded) is a transient condition and must NOT be marked
	// as processed, even if MarkProcessedOnError=true, so it can be retried on restart.
	event1 := newNodeTestEvent("timeout-event", "node-a")
	event2 := newNodeTestEvent("good-event", "node-b")

	watcher := newEventProcessorTestWatcher(event1, event2)

	processor := NewPartitionedEventProcessor(watcher, nil, EventProcessorConfig{
		Workers:              2,
		MarkProcessedOnError: true,
	})

	event2Processed := make(chan struct{})

	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, e *model.HealthEventWithStatus) error {
		if e.HealthEvent.Id == "timeout-event" {
			// Simulate transient context timeout
			return context.DeadlineExceeded
		}
		if e.HealthEvent.Id == "good-event" {
			close(event2Processed)
		}

		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := processor.Start(ctx)
	require.NoError(t, err)

	<-event2Processed

	// Verify that event1's token was NOT checkpointed because timeout is transient
	for _, token := range watcher.markedTokens {
		assert.NotEqual(t, "timeout-event", token, "timeout event must not be checkpointed")
	}
}

func TestPartitionedEventProcessor_DiscardBufferedTasksOnCancellation(t *testing.T) {
	// When context is canceled during shutdown, any buffered tasks in worker channels
	// must be discarded without calling the handler or advancing the checkpoint.
	event1 := newNodeTestEvent("event-1", "node-a")
	event2 := newNodeTestEvent("event-2", "node-a")
	event3 := newNodeTestEvent("event-3", "node-a")

	watcher := newEventProcessorTestWatcher(event1, event2, event3)

	processor := NewPartitionedEventProcessor(watcher, nil, EventProcessorConfig{
		Workers:              1,
		MarkProcessedOnError: true,
	})

	ctx, cancel := context.WithCancel(context.Background())

	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, e *model.HealthEventWithStatus) error {
		if e.HealthEvent.Id == "event-1" {
			// Cancel context while event 2 and 3 are buffered in the worker channel
			cancel()

			return nil
		}

		if e.HealthEvent.Id == "event-2" || e.HealthEvent.Id == "event-3" {
			t.Errorf("Event %s should not have been executed after cancellation", e.HealthEvent.Id)
		}

		return nil
	}))

	_ = processor.Start(ctx)

	// Neither event-2 nor event-3 should have been marked
	for _, token := range watcher.markedTokens {
		assert.NotEqual(t, "event-2", token)
		assert.NotEqual(t, "event-3", token)
	}
}

