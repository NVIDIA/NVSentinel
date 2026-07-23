// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package counter implements counter-based health checks for the NIC
// Health Monitor. The Evaluator reads sysfs counters once per poll and
// translates breaches into HealthEvents using the persistent snapshot
// model described in docs/nic-health-monitor/link-counter-detection.md.
//
// Three rules govern when an event is emitted:
//
//  1. Delta thresholds compare against the previous poll's snapshot
//     (snapshot updated every poll); breach == delta > threshold.
//  2. Velocity thresholds hold the snapshot for a fixed window matching
//     the configured velocityUnit (1s / 60s / 3600s) and only evaluate
//     once that window has fully elapsed; breach == rate > threshold.
//     The snapshot is then advanced to the current reading so the next
//     window starts fresh.
//  3. Breach is latching: the breach flag stays set until the counter
//     resets (current < previous) or the host reboots. Subsequent polls
//     while breached emit nothing. Recovery events fire only on counter
//     reset of a previously breached counter.
package counter

import (
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

const (
	thresholdTypeDelta    = "delta"
	thresholdTypeVelocity = "velocity"

	velocityUnitSecond = "second"
	velocityUnitMinute = "minute"
	velocityUnitHour   = "hour"

	netStatisticsPathPrefix = "statistics/"

	// netdevAttrPathPrefix marks counters that live at the netdev root
	// (/sys/class/net/<iface>/<attr>) rather than under statistics/ —
	// e.g. carrier_changes.
	netdevAttrPathPrefix = "netdev/"
)

// isNetCounterPath reports whether a configured counter is read from
// the network-interface tree (either path class) rather than the
// InfiniBand port tree.
func isNetCounterPath(path string) bool {
	return strings.HasPrefix(path, netStatisticsPathPrefix) ||
		strings.HasPrefix(path, netdevAttrPathPrefix)
}

// Evaluator owns the counter evaluation state for one degradation
// check. State is rehydrated from the statefile.Manager at construction
// and flushed back to it at the end of each poll cycle by the caller.
//
// The maps are keyed by `<device>:<port>:<counter_name>` (or
// `net:<iface>:<counter_name>` for interface-level counters such as
// carrier_changes).
type Evaluator struct {
	nodeName           string
	reader             sysfs.Reader
	processingStrategy pb.ProcessingStrategy

	// snapshots is the persisted (value, timestamp) per counter. For
	// delta thresholds it is updated every poll; for velocity thresholds
	// it is held for the duration of a window so rates are computed over
	// real elapsed time.
	snapshots map[string]statefile.CounterSnapshot

	// breachFlags is the persisted latching breach state per counter.
	// Once an entry has Breached=true the counter only emits a recovery
	// event after a counter reset is detected (current < previous).
	breachFlags map[string]statefile.CounterBreachFlag

	// lastPollValues is in-memory only; it lets us detect a counter
	// reset on the very next poll regardless of how stale the snapshot
	// is. Sysfs counters are monotonic, so current < lastPollValue is a
	// definitive reset signal. The map is empty after a pod restart, in
	// which case we fall back to comparing against snapshots[key].Value.
	lastPollValues map[string]uint64

	// ownedKeys tracks the counter keys this evaluator has actually
	// touched during its lifetime. Snapshots() and BreachFlags() filter
	// their returned maps to only these keys so that a sibling
	// evaluator's keys (e.g., the Ethernet check's keys carried in this
	// IB evaluator's map after a pod restart) are NOT written back to
	// the shared state file with stale data. Without this filter the
	// two evaluators would clobber each other's persisted state.
	ownedKeys map[string]struct{}

	// touchedThisPoll tracks the keys evaluateOne actually observed on
	// the current poll. Clone resets it, so it is per-poll by
	// construction. The baseline reconciliation uses it to find breached
	// latches whose keys were NOT observable (absent device, unreadable
	// counter file, vanished netdev) and re-assert them after the
	// check-scoped clear — see ReplayUnobservedBreaches.
	touchedThisPoll map[string]struct{}

	// bootIDChanged signals that a boot-baseline reconciliation is owed:
	// the poll that runs it emits the check-scoped clear plus a healthy
	// baseline event per readable counter. The check layer decides WHEN
	// it runs (only on a complete enumeration — see prepareCounterPoll)
	// via baselineActive, and clears this flag once it has.
	bootIDChanged bool

	// baselineActive gates evaluateOne's baseline behaviour for the
	// current poll. While a baseline is owed but discovery is still
	// partial, the check layer sets this false so readable counters are
	// evaluated (and seeded) normally instead of re-emitting baselines
	// every poll. Defaults to true so a standalone evaluator behaves
	// like the legacy first-poll-after-boot contract.
	baselineActive bool

	// unreadableWarned de-duplicates the enabled-but-unreadable counter
	// warning per key. An unreadable counter is silently skipped by
	// evaluation (transient failures are normal during firmware resets),
	// but a persistent failure must be visible: a wrong sysfs path once
	// kept carrier_changes unmonitored for its entire life without a
	// single log line.
	unreadableWarned map[string]bool
}

// NewEvaluator constructs an Evaluator seeded with snapshots and breach
// flags loaded from the persistent state file. bootIDChanged should be
// true on the first poll after a host reboot so the evaluator emits
// healthy baselines.
func NewEvaluator(
	nodeName string,
	reader sysfs.Reader,
	processingStrategy pb.ProcessingStrategy,
	snapshots map[string]statefile.CounterSnapshot,
	breachFlags map[string]statefile.CounterBreachFlag,
	bootIDChanged bool,
) *Evaluator {
	if snapshots == nil {
		snapshots = make(map[string]statefile.CounterSnapshot)
	}

	if breachFlags == nil {
		breachFlags = make(map[string]statefile.CounterBreachFlag)
	}

	return &Evaluator{
		nodeName:           nodeName,
		reader:             reader,
		processingStrategy: processingStrategy,
		snapshots:          snapshots,
		breachFlags:        breachFlags,
		lastPollValues:     make(map[string]uint64),
		ownedKeys:          make(map[string]struct{}),
		touchedThisPoll:    make(map[string]struct{}),
		bootIDChanged:      bootIDChanged,
		baselineActive:     true,
		unreadableWarned:   make(map[string]bool),
	}
}

// Clone returns an independent candidate evaluator for a transactional poll.
// Reader/configuration dependencies are shared, while every mutable map is
// copied (values are plain value types, so a shallow maps.Clone is a full
// copy) so a failed publication can discard the candidate without advancing
// snapshots, breach latches, reset detection, or boot-baseline state.
func (e *Evaluator) Clone() *Evaluator {
	clone := *e
	clone.snapshots = maps.Clone(e.snapshots)
	clone.breachFlags = maps.Clone(e.breachFlags)
	clone.lastPollValues = maps.Clone(e.lastPollValues)
	clone.ownedKeys = maps.Clone(e.ownedKeys)
	clone.unreadableWarned = maps.Clone(e.unreadableWarned)
	// Deliberately fresh, not cloned: the touched set is per-poll and a
	// candidate is created once per poll.
	clone.touchedThisPoll = make(map[string]struct{})

	return &clone
}

// HasState reports whether an evaluator has committed counter history. It is
// used to distinguish a node that genuinely has no InfiniBand sysfs tree from
// an incomplete enumeration that would otherwise discard existing state.
func (e *Evaluator) HasState() bool {
	return len(e.snapshots) > 0 || len(e.breachFlags) > 0 || len(e.lastPollValues) > 0
}

// Snapshots returns a copy of this evaluator's owned snapshots so the
// caller can persist them via statefile.Manager.UpdateCounterSnapshots.
// Only keys this evaluator has actually evaluated (via evaluateOne) are
// returned; cross-evaluator keys carried in the in-memory map (loaded
// from the shared state file at construction) are filtered out so the
// two checks cannot overwrite each other's persisted entries.
func (e *Evaluator) Snapshots() map[string]statefile.CounterSnapshot {
	out := make(map[string]statefile.CounterSnapshot, len(e.ownedKeys))

	for k := range e.ownedKeys {
		if v, ok := e.snapshots[k]; ok {
			out[k] = v
		}
	}

	return out
}

// BreachFlags returns a copy of this evaluator's owned breach flags.
// Keys explicitly cleared during this lifetime are returned with
// Breached=false so that statefile.Manager.UpdateBreachFlags can
// propagate the deletion to the persisted map (the merge logic deletes
// any incoming Breached=false entry).
func (e *Evaluator) BreachFlags() map[string]statefile.CounterBreachFlag {
	out := make(map[string]statefile.CounterBreachFlag, len(e.ownedKeys))

	for k := range e.ownedKeys {
		if v, ok := e.breachFlags[k]; ok {
			out[k] = v
		} else {
			// Owned but not currently breached — explicit Breached=false
			// signals the manager to delete any stale persisted entry.
			out[k] = statefile.CounterBreachFlag{Breached: false}
		}
	}

	return out
}

// EvaluateCounters reads each enabled IB-tree counter for the given port.
// Counters with a statistics/ path prefix are skipped (handled by
// EvaluateNetCounters).
func (e *Evaluator) EvaluateCounters(
	dev *discovery.IBDevice,
	port *discovery.IBPort,
	counters []config.CounterConfig,
	checkName string,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	now := time.Now()

	for _, counterCfg := range counters {
		if !counterCfg.Enabled || isNetCounterPath(counterCfg.Path) {
			continue
		}

		key := portCounterKey(port.Device, port.Port, counterCfg.Name)

		currentValue, err := e.reader.ReadIBPortCounter(port.Device, port.Port, counterCfg.Path)
		if err != nil {
			e.warnUnreadableOnce(key, counterCfg.Path, err)
			continue
		}

		entities := checks.PortEntities(port.Device, port.Port)

		if event := e.evaluateOne(key, currentValue, now, counterCfg, entities, checkName); event != nil {
			events = append(events, event)
		}
	}

	return events
}

// EvaluateNetCounters reads counters whose path starts with "statistics/"
// from /sys/class/net/<iface>/statistics/. The counter file is derived
// from the configured path (e.g., "statistics/carrier_changes" reads
// "carrier_changes").
func (e *Evaluator) EvaluateNetCounters(
	dev *discovery.IBDevice,
	port *discovery.IBPort,
	counters []config.CounterConfig,
	checkName string,
) []*pb.HealthEvent {
	if dev.NetDev == "" {
		return nil
	}

	var events []*pb.HealthEvent

	now := time.Now()

	for _, counterCfg := range counters {
		if !counterCfg.Enabled || !isNetCounterPath(counterCfg.Path) {
			continue
		}

		key := netCounterKey(dev.NetDev, counterCfg.Name)

		currentValue, err := e.readNetCounter(dev.NetDev, counterCfg.Path)
		if err != nil {
			e.warnUnreadableOnce(key, counterCfg.Path, err)
			continue
		}

		entities := checks.PortEntities(port.Device, port.Port)

		if event := e.evaluateOne(key, currentValue, now, counterCfg, entities, checkName); event != nil {
			events = append(events, event)
		}
	}

	return events
}

// evaluateOne applies the snapshot/threshold/breach state machine for a
// single counter and returns an event if a health boundary was crossed.
// All snapshot, breach-flag, and last-poll-value mutations happen here.
func (e *Evaluator) evaluateOne(
	key string,
	currentValue uint64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	// Mark this key as owned by this evaluator. Snapshots() and
	// BreachFlags() filter on this so cross-evaluator keys carried in
	// the in-memory map (loaded from the shared state file at startup)
	// aren't written back when this evaluator persists.
	e.ownedKeys[key] = struct{}{}
	e.touchedThisPoll[key] = struct{}{}

	// Boot baseline takes precedence over everything else when the check
	// layer has activated it for this poll — see baselineOne.
	if e.bootIDChanged && e.baselineActive {
		return e.baselineOne(key, currentValue, now, counterCfg, entities, checkName)
	}

	defer func() { e.lastPollValues[key] = currentValue }()

	prev, hasSnapshot := e.snapshots[key]
	if !hasSnapshot {
		// First time seeing this counter. Seed snapshot only.
		e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

		return nil
	}

	if e.isReset(key, currentValue, prev.Value) {
		return e.handleReset(key, currentValue, now, counterCfg, entities, checkName)
	}

	// Latching breach: while we are already breached, suppress all
	// further events. The snapshot is left untouched so that the same
	// reset detection works whenever the admin clears the counter; a
	// post-reset window starts cleanly from the new baseline.
	if e.isBreached(key) {
		return nil
	}

	switch counterCfg.ThresholdType {
	case thresholdTypeDelta:
		return e.evaluateDelta(key, currentValue, prev, now, counterCfg, entities, checkName)
	case thresholdTypeVelocity:
		return e.evaluateVelocity(key, currentValue, prev, now, counterCfg, entities, checkName)
	default:
		slog.Warn("Unknown threshold type",
			"type", counterCfg.ThresholdType, "counter", counterCfg.Name)

		return nil
	}
}

// baselineOne handles one counter on the baseline reconciliation poll:
// seed the snapshot, then either re-assert a breach latched during the
// pre-baseline window (the check-scoped clear just wiped its downstream
// condition) or emit the healthy baseline. The bootIDChanged flag is
// not cleared per-counter — it is cleared at the end of the baseline
// poll by the degradation check (see ClearBootIDFlag).
func (e *Evaluator) baselineOne(
	key string,
	currentValue uint64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	// Detect an administrative counter reset BEFORE overwriting the
	// window's observations: a value first seen below the previous one
	// on this very poll means the counter recovered, and replaying its
	// window breach would publish a false condition right after the
	// clear — while destroying the only evidence of the reset.
	flag, hasFlag := e.breachFlags[key]
	windowBreached := hasFlag && flag.Breached
	resetObserved := false

	if windowBreached {
		if prev, hasSnapshot := e.snapshots[key]; hasSnapshot {
			resetObserved = e.isReset(key, currentValue, prev.Value)
		}
	}

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}
	e.lastPollValues[key] = currentValue

	if windowBreached && !resetObserved {
		return e.replayBreach(flag, counterCfg, entities, checkName)
	}

	// Explicit Breached=false so the manager deletes any stale persisted
	// breach flag — from the previous boot, or from a window breach
	// whose counter was reset before this reconciliation poll.
	e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

	return e.baselineEvent(counterCfg, entities, checkName)
}

