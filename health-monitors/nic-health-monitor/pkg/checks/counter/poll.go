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

package counter

import (
	"fmt"
	"log/slog"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// counterPollDeps carries the per-check wiring the shared counter poll
// needs beyond the evaluator itself.
type counterPollDeps struct {
	reader             sysfs.Reader
	cfg                *config.Config
	classifier         *topology.Classifier
	nodeName           string
	checkName          string
	processingStrategy pb.ProcessingStrategy
}

// counterDiscoveryGate applies the shared completeness policy for a
// counter poll. done=true means Prepare must return early with err
// (nil err for the quiet no-op cases).
//
// An incomplete enumeration with committed counter history is a loud
// error: discarding the poll preserves snapshots, latches, and reset
// detection. Without history it is a quiet no-op so nodes without an IB
// tree stay silent.
//
// A partial first read (unreadable devices, no committed history)
// defers the poll — it cannot establish trustworthy snapshots for every
// counter — but only up to checks.FirstPollDeferralLimit consecutive
// polls. Beyond that the poll proceeds with the readable subset: one
// permanently unreadable device must not silently disable counter
// monitoring of every other NIC forever.
func counterDiscoveryGate(
	result *discovery.DiscoveryResult,
	hasState bool,
	deps counterPollDeps,
	deferrals *int,
) (bool, error) {
	if !result.Complete {
		if hasState {
			return true, fmt.Errorf("failed to discover devices: InfiniBand sysfs tree unavailable")
		}

		return true, nil
	}

	if len(result.UnreadableDevices) == 0 || hasState {
		return false, nil
	}

	switch {
	case *deferrals < checks.FirstPollDeferralLimit:
		*deferrals++

		metrics.FirstPollDeferred.WithLabelValues(deps.nodeName, deps.checkName).Inc()
		slog.Warn("Deferring first counter poll: some devices are unreadable",
			"check", deps.checkName,
			"unreadable_devices", len(result.UnreadableDevices),
			"deferrals", *deferrals,
			"limit", checks.FirstPollDeferralLimit)

		return true, nil
	case *deferrals == checks.FirstPollDeferralLimit:
		// Log the transition exactly once; the counter loop polls every
		// second and must not warn forever when no device is readable.
		*deferrals++

		slog.Warn("First-poll deferral limit reached; proceeding with readable devices only",
			"check", deps.checkName,
			"unreadable_devices", len(result.UnreadableDevices))
	}

	return false, nil
}

// prepareCounterPoll runs the shared transactional counter poll:
// discover devices, apply the completeness gate, and evaluate against a
// cloned candidate evaluator. It returns a nil candidate when the gate
// defers the poll; on success the caller stages the candidate as its
// pending state.
//
// Event ordering within the returned batch is load-bearing: the
// check-scoped baseline clear (emitted on the first poll after a host
// reboot) is constructed first and backdated so no downstream consumer
// can apply it after — and thereby wipe — the current-boot events that
// follow it.
func prepareCounterPoll(
	deps counterPollDeps,
	committed *Evaluator,
	firstPollDeferrals *int,
	evaluate func(candidate *Evaluator, devices []discovery.IBDevice) []*pb.HealthEvent,
) (*Evaluator, []*pb.HealthEvent, error) {
	result, err := discovery.DiscoverDevicesWithOverride(
		deps.reader, deps.cfg.NicExclusionRegex, deps.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to discover devices: %w", err)
	}

	if done, gateErr := counterDiscoveryGate(result, committed.HasState(), deps, firstPollDeferrals); done {
		return nil, nil, gateErr
	}

	candidate := committed.Clone()

	// The baseline reconciliation (check-scoped clear + baselines +
	// window-breach replay) waits for the first COMPLETE enumeration:
	// running it while a device is unreadable would wipe that device's
	// previous-boot conditions with nothing able to re-assert them.
	// Until then readable counters are monitored normally (silent
	// seeding via the no-snapshot path) and the baseline stays owed.
	runBaseline := candidate.BootBaselinePending() && len(result.UnreadableDevices) == 0
	candidate.SetBaselineActive(runBaseline)

	var events []*pb.HealthEvent

	if runBaseline {
		events = append(events, checks.NewBaselineClearEvent(
			deps.nodeName, deps.checkName,
			"Host reboot detected: clearing all conditions previously reported by this check",
			deps.processingStrategy,
		))
	}

	events = append(events, evaluate(candidate, result.Devices)...)
	// Sweep before the unobserved replay: latches the sweep consumes
	// (disabled counter, ineligible device) drop to Breached=false and
	// must not be re-asserted.
	events = append(events, candidate.SweepStaleBreaches(
		enabledCounterNames(deps.cfg),
		ineligibleDevices(result.Devices, deps.classifier),
		deps.checkName,
	)...)

	if runBaseline {
		// Re-assert window breaches whose keys were not observable on
		// this poll — the clear wiped their conditions and evaluation
		// could not replay them.
		events = append(events, candidate.ReplayUnobservedBreaches(deps.checkName)...)

		candidate.ClearBootIDFlag()
		// Each event stamps its own time.Now(); guard the clear-first
		// ordering against backward wall-clock steps.
		checks.EnsureClearPrecedesBatch(events)
	}

	return candidate, events, nil
}

// enabledCounterNames returns the counter names currently enabled in
// the configuration; input to the stale-breach sweep.
func enabledCounterNames(cfg *config.Config) map[string]bool {
	out := make(map[string]bool, len(cfg.CounterDetection.Counters))

	for _, c := range cfg.CounterDetection.Counters {
		if c.Enabled {
			out[c.Name] = true
		}
	}

	return out
}

// ineligibleDevices returns the discovered devices that are out of
// monitoring scope (unsupported vendor or management-classified).
// Devices absent from discovery are deliberately not included: absence
// can be transient (e.g., a firmware reset), so their latches must
// survive — see Evaluator.SweepStaleBreaches.
func ineligibleDevices(devices []discovery.IBDevice, classifier *topology.Classifier) map[string]bool {
	out := make(map[string]bool)

	for i := range devices {
		dev := &devices[i]
		if !checks.EligibleDevice(dev, classifier) {
			out[dev.Name] = true
		}
	}

	return out
}
