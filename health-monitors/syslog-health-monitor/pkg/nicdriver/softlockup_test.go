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
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Realistic journal MESSAGE lines from an mlx5 NAPI soft-lockup kernel dump.
const (
	lockupHeaderLine  = "watchdog: BUG: soft lockup - CPU#94 stuck for 226906s! [swapper/94:0]"
	lockupModulesLine = "Modules linked in: xt_conntrack nf_conntrack br_netfilter mlx5_ib mlx5_core"
	lockupCPULine     = "CPU: 94 PID: 0 Comm: swapper/94 Tainted: G             L 5.15.0-1053-azure #61-Ubuntu"
	lockupRIPLine     = "RIP: 0010:mlx5e_poll_ico_cq+0x8b/0x1a0 [mlx5_core]"
	lockupFrameLine   = " mlx5e_napi_poll+0x142/0x680 [mlx5_core]"
	lockupSpecLine    = " ? mlx5e_napi_poll+0x142/0x680 [mlx5_core]"
	lockupNetRxLine   = " net_rx_action+0x142/0x2a0"
	lockupSoftirqLine = " __do_softirq+0xd9/0x2e7"
	unrelatedLine     = "audit: type=1400 audit(1743126491.123:456): apparmor=\"STATUS\""
)

var lockupTestPattern = CompiledPattern{
	Name:              softLockupPatternName,
	Re:                regexp.MustCompile(`mlx5e_(poll_ico_cq|napi_poll)\+0x`),
	IsFatal:           true,
	RecommendedAction: pb.RecommendedAction_REPLACE_VM,
}

func makeLockupHandler(t *testing.T) *NICDriverHandler {
	t.Helper()

	patterns := append([]CompiledPattern{}, defaultTestPatterns...)
	patterns = append(patterns, lockupTestPattern)

	return makeHandler(t, patterns, newMockResolver(nil))
}

// feed runs lines through the handler and returns the events emitted per line
// (nil entries for lines that produced nothing).
func feed(t *testing.T, h *NICDriverHandler, lines ...string) []*pb.HealthEvents {
	t.Helper()

	results := make([]*pb.HealthEvents, 0, len(lines))

	for _, line := range lines {
		events, err := h.ProcessLine(line)
		require.NoError(t, err)

		results = append(results, events)
	}

	return results
}

func onlyEvent(t *testing.T, events *pb.HealthEvents) *pb.HealthEvent {
	t.Helper()

	require.NotNil(t, events)
	require.Len(t, events.Events, 1)

	return events.Events[0]
}

func TestSoftLockup_HeaderThenRIPEmitsFatal(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h, lockupHeaderLine, lockupModulesLine, lockupCPULine, lockupRIPLine)

	assert.Nil(t, results[0], "header line alone must not emit")
	assert.Nil(t, results[1])
	assert.Nil(t, results[2])

	event := onlyEvent(t, results[3])
	assert.True(t, event.IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, event.RecommendedAction)
	assert.Equal(t, []string{softLockupPatternName}, event.ErrorCode)
	assert.Equal(t, componentClassNIC, event.ComponentClass)
	assert.Empty(t, event.EntitiesImpacted, "dump carries no BDF, event is node-scoped")
	assert.Equal(t, "94", event.Metadata["cpu"])
	assert.Equal(t, "226906", event.Metadata["durationSeconds"])
	assert.Contains(t, event.Message, "CPU#94")
	assert.Contains(t, event.Message, "226906s")
	assert.Contains(t, event.Message, lockupRIPLine)
}

func TestSoftLockup_CallTraceFrameAlsoConfirms(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h, lockupHeaderLine, lockupModulesLine, lockupFrameLine)

	event := onlyEvent(t, results[2])
	assert.True(t, event.IsFatal)
	assert.Equal(t, []string{softLockupPatternName}, event.ErrorCode)
}

func TestSoftLockup_SpeculativeFramesDoNotConfirm(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h, lockupHeaderLine, lockupSpecLine, lockupSpecLine, lockupNetRxLine)

	for i, r := range results {
		assert.Nil(t, r, "line %d must not emit", i)
	}
}

func TestSoftLockup_SpeculativeThenRealFrameConfirms(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h, lockupHeaderLine, lockupSpecLine, lockupFrameLine)

	assert.Nil(t, results[1], "speculative frame must not emit")
	onlyEvent(t, results[2])
}