// evaluateDelta evaluates a delta-typed counter. Snapshot is advanced
// every poll regardless of whether a breach was detected so the next
// poll's delta is measured against the latest reading.
func (e *Evaluator) evaluateDelta(
	key string,
	currentValue uint64,
	prev statefile.CounterSnapshot,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	delta := currentValue - prev.Value

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if float64(delta) <= counterCfg.Threshold {
		return nil
	}

	return e.recordBreach(key, currentValue, delta, 0, now, counterCfg, entities, checkName)
}

// evaluateVelocity evaluates a velocity-typed counter. The snapshot is
// held for the configured window so that the rate is computed over a
// real, fully elapsed window rather than a partial sample. Once the
// window has elapsed (and only then) the snapshot is advanced to the
// current reading so the next window measures fresh data.
func (e *Evaluator) evaluateVelocity(
	key string,
	currentValue uint64,
	prev statefile.CounterSnapshot,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	elapsed := now.Sub(prev.Timestamp)
	window := windowDuration(counterCfg.VelocityUnit)

	if elapsed < window {
		return nil
	}

	delta := currentValue - prev.Value
	rate := computeRate(delta, elapsed, counterCfg.VelocityUnit)

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if rate <= counterCfg.Threshold {
		return nil
	}

	return e.recordBreach(key, currentValue, delta, rate, now, counterCfg, entities, checkName)
}

