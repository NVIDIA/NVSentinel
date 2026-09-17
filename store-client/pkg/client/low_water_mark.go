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
	"sync"
)

type trackerItem struct {
	seq   uint64
	token []byte
	done  bool
}

// LowWaterMarkTracker tracks in-flight change stream events and computes
// the highest contiguous completed resume token.
//
// Because events may finish processing out of order when handled concurrently,
// the resume token must only advance past an event once every earlier event has
// also finished. This prevents skipped events on crash or restart.
type LowWaterMarkTracker struct {
	mu                  sync.Mutex
	items               []*trackerItem
	itemMap             map[uint64]*trackerItem
	nextSeq             uint64
	lastCheckpointToken []byte
	drainCh             chan struct{}
}

// NewLowWaterMarkTracker creates a new thread-safe LowWaterMarkTracker.
func NewLowWaterMarkTracker() *LowWaterMarkTracker {
	return &LowWaterMarkTracker{
		itemMap: make(map[uint64]*trackerItem),
		nextSeq: 1,
		drainCh: make(chan struct{}, 1),
	}
}

// Register registers a newly admitted change stream event with its resume token
// and returns an allocated sequence number.
func (t *LowWaterMarkTracker) Register(token []byte) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	seq := t.nextSeq
	t.nextSeq++

	item := &trackerItem{
		seq:   seq,
		token: token,
		done:  false,
	}

	t.items = append(t.items, item)
	t.itemMap[seq] = item

	return seq
}

// MarkDone marks the given sequence number as completed.
// If this resolves the head of the in-flight window, the low-water mark
// advances across all contiguous completed items. The newest contiguous token
// is returned. If the watermark does not advance, nil is returned.
func (t *LowWaterMarkTracker) MarkDone(seq uint64) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	item, exists := t.itemMap[seq]
	if !exists {
		return nil
	}

	item.done = true

	return t.advanceLocked()
}

// advanceLocked inspects the head of the items slice and pops contiguous
// completed entries. Must be called while holding t.mu.
func (t *LowWaterMarkTracker) advanceLocked() []byte {
	var highestToken []byte

	popCount := 0

	for popCount < len(t.items) && t.items[popCount].done {
		highestToken = t.items[popCount].token
		delete(t.itemMap, t.items[popCount].seq)
		t.items[popCount] = nil
		popCount++
	}

	if popCount > 0 {
		t.items = t.items[popCount:]
		if len(t.items) == 0 && cap(t.items) > 1024 {
			t.items = nil
		}

		if len(highestToken) > 0 {
			t.lastCheckpointToken = highestToken
		}

		// Notify any backpressure waiter that in-flight capacity has been freed
		select {
		case t.drainCh <- struct{}{}:
		default:
		}
	}

	return highestToken
}

// InFlightCount returns the current number of in-flight / uncheckpointed items.
func (t *LowWaterMarkTracker) InFlightCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.itemMap)
}

// DrainCh returns a channel that receives a signal when in-flight items drain.
func (t *LowWaterMarkTracker) DrainCh() <-chan struct{} {
	return t.drainCh
}

// LastCheckpointedToken returns the latest checkpointed token.
func (t *LowWaterMarkTracker) LastCheckpointedToken() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.lastCheckpointToken
}

// Flush advances over any contiguous completed items and returns the newest token.
func (t *LowWaterMarkTracker) Flush() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.advanceLocked()
}