// TestSoftLockup_EvidenceWithoutHeaderNoEvent guards against the evidence
// regex acting as a single-line pattern: a lone mlx5 stack frame (e.g. from
// an unrelated WARN backtrace) must not emit anything.
func TestSoftLockup_EvidenceWithoutHeaderNoEvent(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h, lockupRIPLine, lockupFrameLine)

	assert.Nil(t, results[0])
	assert.Nil(t, results[1])
}

// TestSoftLockup_GenericLockupNoMlx5NoEvent verifies that a soft lockup in a
// different subsystem is not attributed to the mlx5 NIC. Notably,
// net_rx_action appears in every NAPI lockup regardless of driver and must
// not count as mlx5 evidence.
func TestSoftLockup_GenericLockupNoMlx5NoEvent(t *testing.T) {
	h := makeLockupHandler(t)

	results := feed(t, h,
		lockupHeaderLine,
		lockupModulesLine,
		"RIP: 0010:e1000_clean+0x2b/0x8a0 [e1000e]",
		lockupNetRxLine,
		lockupSoftirqLine,
	)

	for i, r := range results {
		assert.Nil(t, r, "line %d must not emit", i)
	}
}

func TestSoftLockup_WindowBoundary(t *testing.T) {
	t.Run("evidence on last line of window confirms", func(t *testing.T) {
		h := makeLockupHandler(t)

		_, err := h.ProcessLine(lockupHeaderLine)
		require.NoError(t, err)

		for i := 0; i < softLockupWindowLines-1; i++ {
			events, err := h.ProcessLine(unrelatedLine)
			require.NoError(t, err)
			require.Nil(t, events)
		}

		events, err := h.ProcessLine(lockupRIPLine)
		require.NoError(t, err)
		onlyEvent(t, events)
	})

	t.Run("evidence one line past window is ignored", func(t *testing.T) {
		h := makeLockupHandler(t)

		_, err := h.ProcessLine(lockupHeaderLine)
		require.NoError(t, err)

		for i := 0; i < softLockupWindowLines; i++ {
			events, err := h.ProcessLine(unrelatedLine)
			require.NoError(t, err)
			require.Nil(t, events)
		}

		events, err := h.ProcessLine(lockupRIPLine)
		require.NoError(t, err)
		assert.Nil(t, events)
	})
}

// TestSoftLockup_NewHeaderReArmsWindow verifies that a fresh soft-lockup
// re-report resets the window and updates the captured header fields — the
// kernel re-reports a persistent lockup with a growing duration.
func TestSoftLockup_NewHeaderReArmsWindow(t *testing.T) {
	h := makeLockupHandler(t)

	_, err := h.ProcessLine(lockupHeaderLine)
	require.NoError(t, err)

	for i := 0; i < softLockupWindowLines; i++ {
		_, err := h.ProcessLine(unrelatedLine)
		require.NoError(t, err)
	}

	secondHeader := "watchdog: BUG: soft lockup - CPU#94 stuck for 226932s! [swapper/94:0]"
	_, err = h.ProcessLine(secondHeader)
	require.NoError(t, err)

	events, err := h.ProcessLine(lockupRIPLine)
	require.NoError(t, err)

	event := onlyEvent(t, events)
	assert.Equal(t, "226932", event.Metadata["durationSeconds"],
		"metadata must come from the most recent header")
}

func TestSoftLockup_CooldownSuppressesRepeats(t *testing.T) {
	h := makeLockupHandler(t)

	current := time.Date(2026, 3, 28, 2, 8, 11, 0, time.UTC)
	h.lockup.now = func() time.Time { return current }

	results := feed(t, h, lockupHeaderLine, lockupRIPLine)
	onlyEvent(t, results[1])

	// The kernel watchdog re-reports a persistent lockup within seconds.
	current = current.Add(30 * time.Second)

	results = feed(t, h, lockupHeaderLine, lockupRIPLine)
	assert.Nil(t, results[1], "re-report within cooldown must be suppressed")

	current = current.Add(softLockupEmitCooldown)

	results = feed(t, h, lockupHeaderLine, lockupRIPLine)
	onlyEvent(t, results[1])
}

// TestSoftLockup_InterleavedPatternLinesCoexist verifies that single-line
// patterns keep firing while the detector window is armed, and that the
// window still confirms afterwards — the TX timeout line and the lockup dump
// co-occur in a real wedge.
func TestSoftLockup_InterleavedPatternLinesCoexist(t *testing.T) {
	h := makeLockupHandler(t)

	txLine := "mlx5_core 0000:65:00.0 ens15np0: TX timeout detected"

	results := feed(t, h, lockupHeaderLine, txLine, lockupCPULine, lockupRIPLine)

	txEvent := onlyEvent(t, results[1])
	assert.Equal(t, []string{"mlx5_tx_timeout_detected"}, txEvent.ErrorCode)
	assert.False(t, txEvent.IsFatal)

	lockupEvent := onlyEvent(t, results[3])
	assert.Equal(t, []string{softLockupPatternName}, lockupEvent.ErrorCode)
	assert.True(t, lockupEvent.IsFatal)
}