// recordBreach builds the unhealthy event, sets the breach flag, and
// bumps the breach-counter metric. Caller must have already updated the
// snapshot.
func (e *Evaluator) recordBreach(
	key string,
	currentValue uint64,
	delta uint64,
	rate float64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	// Fatal and non-fatal counter conditions deliberately retain the
	// degradation check's identity. Reusing the state check name causes an
	// ACTIVE/LinkUp state recovery to clear a counter-originated condition in
	// fault-quarantine even though the counter latch remains breached.
	effectiveCheckName := checkName

	device, port := devicePortFromEntities(entities)

	metrics.CounterThresholdBreaches.WithLabelValues(
		e.nodeName,
		counterCfg.Name,
		device,
		port,
		fmt.Sprintf("%t", counterCfg.IsFatal),
	).Inc()

	// Device/Port capture the event's entities so reconciliation can
	// re-assert this breach even if the key becomes unreadable.
	e.breachFlags[key] = statefile.CounterBreachFlag{
		Breached:  true,
		CheckName: effectiveCheckName,
		IsFatal:   counterCfg.IsFatal,
		Since:     now,
		Device:    device,
		Port:      port,
	}

	rateUnit := "interval"

	if counterCfg.ThresholdType == thresholdTypeVelocity {
		switch counterCfg.VelocityUnit {
		case velocityUnitSecond:
			rateUnit = "sec"
		case velocityUnitMinute:
			rateUnit = "min"
		case velocityUnitHour:
			rateUnit = "hour"
		default:
			rateUnit = counterCfg.VelocityUnit
		}
	}

	message := fmt.Sprintf(
		"Port %s port %s: %s - %s (value=%d, delta=%d, rate=%.2f/%s)",
		device, port, counterCfg.Name, counterCfg.Description,
		currentValue, delta, rate, rateUnit,
	)

	return withCounterCode(checks.NewHealthEvent(
		e.nodeName, effectiveCheckName, message,
		entities,
		counterCfg.IsFatal, false,
		ResolveAction(counterCfg.IsFatal),
		e.processingStrategy,
	), counterCfg.Name)
}

