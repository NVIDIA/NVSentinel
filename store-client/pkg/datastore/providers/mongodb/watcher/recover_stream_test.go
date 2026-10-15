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

package watcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A change-stream failure used to end the event loop permanently: eventLoop returned on
// stepFailed, Start's deferred closeOnce closed the event channel, and the processor
// downstream read that close as a clean shutdown. These tests pin the contract that replaced
// it — bounded recovery, then exit. The transient case is absorbed; a persistent failure still
// stops the loop, so #2024's fatal-exit path turns it into a restart that shows in
// restartCount rather than a Ready pod consuming nothing. See #2030.

func newRecoverTestWatcher(reopen func(context.Context, bool) error) *ChangeStreamWatcher {
	return &ChangeStreamWatcher{
		clientName:     "recover-test",
		reopen:         reopen,
		reopenInterval: time.Millisecond,
		reopenBudget:   time.Minute, // far beyond what the fast tests need
	}
}

func TestRecoverStream_ReopenSucceedsFirstTry_ContinuesLoop(t *testing.T) {
	var calls atomic.Int32

	w := newRecoverTestWatcher(func(context.Context, bool) error {
		calls.Add(1)
		return nil
	})

	require.True(t, w.recoverStream(context.Background(), false))
	require.Equal(t, int32(1), calls.Load())
}

// The case the production incident actually was: the server is briefly unreachable, so the
// first attempts fail. This is what the budget exists to absorb.
func TestRecoverStream_ReopenFailsThenSucceeds_RetriesWithinBudget(t *testing.T) {
	var calls atomic.Int32

	w := newRecoverTestWatcher(func(context.Context, bool) error {
		if calls.Add(1) < 3 {
			return errors.New("server selection error: context deadline exceeded")
		}

		return nil
	})

	require.True(t, w.recoverStream(context.Background(), false))
	require.Equal(t, int32(3), calls.Load())
}

// The other half of bounded-then-exit, and the reason it is bounded at all: a failure that
// does not clear must stop the loop rather than spin forever, so the process can exit and the
// restart becomes visible.
func TestRecoverStream_BudgetSpent_StopsLoop(t *testing.T) {
	var calls atomic.Int32

	w := newRecoverTestWatcher(func(context.Context, bool) error {
		calls.Add(1)
		return errors.New("still unreachable")
	})
	w.reopenInterval = 10 * time.Millisecond
	w.reopenBudget = 60 * time.Millisecond

	start := time.Now()
	require.False(t, w.recoverStream(context.Background(), false),
		"a spent budget must stop the event loop, not keep retrying")
	elapsed := time.Since(start)

	require.Greater(t, calls.Load(), int32(1), "the budget must allow more than one attempt")
	require.Less(t, elapsed, 2*time.Second, "recoverStream must return once the budget is spent")
}

// The budget is wall-clock, not a number of attempts: what it has to cover is an outage of a
// certain duration. A short interval against the same budget therefore buys more attempts.
func TestRecoverStream_BudgetIsTimeNotAttempts(t *testing.T) {
	count := func(interval time.Duration) int32 {
		var calls atomic.Int32

		w := newRecoverTestWatcher(func(context.Context, bool) error {
			calls.Add(1)
			return errors.New("still unreachable")
		})
		w.reopenInterval = interval
		w.reopenBudget = 100 * time.Millisecond

		w.recoverStream(context.Background(), false)

		return calls.Load()
	}

	require.Greater(t, count(5*time.Millisecond), count(40*time.Millisecond),
		"halving the interval under the same budget must yield more attempts")
}

func TestRecoverStream_ContextAlreadyDone_StopsWithoutReopening(t *testing.T) {
	var calls atomic.Int32

	w := newRecoverTestWatcher(func(context.Context, bool) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.False(t, w.recoverStream(ctx, false))
	require.Zero(t, calls.Load(), "a cancelled context must not trigger a reopen")
}

// Shutdown has to win against the retry loop, otherwise Close blocks on w.done until its
// 5s timeout on every restart that happens to land during a reopen.
func TestRecoverStream_ContextCancelledWhileRetrying_StopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var calls atomic.Int32

	w := newRecoverTestWatcher(func(context.Context, bool) error {
		if calls.Add(1) == 2 {
			cancel()
		}

		return errors.New("still unreachable")
	})
	w.reopenInterval = 10 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- w.recoverStream(ctx, false) }()

	select {
	case cont := <-done:
		require.False(t, cont, "recoverStream must report that the loop should stop")
	case <-time.After(2 * time.Second):
		t.Fatal("recoverStream did not return after its context was cancelled")
	}
}

// recoverStream is reached from a watcher the constructor built, where the seam fields are
// unset; it has to fall back to the real reopen path and the package defaults rather than
// spinning on a zero interval, panicking on a nil func, or treating a zero budget as spent.
func TestRecoverStream_UnsetSeams_ResolveDefaults(t *testing.T) {
	w := &ChangeStreamWatcher{clientName: "defaults"}
	require.Nil(t, w.reopen)
	require.Zero(t, w.reopenInterval)
	require.Zero(t, w.reopenBudget)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled context returns before any default is exercised against a real server,
	// which is the only way to assert this without a MongoDB.
	require.False(t, w.recoverStream(ctx, false))
}

// CodeRabbit's finding on #2040: with no stored checkpoint the reopen would start at the
// current oplog position and drop everything written during the outage, so reopenChangeStream
// falls back to the failed cursor's own position. That fallback must be disabled when the
// stored token was just discarded as unrecoverable, because that path starts fresh on purpose
// and the in-memory token is the position the server rejected. recoverStream's only job here
// is to carry the answer through unchanged.
func TestRecoverStream_ForwardsTokenDiscardedFlag(t *testing.T) {
	for _, discarded := range []bool{false, true} {
		var seen atomic.Bool

		var called atomic.Bool

		w := newRecoverTestWatcher(func(_ context.Context, tokenDiscarded bool) error {
			seen.Store(tokenDiscarded)
			called.Store(true)

			return nil
		})

		require.True(t, w.recoverStream(context.Background(), discarded))
		require.True(t, called.Load(), "reopen must be called")
		require.Equal(t, discarded, seen.Load(),
			"recoverStream must pass tokenDiscarded through untouched")
	}
}

// The classification behind that flag. The unrecoverable branch is not exercised here because
// it deletes the stored token, which needs a live collection; isUnrecoverableResumeTokenError
// has its own table test for the codes.
func TestHandleChangeStreamError_RecoverableError_KeepsStoredToken(t *testing.T) {
	w := &ChangeStreamWatcher{clientName: "classify"}
	require.False(t, w.handleChangeStreamError(errors.New("server selection error")),
		"a transient failure must not report the stored token as discarded")
}
