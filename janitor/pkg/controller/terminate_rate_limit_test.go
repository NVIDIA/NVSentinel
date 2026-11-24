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

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var rateLimitTestNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestTerminateRateLimit_AllowsUpToTheLimitThenWaitsForTheOldestSignal(t *testing.T) {
	limit := newTerminateRateLimit(2, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)
	_, ok = limit.reserve("b", rateLimitTestNow.Add(10*time.Second), nil)
	require.True(t, ok)

	// The window holds a (sent at 0s) and b (10s): c waits until a leaves it at 60s.
	wait, ok := limit.reserve("c", rateLimitTestNow.Add(20*time.Second), nil)
	require.False(t, ok)
	require.Equal(t, 40*time.Second, wait)

	_, ok = limit.reserve("c", rateLimitTestNow.Add(61*time.Second), nil)
	require.True(t, ok, "a left the window")
}

func TestTerminateRateLimit_CountsSignalsTheStatusesRecord(t *testing.T) {
	limit := newTerminateRateLimit(2, time.Minute)
	recorded := map[string]time.Time{
		// Sent by a previous janitor process, before a restart.
		"before-restart": rateLimitTestNow.Add(-30 * time.Second),
		// Outside the window.
		"long-ago": rateLimitTestNow.Add(-2 * time.Minute),
	}

	_, ok := limit.reserve("a", rateLimitTestNow, recorded)
	require.True(t, ok)

	// a now appears in both the statuses and this process's record: it counts once.
	recorded["a"] = rateLimitTestNow
	wait, ok := limit.reserve("b", rateLimitTestNow.Add(time.Second), recorded)
	require.False(t, ok)
	require.Equal(t, 29*time.Second, wait, "before-restart leaves the window first")
}

func TestTerminateRateLimit_AResendTakesNoSecondSignal(t *testing.T) {
	limit := newTerminateRateLimit(1, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)

	// The status update of a's signal was lost, so a is reconciled and sends again.
	_, ok = limit.reserve("a", rateLimitTestNow.Add(time.Second), nil)
	require.True(t, ok)

	_, ok = limit.reserve("b", rateLimitTestNow.Add(2*time.Second), nil)
	require.False(t, ok)
}

func TestTerminateRateLimit_AFailedSignalIsReleased(t *testing.T) {
	limit := newTerminateRateLimit(1, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)
	limit.release("a")

	_, ok = limit.reserve("b", rateLimitTestNow.Add(time.Second), nil)
	require.True(t, ok, "a's terminate failed, so its signal does not count")
}

func TestTerminateRateLimit_WaitsAtLeastASecond(t *testing.T) {
	limit := newTerminateRateLimit(1, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)

	// a leaves the window in a few milliseconds.
	wait, ok := limit.reserve("b", rateLimitTestNow.Add(time.Minute-time.Millisecond), nil)
	require.False(t, ok)
	require.Equal(t, minimumRateLimitWait, wait)
}

func TestTerminateRateLimit_TheHoldCoversTheSignalFromWhenItWasSent(t *testing.T) {
	limit := newTerminateRateLimit(1, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)

	// The provider took 50s to accept a's signal, and the status does not show it yet.
	limit.recordSent("a", rateLimitTestNow.Add(50*time.Second))

	wait, ok := limit.reserve("b", rateLimitTestNow.Add(61*time.Second), nil)
	require.False(t, ok, "a's signal, sent at 50s, is still in the window")
	require.Equal(t, 49*time.Second, wait)

	_, ok = limit.reserve("b", rateLimitTestNow.Add(111*time.Second), nil)
	require.True(t, ok)
}

func TestTerminateRateLimit_AFailedResendKeepsTheSentSignal(t *testing.T) {
	limit := newTerminateRateLimit(1, time.Minute)

	_, ok := limit.reserve("a", rateLimitTestNow, nil)
	require.True(t, ok)
	limit.recordSent("a", rateLimitTestNow.Add(time.Second))

	// a's status update was lost: a resends, and the resend fails.
	_, ok = limit.reserve("a", rateLimitTestNow.Add(10*time.Second), nil)
	require.True(t, ok)
	limit.release("a")

	_, ok = limit.reserve("b", rateLimitTestNow.Add(20*time.Second), nil)
	require.False(t, ok, "a's first signal was sent and still counts")
}