// handleReset clears the persisted breach flag (if any), advances the
// snapshot to the current reading, and emits a recovery event when the
// reset cleared a previously breached counter.
func (e *Evaluator) handleReset(
	key string,
	currentValue uint64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	flag, wasBreached := e.breachFlags[key]

	e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if !wasBreached || !flag.Breached {
		return nil
	}

	device, port := devicePortFromEntities(entities)
	message := fmt.Sprintf("Counter %s recovered on port %s port %s", counterCfg.Name, device, port)

	// Recovery uses the same CheckName as the original breach so the
	// platform clears the same condition. RecommendedAction on a
	// recovery is always NONE per design.
	recoveryCheckName := flag.CheckName
	if recoveryCheckName == "" {
		recoveryCheckName = checkName
		if counterCfg.IsFatal {
			recoveryCheckName = fatalCheckName(checkName)
		}
	}

	return withCounterCode(checks.NewHealthEvent(
		e.nodeName, recoveryCheckName, message,
		entities,
		false, true,
		pb.RecommendedAction_NONE,
		e.processingStrategy,
	), counterCfg.Name)
}

// baselineEvent constructs the IsHealthy=true event emitted on the
// first poll after a host reboot for every configured counter. Its
// purpose is to clear stale FATAL conditions left on the platform by
// the previous boot — see docs Section 6.5.
func (e *Evaluator) baselineEvent(
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	effectiveCheckName := checkName

	device, port := devicePortFromEntities(entities)
	message := fmt.Sprintf("Counter %s healthy after reboot on port %s port %s",
		counterCfg.Name, device, port)

	return withCounterCode(checks.NewHealthEvent(
		e.nodeName, effectiveCheckName, message,
		entities,
		false, true,
		pb.RecommendedAction_NONE,
		e.processingStrategy,
	), counterCfg.Name)
}

