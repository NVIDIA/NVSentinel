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

package mongodb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

// lagStateWatcher is a minimal client.ChangeStreamWatcher that reports lag state.
type lagStateWatcher struct {
	client.ChangeStreamWatcher

	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (w *lagStateWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.lastEmptyBatch, w.lastEventRead
}

// plainWatcher reports no lag state, which is what an older or third-party watcher looks like.
type plainWatcher struct {
	client.ChangeStreamWatcher
}

// AdaptedChangeStreamWatcher wraps an interface, so a missing pass-through here would answer for
// the adapter rather than the watcher underneath and report a permanently unknown lag. That is
// the mistake this test exists to catch.
func TestAdaptedChangeStreamWatcher_WrappedProvider_PassesLagStateThrough(t *testing.T) {
	caughtUp := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	eventRead := caughtUp.Add(-time.Minute)

	adapter := NewAdaptedChangeStreamWatcher(&lagStateWatcher{
		lastEmptyBatch: caughtUp,
		lastEventRead:  eventRead,
	})

	provider, ok := adapter.(lagstate.Provider)
	assert.True(t, ok, "the adapter must report lag state")

	gotEmptyBatch, gotEventRead := provider.LagState()

	assert.Equal(t, caughtUp, gotEmptyBatch)
	assert.Equal(t, eventRead, gotEventRead)
}

// Wrapping a watcher that reports no lag state yields two zero times, which callers read as
// unknown. It must not look caught up.
func TestAdaptedChangeStreamWatcher_WatcherWithoutLagState_ReportsUnknown(t *testing.T) {
	adapter := NewAdaptedChangeStreamWatcher(&plainWatcher{})

	provider, ok := adapter.(lagstate.Provider)
	assert.True(t, ok)

	lastEmptyBatch, lastEventRead := provider.LagState()

	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
}
