// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nicdriver

import (
	"log/slog"
	"regexp"
	"time"
)

const softLockupPatternName = "mlx5_napi_soft_lockup"

// A kernel soft-lockup report is emitted as many separate journal entries:
// the header line carries CPU and duration but no driver attribution, while
// the RIP/call-trace lines that attribute it to mlx5 arrive on subsequent
// entries. The detector therefore arms on the header and confirms on an mlx5
// NAPI stack frame within a bounded number of following entries.
//
// The window is line-count based rather than wall-clock based on purpose:
// after a monitor restart the journal backlog is replayed at full speed, so
// wall-clock bounds would not correspond to journal-time distances.
const (
	// softLockupWindowLines bounds how many kernel log entries after a
	// "BUG: soft lockup" header may confirm mlx5 attribution. Dumps are
	// serialized under printk_cpu_sync on modern kernels and the RIP line
	// typically lands within ~10 entries; the slack absorbs interleaved
	// output from older kernels without cpu-sync.
	softLockupWindowLines = 150

	// softLockupEmitCooldown rate-limits fatal event emission: the kernel
	// watchdog re-reports a persistent lockup every few tens of seconds,
	// and each re-report would otherwise re-confirm.
	softLockupEmitCooldown = 30 * time.Minute
)

var (
	softLockupHeaderRe = regexp.MustCompile(`BUG: soft lockup - CPU#(\d+) stuck for (\d+)s`)

	// Frames prefixed with "? " are speculative stack remnants the unwinder
	// is not sure about; they appear in unrelated dumps and must not count
	// as attribution evidence.
	softLockupSpeculativeRe = regexp.MustCompile(`\?\s+mlx5e_(poll_ico_cq|napi_poll)\+0x`)
)

// softLockupMatch is a confirmed mlx5-attributed soft lockup.
type softLockupMatch struct {
	cpu      string
	duration string
	evidence string
}

// softLockupDetector correlates a soft-lockup header with mlx5 NAPI stack
// evidence across consecutive kernel log lines. It is driven line-by-line
// from ProcessLine and is not safe for concurrent use, matching the
// monitor's sequential journal processing.
type softLockupDetector struct {
	pattern   CompiledPattern
	armed     bool
	cpu       string
	duration  string
	linesLeft int
	lastEmit  time.Time
	now       func() time.Time

	// pending holds the last confirmed match until a different line is
	// observed. The monitor re-evaluates the SAME journal entry when event
	// delivery fails (processOneEntryAndAdvance retries without advancing
	// the cursor), so a confirmation must be reproducible on re-delivery or
	// the fatal event is silently dropped after a send failure.
	pending        *softLockupMatch
	pendingMessage string
}

func newSoftLockupDetector(pattern CompiledPattern) *softLockupDetector {
	return &softLockupDetector{
		pattern: pattern,
		now:     time.Now,
	}
}

// observe consumes one kernel log line and returns a non-nil match when the
// line confirms mlx5 attribution for a recently seen soft-lockup header.
func (d *softLockupDetector) observe(message string) *softLockupMatch {
	if d.pending != nil {
		// The pipeline only moves to a different entry after the previous
		// one was delivered successfully, so an identical message is a
		// delivery retry (return the same match, bypassing the cooldown)
		// and any other message means the match was delivered.
		if message == d.pendingMessage {
			return d.pending
		}

		d.pending = nil
		d.pendingMessage = ""
	}

	if m := softLockupHeaderRe.FindStringSubmatch(message); m != nil {
		d.armed = true
		d.cpu = m[1]
		d.duration = m[2]
		d.linesLeft = softLockupWindowLines

		return nil
	}

	if !d.armed {
		return nil
	}

	d.linesLeft--
	if d.linesLeft <= 0 {
		d.armed = false
	}

	if !d.pattern.Re.MatchString(message) || softLockupSpeculativeRe.MatchString(message) {
		return nil
	}

	d.armed = false

	if !d.lastEmit.IsZero() && d.now().Sub(d.lastEmit) < softLockupEmitCooldown {
		slog.Debug("Suppressing repeated mlx5 NAPI soft lockup event within cooldown",
			"cpu", d.cpu, "durationSeconds", d.duration)

		return nil
	}

	d.lastEmit = d.now()

	match := &softLockupMatch{
		cpu:      d.cpu,
		duration: d.duration,
		evidence: message,
	}
	d.pending = match
	d.pendingMessage = message

	return match
}