// ClearBootIDFlag is called by the degradation check after the first
// poll completes so subsequent polls revert to normal evaluation.
func (e *Evaluator) ClearBootIDFlag() {
	e.bootIDChanged = false
}

// BootBaselinePending reports whether the boot-baseline reconciliation
// — the poll that emits the check-scoped clear plus per-counter healthy
// baselines — has not run yet.
func (e *Evaluator) BootBaselinePending() bool {
	return e.bootIDChanged
}

// SetBaselineActive tells the evaluator whether the current poll is the
// baseline reconciliation poll. While a baseline is owed but discovery
// is partial, the check layer passes false so readable counters are
// monitored normally in the meantime.
func (e *Evaluator) SetBaselineActive(active bool) {
	e.baselineActive = active
}

// replayBreach re-asserts a breach that latched during the pre-baseline
// window. The check-scoped clear emitted at the start of the baseline
// batch wiped its downstream condition; without this replay the fault
// would silently vanish while the counter stays latched locally. The
// flag is kept — recovery still requires a counter reset.
func (e *Evaluator) replayBreach(
	flag statefile.CounterBreachFlag,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	effectiveCheckName := flag.CheckName
	if effectiveCheckName == "" {
		effectiveCheckName = checkName
	}

	device, port := devicePortFromEntities(entities)
	message := fmt.Sprintf(
		"Port %s port %s: %s - %s (re-asserting outstanding breach after baseline reconciliation)",
		device, port, counterCfg.Name, counterCfg.Description)

	slog.Info("Baseline run: re-asserting window-latched counter breach",
		"counter", counterCfg.Name, "device", device, "port", port,
		"check", effectiveCheckName, "is_fatal", flag.IsFatal)

	return withCounterCode(checks.NewHealthEvent(
		e.nodeName, effectiveCheckName, message,
		entities,
		flag.IsFatal, false,
		ResolveAction(flag.IsFatal),
		e.processingStrategy,
	), counterCfg.Name)
}