// TestSoftLockup_NotConfiguredMeansDisabled verifies that without the
// mlx5_napi_soft_lockup pattern in the config, a full dump emits nothing.
func TestSoftLockup_NotConfiguredMeansDisabled(t *testing.T) {
	h := makeHandler(t, defaultTestPatterns, newMockResolver(nil))
	require.Nil(t, h.lockup)

	results := feed(t, h, lockupHeaderLine, lockupModulesLine, lockupRIPLine, lockupFrameLine)

	for i, r := range results {
		assert.Nil(t, r, "line %d must not emit", i)
	}
}

// TestSoftLockup_PatternRoutedOutOfSingleLineLoop verifies newWithDeps moves
// the lockup pattern out of the first-match-wins list.
func TestSoftLockup_PatternRoutedOutOfSingleLineLoop(t *testing.T) {
	h := makeLockupHandler(t)

	require.NotNil(t, h.lockup)
	assert.Len(t, h.patterns, len(defaultTestPatterns))

	for _, p := range h.patterns {
		assert.NotEqual(t, softLockupPatternName, p.Name)
	}
}

func TestSoftLockup_HeaderRegexVariants(t *testing.T) {
	variants := []string{
		"watchdog: BUG: soft lockup - CPU#94 stuck for 226906s! [swapper/94:0]",
		"NMI watchdog: BUG: soft lockup - CPU#3 stuck for 22s! [kworker/3:1:181]",
		"BUG: soft lockup - CPU#0 stuck for 26s! [systemd:1]",
	}

	for _, line := range variants {
		t.Run(line, func(t *testing.T) {
			assert.True(t, softLockupHeaderRe.MatchString(line))
		})
	}
}

// TestSoftLockup_RedeliveryOfConfirmingEntryReturnsSameMatch covers the
// monitor's at-least-once delivery: when sending a health event fails,
// processOneEntryAndAdvance re-evaluates the SAME journal entry without
// advancing the cursor. The detector must reproduce the confirmed match on
// re-delivery (bypassing the cooldown) or the fatal event is silently lost.
func TestSoftLockup_RedeliveryOfConfirmingEntryReturnsSameMatch(t *testing.T) {
	h := makeLockupHandler(t)

	current := time.Date(2026, 3, 28, 2, 8, 11, 0, time.UTC)
	h.lockup.now = func() time.Time { return current }

	results := feed(t, h, lockupHeaderLine, lockupRIPLine)
	first := onlyEvent(t, results[1])

	// Simulate two failed-send retries of the same entry: identical message,
	// no cursor advance. Both must reproduce the fatal event even though the
	// cooldown window is now active.
	for i := 0; i < 2; i++ {
		events, err := h.ProcessLine(lockupRIPLine)
		require.NoError(t, err)

		retry := onlyEvent(t, events)
		assert.Equal(t, first.ErrorCode, retry.ErrorCode)
		assert.Equal(t, first.Message, retry.Message)
		assert.True(t, retry.IsFatal)
	}

	// A different line means the entry was delivered; the retry state clears.
	events, err := h.ProcessLine(unrelatedLine)
	require.NoError(t, err)
	assert.Nil(t, events)

	// The same evidence line seen again later (detector disarmed, no pending
	// retry) must not emit: it is a stray frame, not a re-delivery.
	events, err = h.ProcessLine(lockupRIPLine)
	require.NoError(t, err)
	assert.Nil(t, events)
}

func TestSoftLockup_ManyRepeatsEmitOncePerCooldown(t *testing.T) {
	h := makeLockupHandler(t)

	current := time.Date(2026, 3, 28, 2, 8, 11, 0, time.UTC)
	h.lockup.now = func() time.Time { return current }

	emitted := 0

	for i := 0; i < 100; i++ {
		header := fmt.Sprintf(
			"watchdog: BUG: soft lockup - CPU#94 stuck for %ds! [swapper/94:0]", 226906+i*26)

		results := feed(t, h, header, lockupCPULine, lockupRIPLine)
		if results[2] != nil {
			emitted++
		}

		current = current.Add(26 * time.Second)
	}

	assert.Equal(t, 2, emitted,
		"100 re-reports over ~43 minutes must yield one event per cooldown period")
}
