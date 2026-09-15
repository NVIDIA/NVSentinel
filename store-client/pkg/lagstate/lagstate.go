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

// Package lagstate holds the two timestamps change stream lag is derived from, per ADR-054.
//
// The provider owns these facts; the metrics that read them live in pkg/client, which cannot
// be imported from a provider without an import cycle. Tracker is embedded by each provider's
// watcher so both report lag identically.
package lagstate

import (
	"sync"
	"time"
)

// Provider is the optional interface a change stream watcher satisfies to have its lag
// exported. It is deliberately not part of datastore.ChangeStreamWatcher: that interface is
// satisfied by whole-type assertion in several places, so adding a method to it would silently
// drop implementations rather than fail the build.
//
// Every wrapper around a watcher must pass this through, or the assertion that finds it answers
// for the wrapper instead of the watcher underneath.
type Provider interface {
	LagState() (lastEmptyBatch, lastEventRead time.Time)
}

// Tracker records when a watcher last observed itself caught up, and the server-side time of
// the most recent event it read. Safe for concurrent use: the writer is the watcher's read
// loop and the reader is a metrics collector on an unrelated goroutine.
//
// The zero Tracker is ready to use and reports both timestamps as zero, which callers must
// treat as "lag unknown" rather than as caught up. Reporting zero lag for a watcher that has
// not been observed at all is the false-healthy failure ADR-054 exists to remove.
type Tracker struct {
	mu sync.Mutex

	lastEmptyBatchAt time.Time
	lastEventReadAt  time.Time
}

// RecordCaughtUp marks the moment the stream reported nothing further to deliver. That is
// affirmative evidence of being caught up, evaluated against this consumer's own filtered
// stream, which is what position and stream-head comparisons cannot provide.
func (t *Tracker) RecordCaughtUp(at time.Time) {
	if at.IsZero() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastEmptyBatchAt = at
}

// RecordEventRead marks the server-side time of an event just read off the stream. Events
// normally arrive in time order, but the timestamp is kept monotonic so an out-of-order or
// replayed event cannot make a lagging consumer look caught up.
//
// The event's own time is used rather than the wall clock, because lag is the age of what the
// consumer is reading: a backlog of old events must read as old.
func (t *Tracker) RecordEventRead(at time.Time) {
	if at.IsZero() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if at.After(t.lastEventReadAt) {
		t.lastEventReadAt = at
	}
}

// LagState returns the two timestamps. Either may be zero, meaning no observation of that
// kind has been made yet.
func (t *Tracker) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.lastEmptyBatchAt, t.lastEventReadAt
}