// SweepStaleBreaches emits recovery events for latched breaches that
// can never recover through evaluation again: their counter is no
// longer enabled in the configuration, or their device is discovered
// but no longer eligible (e.g., classified as a management NIC). Without
// this, such a latch would hold its downstream FATAL condition forever —
// the normal recovery path only fires from a counter that is actually
// read.
//
// Keys whose device is merely absent from this poll's discovery are NOT
// swept: absence can be transient (firmware reset), and the next
// boot-ID change clears those conditions via the baseline clear. Flags
// recorded by the sibling check (different CheckName) are left for that
// check's own sweep. Entity identity comes from the flag itself
// (recorded at breach time) with a fallback to parsing port-format
// keys; a flag with no resolvable identity is left for the baseline
// reconciliation, which consumes it.
func (e *Evaluator) SweepStaleBreaches(
	enabledCounters map[string]bool,
	ineligibleDevices map[string]bool,
	checkName string,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for key, flag := range e.breachFlags {
		if !flag.Breached || flag.CheckName != checkName {
			continue
		}

		entities, device, counterName := breachIdentity(key, flag)
		if entities == nil {
			continue
		}

		if enabledCounters[counterName] && !ineligibleDevices[device] {
			continue
		}

		e.ownedKeys[key] = struct{}{}
		e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

		slog.Info("Clearing stale counter breach: key is no longer monitored",
			"key", key, "check", checkName,
			"counter_enabled", enabledCounters[counterName],
			"device_ineligible", ineligibleDevices[device])

		message := fmt.Sprintf(
			"Counter %s on %s is no longer monitored "+
				"(counter disabled or device out of monitoring scope); clearing stale breach",
			counterName, device)

		events = append(events, withCounterCode(checks.NewHealthEvent(
			e.nodeName, checkName, message,
			entities,
			false, true,
			pb.RecommendedAction_NONE,
			e.processingStrategy,
		), counterName))
	}

	return events
}

// ReplayUnobservedBreaches re-asserts current-boot breach latches whose
// keys were NOT observed on the reconciliation poll — device absent
// from enumeration, counter file unreadable, netdev gone. The
// check-scoped clear just wiped their downstream conditions; without
// this replay the local latch would stay set while downstream shows
// healthy, and isBreached would silently suppress every future breach
// on the key. Latches the sweep already consumed this poll have
// Breached=false and are skipped. A flag with no resolvable identity
// (legacy, pre-identity schema) is consumed instead, so local state can
// never desynchronize from the downstream clear.
func (e *Evaluator) ReplayUnobservedBreaches(checkName string) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for key, flag := range e.breachFlags {
		if !flag.Breached || flag.CheckName != checkName {
			continue
		}

		if _, touched := e.touchedThisPoll[key]; touched {
			continue
		}

		entities, device, counterName := breachIdentity(key, flag)
		if entities == nil {
			e.ownedKeys[key] = struct{}{}
			e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

			slog.Warn("Consuming window breach with no recorded identity: "+
				"cannot re-assert it after the baseline clear",
				"key", key, "check", checkName)

			continue
		}

		slog.Info("Baseline run: re-asserting window breach whose counter is not observable",
			"key", key, "check", checkName, "device", device, "is_fatal", flag.IsFatal)

		message := fmt.Sprintf(
			"Counter %s on %s is not currently readable; "+
				"re-asserting outstanding breach after baseline reconciliation",
			counterName, device)

		events = append(events, withCounterCode(checks.NewHealthEvent(
			e.nodeName, checkName, message,
			entities,
			flag.IsFatal, false,
			ResolveAction(flag.IsFatal),
			e.processingStrategy,
		), counterName))
	}

	return events
}

