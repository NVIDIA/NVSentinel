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

package lagstate

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The zero Tracker must report both timestamps as zero, which callers read as "lag unknown".
// Reporting anything else here would let a watcher that has not been observed look caught up.
func TestLagState_ZeroTracker_ReportsNoObservation(t *testing.T) {
	var tracker Tracker

	lastEmptyBatch, lastEventRead := tracker.LagState()

	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
}

func TestLagState_BothRecorded_ReportsEachIndependently(t *testing.T) {
	var tracker Tracker

	caughtUp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eventRead := caughtUp.Add(-time.Hour)

	tracker.RecordCaughtUp(caughtUp)
	tracker.RecordEventRead(eventRead)

	lastEmptyBatch, lastEventRead := tracker.LagState()

	assert.Equal(t, caughtUp, lastEmptyBatch)
	assert.Equal(t, eventRead, lastEventRead)
}

// An out-of-order or replayed event must not pull lastEventRead backwards, which would make a
// lagging consumer look caught up.
func TestRecordEventRead_OlderEvent_KeepsNewestTimestamp(t *testing.T) {
	var tracker Tracker

	newest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tracker.RecordEventRead(newest)
	tracker.RecordEventRead(newest.Add(-10 * time.Minute))

	_, lastEventRead := tracker.LagState()

	assert.Equal(t, newest, lastEventRead)
}

func TestRecordCaughtUp_ZeroTimestamp_IsIgnored(t *testing.T) {
	var tracker Tracker

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tracker.RecordCaughtUp(at)
	tracker.RecordEventRead(at)

	tracker.RecordCaughtUp(time.Time{})
	tracker.RecordEventRead(time.Time{})

	lastEmptyBatch, lastEventRead := tracker.LagState()

	assert.Equal(t, at, lastEmptyBatch)
	assert.Equal(t, at, lastEventRead)
}

// The writer is a watcher's read loop and the reader is a metrics collector on an unrelated
// goroutine, so this runs under -race in CI.
func TestTracker_ConcurrentRecordAndRead_StaysConsistent(t *testing.T) {
	var tracker Tracker

	var wg sync.WaitGroup

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	for i := range 50 {
		wg.Add(2)

		go func(i int) {
			defer wg.Done()
			tracker.RecordEventRead(start.Add(time.Duration(i) * time.Second))
		}(i)

		go func() {
			defer wg.Done()
			tracker.LagState()
		}()
	}

	wg.Wait()

	_, lastEventRead := tracker.LagState()
	assert.Equal(t, start.Add(49*time.Second), lastEventRead)
}
