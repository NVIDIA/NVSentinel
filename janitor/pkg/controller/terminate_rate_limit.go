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
	"sync"
	"time"
)

// conditionReasonRateLimited marks a TerminateNode whose terminate signal waits for the rate limit.
const conditionReasonRateLimited = "RateLimited"

// minimumRateLimitWait keeps a waiting request from being reconciled in a tight loop when the
// oldest signal in the window is about to leave it.
const minimumRateLimitWait = time.Second

// terminateRateLimit allows at most limit terminate signals in any window. The signals in the
// window are the ones the TerminateNode statuses record, so the limit holds across restarts, plus
// the ones this process holds, whose status the cache may not show yet. A request over the limit
// waits; it does not fail, so fault-remediation does not spend a remediation attempt on it.
type terminateRateLimit struct {
	limit  int
	window time.Duration

	mu sync.Mutex
	// holds keeps this process's signals by TerminateNode name until they leave the window.
	holds map[string]terminateHold
}

// terminateHold is a signal this process is about to send, or sent, for one TerminateNode.
type terminateHold struct {
	at time.Time
	// sent marks a signal the provider accepted. Its hold is never released.
	sent bool
}

func newTerminateRateLimit(limit int, window time.Duration) *terminateRateLimit {
	return &terminateRateLimit{limit: limit, window: window, holds: make(map[string]terminateHold)}
}

// reserve claims a signal for the named TerminateNode at now. recorded holds the signal times the
// TerminateNode statuses show, by name. When the window is full, reserve claims nothing and
// returns how long to wait for a signal to leave the window.
func (l *terminateRateLimit) reserve(name string, now time.Time, recorded map[string]time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	start := now.Add(-l.window)
	inWindow := make(map[string]time.Time, len(recorded)+len(l.holds))

	for other, sentAt := range recorded {
		if sentAt.After(start) {
			inWindow[other] = sentAt
		}
	}

	for other, hold := range l.holds {
		if !hold.at.After(start) {
			delete(l.holds, other)
			continue
		}

		if _, ok := inWindow[other]; !ok {
			inWindow[other] = hold.at
		}
	}

	// A request that already holds a signal, such as a resend after a lost status update, does
	// not take a second one, and its hold keeps its time.
	if _, ok := inWindow[name]; ok {
		return 0, true
	}

	if len(inWindow) < l.limit {
		l.holds[name] = terminateHold{at: now}
		return 0, true
	}

	oldest := now
	for _, sentAt := range inWindow {
		if sentAt.Before(oldest) {
			oldest = sentAt
		}
	}

	return max(oldest.Add(l.window).Sub(now), minimumRateLimitWait), false
}

// recordSent marks the named request's signal as accepted at sentAt, the time its status records.
// Its hold then lasts until that signal leaves the window, and a later failed resend keeps it.
func (l *terminateRateLimit) recordSent(name string, sentAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.holds[name] = terminateHold{at: sentAt, sent: true}
}

// release returns the hold of a request whose terminate failed: no instance went away. The hold of
// a signal the provider accepted stays.
func (l *terminateRateLimit) release(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if hold, ok := l.holds[name]; ok && !hold.sent {
		delete(l.holds, name)
	}
}