// breachIdentity resolves a breach flag's event entities: preferably
// from the identity recorded at breach time, otherwise by parsing a
// port-format key. Interface-level keys without recorded identity are
// unresolvable (nil entities).
func breachIdentity(key string, flag statefile.CounterBreachFlag) (entities []*pb.Entity, device, counter string) {
	counter = key
	if idx := strings.LastIndex(key, ":"); idx >= 0 {
		counter = key[idx+1:]
	}

	if flag.Device != "" && flag.Port != "" {
		if p, err := strconv.Atoi(flag.Port); err == nil {
			return checks.PortEntities(flag.Device, p), flag.Device, counter
		}
	}

	if device, port, counterName, ok := parsePortCounterKey(key); ok {
		return checks.PortEntities(device, port), device, counterName
	}

	return nil, "", counter
}

// parsePortCounterKey splits a `<device>:<port>:<counter>` key. It
// reports ok=false for interface-level keys ("net:<iface>:<counter>")
// and anything else it cannot parse.
func parsePortCounterKey(key string) (device string, port int, counter string, ok bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] == "net" {
		return "", 0, "", false
	}

	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, "", false
	}

	return parts[0], port, parts[2], true
}

// isReset returns true when the latest reading is strictly less than
// the previous poll's value (in-memory) or — if no in-memory value yet
// exists, e.g. the first poll after a pod restart — strictly less than
// the persisted snapshot value.
func (e *Evaluator) isReset(key string, currentValue, snapshotValue uint64) bool {
	if last, ok := e.lastPollValues[key]; ok {
		return currentValue < last
	}

	return currentValue < snapshotValue
}

// isBreached reports whether the persisted breach flag for this key is
// currently set.
func (e *Evaluator) isBreached(key string) bool {
	flag, ok := e.breachFlags[key]
	return ok && flag.Breached
}

// fatalCheckName preserves the legacy recovery identity for old persisted
// breach flags that predate fatal-counter identity separation. New breach and
// baseline events keep their degradation check name; handleReset uses this
// mapping only when a legacy flag has no stored CheckName.
func fatalCheckName(checkName string) string {
	switch checkName {
	case checks.InfiniBandDegradationCheckName:
		return checks.InfiniBandStateCheckName
	case checks.EthernetDegradationCheckName:
		return checks.EthernetStateCheckName
	default:
		return checkName
	}
}

// windowDuration returns the time window over which a velocity counter
// must accumulate samples before it is evaluated. It mirrors the
// configured velocityUnit one-to-one so a `120/hour` threshold actually
// observes a one-hour window rather than extrapolating from a 1s
// sample.
func windowDuration(unit string) time.Duration {
	switch unit {
	case velocityUnitSecond:
		return time.Second
	case velocityUnitMinute:
		return time.Minute
	case velocityUnitHour:
		return time.Hour
	default:
		return time.Second
	}
}

// computeRate converts a delta and elapsed duration into a rate
// expressed in the configured velocity unit (events per unit).
func computeRate(delta uint64, elapsed time.Duration, unit string) float64 {
	if elapsed <= 0 {
		return 0
	}

	switch unit {
	case velocityUnitSecond:
		return float64(delta) / elapsed.Seconds()
	case velocityUnitMinute:
		return float64(delta) / elapsed.Minutes()
	case velocityUnitHour:
		return float64(delta) / elapsed.Hours()
	default:
		return float64(delta) / elapsed.Seconds()
	}
}

// CalculateDelta is retained for compatibility with existing tests
// (used outside the evaluator). It applies the same monotonic-counter
// rule used inside evaluateOne: a current value below the previous
// value is treated as a reset and the entire current value is returned
// as the delta.
func CalculateDelta(current, previous uint64) uint64 {
	if current < previous {
		return current
	}

	return current - previous
}

// EvaluateThreshold is retained for compatibility with existing tests.
// It evaluates a single observation against the configured threshold
// using the same comparison the evaluator does internally.
func EvaluateThreshold(cfg config.CounterConfig, delta uint64, elapsed time.Duration) bool {
	switch cfg.ThresholdType {
	case thresholdTypeDelta:
		return float64(delta) > cfg.Threshold
	case thresholdTypeVelocity:
		return computeRate(delta, elapsed, cfg.VelocityUnit) > cfg.Threshold
	default:
		return false
	}
}

// ComputeRate is the exported form of computeRate for tests.
func ComputeRate(delta uint64, elapsed time.Duration, unit string) float64 {
	return computeRate(delta, elapsed, unit)
}

// ResolveAction derives the recommended action from the isFatal flag.
func ResolveAction(isFatal bool) pb.RecommendedAction {
	if isFatal {
		return pb.RecommendedAction_REPLACE_VM
	}

	return pb.RecommendedAction_NONE
}

func portCounterKey(device string, port int, counterName string) string {
	return fmt.Sprintf("%s:%d:%s", device, port, counterName)
}

func netCounterKey(iface, counterName string) string {
	return fmt.Sprintf("net:%s:%s", iface, counterName)
}

// readNetCounter routes a net-tree counter read to the right sysfs
// location for its path class: statistics/ entries live under
// /sys/class/net/<iface>/statistics/, netdev/ entries at the netdev
// root (e.g. carrier_changes, which sits beside operstate).
func (e *Evaluator) readNetCounter(iface, path string) (uint64, error) {
	if strings.HasPrefix(path, netStatisticsPathPrefix) {
		statFile := strings.TrimPrefix(path, netStatisticsPathPrefix)
		if statFile == "" {
			return 0, fmt.Errorf("empty statistics counter path %q", path)
		}

		return e.reader.ReadNetStatistic(iface, statFile)
	}

	attr := strings.TrimPrefix(path, netdevAttrPathPrefix)
	if attr == "" {
		return 0, fmt.Errorf("empty netdev attribute path %q", path)
	}

	return e.reader.ReadNetAttribute(iface, attr)
}

// warnUnreadableOnce surfaces an enabled counter whose sysfs source
// cannot be read. Evaluation still skips it (transient read failures
// are normal during firmware resets), but persistent unreadability must
// be visible in the logs rather than silently unmonitored.
func (e *Evaluator) warnUnreadableOnce(key, path string, err error) {
	if e.unreadableWarned[key] {
		return
	}

	e.unreadableWarned[key] = true

	slog.Warn("Enabled counter is not readable; it will not be monitored until the read succeeds",
		"key", key, "path", path, "error", err)
}

// withCounterCode stamps the per-counter identity onto an event.
// Downstream consumers scope condition clearing by ErrorCode: without
// it, every counter on a port shares one identity (check name + NIC +
// NICPort) and a recovery for one counter clears every sibling
// counter's condition — while the siblings' local latches stay set and
// suppress re-publication. Every counter event type must carry the same
// code (the counter name); only the check-scoped baseline clear is
// deliberately code-less, because it means "clear everything".
func withCounterCode(evt *pb.HealthEvent, counterName string) *pb.HealthEvent {
	evt.ErrorCode = []string{counterName}
	return evt
}

// devicePortFromEntities extracts the NIC device name and port string
// from a HealthEvent entity slice produced by checks.PortEntities. It
// returns empty strings if the slice does not contain the expected
// shape — the caller falls back to those for log/metric labels.
func devicePortFromEntities(entities []*pb.Entity) (string, string) {
	var device, port string

	for _, e := range entities {
		switch e.EntityType {
		case checks.EntityTypeNIC:
			device = e.EntityValue
		case checks.EntityTypePort:
			port = e.EntityValue
		}
	}

	return device, port
}
