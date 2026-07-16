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

package state

import (
	"fmt"
	"log/slog"
	"maps"
	"strings"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// linkLayerStrategy defines the per-check hooks that differ between
// InfiniBand and Ethernet state checks. The baseStateCheck delegates to
// these methods wherever the two checks diverge.
type linkLayerStrategy interface {
	checkName() string
	linkLayer() string
	isTargetPort(port *discovery.IBPort) bool
	formatDeviceDisappearance(device string) string
	formatPortDisappearance(device string, port int) string
}

// baseStateCheck holds the fields and methods shared by both
// InfiniBandStateCheck and EthernetStateCheck. Each concrete check
// embeds this struct and sets the strategy to itself so the shared
// methods can call back into per-check hooks.
type baseStateCheck struct {
	nodeName           string
	reader             sysfs.Reader
	cfg                *config.Config
	processingStrategy pb.ProcessingStrategy
	classifier         *topology.Classifier

	state                *statefile.Manager
	emitHealthyBaselines bool

	previousDevices map[string]bool
	previousPorts   map[string]portSnapshot

	// anomalousLatch is the set of cards currently reported anomalous
	// via a FATAL card-homogeneity event. Latched cards stay silent
	// until they recover (present + decisive group + at/above mode),
	// which emits the matching card-healthy event. Persisted to the
	// state file — where it survives pod restarts, reboots, and
	// discovery-scope changes — so nothing can orphan a card entity
	// held by fault-quarantine.
	anomalousLatch map[string]bool

	// disappearedLatch tracks device-level FATALs that still need a healthy
	// re-enumeration event. deviceMissCounts debounces confirmed enumeration
	// absences before creating such a FATAL.
	disappearedLatch map[string]bool
	deviceMissCounts map[string]int

	// exemptionLogged de-duplicates the informational management-sibling
	// exemption log: the classification is static for a process lifetime,
	// so each card is logged once rather than every poll. Logging state
	// only — deliberately outside the transactional commit.
	exemptionLogged map[string]bool

	// saveFailed records that the last state-file Save failed, so the
	// next committed poll retries even when nothing changed. Persistence
	// bookkeeping only — deliberately outside the transactional commit.
	saveFailed bool

	pending *statePollCommit

	strategy linkLayerStrategy
}

const deviceMissThreshold = 3

type statePollCommit struct {
	devices          map[string]bool
	ports            map[string]portSnapshot
	anomalousLatch   map[string]bool
	disappearedLatch map[string]bool
	deviceMissCounts map[string]int
	linkLayer        string
}

// seedFromPersistedState pre-populates previousPorts and previousDevices
// from the persisted state file so the first Run after a pod restart
// behaves like a subsequent poll (recovery events for ports that
// transitioned while the pod was down). Does nothing when the state
// file reports a boot ID change or when the file is empty — in those
// cases the check falls back to the first-poll code paths.
func (b *baseStateCheck) seedFromPersistedState() {
	// The card-anomaly latch is seeded unconditionally: it survives
	// boot-ID and scope resets in the state file (an outstanding card
	// FATAL downstream doesn't stop being outstanding because the node
	// rebooted or the discovery scope changed), so the recovery event
	// can be emitted whenever positive evidence finally arrives.
	b.anomalousLatch = make(map[string]bool)
	for card := range b.state.AnomalousCardsFor(b.strategy.linkLayer()) {
		b.anomalousLatch[card] = true
	}

	b.disappearedLatch = make(map[string]bool)
	for device := range b.state.DisappearedDevicesFor(b.strategy.linkLayer()) {
		b.disappearedLatch[device] = true
	}

	b.deviceMissCounts = b.state.DeviceMissCountsFor(b.strategy.linkLayer())
	b.exemptionLogged = make(map[string]bool)

	if b.emitHealthyBaselines {
		return
	}

	persisted := b.state.PortStatesFor(b.strategy.linkLayer())
	if len(persisted) == 0 {
		return
	}

	b.previousPorts = make(map[string]portSnapshot, len(persisted))
	for k, v := range persisted {
		b.previousPorts[k] = portSnapshot{
			Device:        v.Device,
			Port:          v.Port,
			State:         v.State,
			PhysicalState: v.PhysicalState,
		}
	}

	b.previousDevices = make(map[string]bool)
	for _, v := range persisted {
		b.previousDevices[v.Device] = true
	}

	slog.Info("Seeded state check from persisted state",
		"linkLayer", b.strategy.linkLayer(),
		"port_states", len(b.previousPorts),
		"known_devices", len(b.previousDevices),
	)
}

// portIsHealthy reports whether a port snapshot is fully operational.
// Both link layers share the same healthy definition.
func portIsHealthy(snap portSnapshot) bool {
	return snap.State == checks.IBStateActive && snap.PhysicalState == checks.IBPhysLinkUp
}

// pollAggregates carries the link-layer-agnostic slice of one poll that
// the shared event-assembly pipeline consumes.
type pollAggregates struct {
	seenDevices     map[string]bool
	parsedDevices   map[string]bool
	currentDevices  map[string]bool
	currentPorts    map[string]portSnapshot
	cardActive      map[string]int
	cardTotal       map[string]int
	cardRole        map[string]topology.Role
	managementCards map[string]bool
	uncertain       bool
}

// buildEvents runs the shared per-poll event pipeline: disappearance
// handling first (it may retain held devices and adds to discovery
// uncertainty), then the concrete check's per-port transitions, then
// port/device lifecycle recoveries and the card-homogeneity lifecycle.
//
// Card homogeneity is evaluated every poll: anomalies feed the
// first-poll per-port severity decision and drive the card-anomaly
// latch (FATAL on onset, card-healthy on recovery — see
// cardHomogeneityEvents). It is skipped while the inclusion override is
// active (peer-group statistics carry no signal over a pinned set, see
// overrideActive) and while discovery is uncertain (partial reads or
// held misses would make present cards look anomalous).
func (b *baseStateCheck) buildEvents(
	agg pollAggregates,
	baselineRun bool,
	portEvents func(anomalousCards map[string]topology.CardAnomaly) []*pb.HealthEvent,
) []*pb.HealthEvent {
	disappearanceEvents, heldMissingState := b.detectDeviceDisappearance(
		agg.seenDevices, agg.currentDevices, agg.currentPorts)
	discoveryUncertain := agg.uncertain || heldMissingState

	b.exemptManagementSiblingCards(agg)

	var anomalousCards map[string]topology.CardAnomaly

	var evaluatedCards map[string]int

	if !b.overrideActive() && !discoveryUncertain {
		anomalousCards, evaluatedCards = b.classifier.EvaluateCardHomogeneity(
			agg.cardActive, agg.cardTotal, agg.cardRole)
	}

	// Snapshot the disappearance latch before the port pass so devices it
	// consumes (via consumeDisappearanceRecovery) can be diffed afterwards
	// and given a device-scoped recovery. Latches added above by
	// detectDeviceDisappearance survive the pass and never appear in the
	// diff; latches consumed later by consumeReenumeratedDisappearances
	// emit their own device event and are still present at diff time.
	latchedBefore := maps.Clone(b.disappearedLatch)

	events := portEvents(anomalousCards)
	events = append(events, disappearanceEvents...)
	events = append(events, b.detectPortDisappearance(agg.currentDevices, agg.currentPorts)...)
	events = append(events, b.deviceDisappearanceRecoveries(latchedBefore)...)
	events = append(events, b.consumeReenumeratedDisappearances(agg.parsedDevices, agg.currentDevices)...)
	events = append(events, b.consumeExemptCardLatches(agg.managementCards)...)

	if !b.overrideActive() && !discoveryUncertain {
		events = append(events, b.cardHomogeneityEvents(
			agg.cardActive, agg.cardRole, anomalousCards, evaluatedCards, baselineRun)...)
	}

	return events
}

// exemptManagementSiblingCards removes cards that have a management-
// classified sibling function from the homogeneity inputs. Such a card
// is frontend plumbing: its active member is the excluded management
// NIC itself, so peer comparison would misread the remaining (often
// intentionally uncabled) function as a failure — with the active
// sibling excluded from counting, the card shows 0 active ports against
// a decisive peer mode (observed on OCI BM.GPU.L40S.4: eth0 excluded as
// the default-route NIC left card 0000:5a:00 counting 0 < mode 1,
// fataling its uncabled aux port on every node of the SKU). Exempt
// cards keep per-port runtime transition detection; they only opt out
// of peer-evidence verdicts. VF siblings do not exempt a card — they
// are skipped by discovery before classification and carry no frontend
// signal.
func (b *baseStateCheck) exemptManagementSiblingCards(agg pollAggregates) {
	for card := range agg.managementCards {
		if _, tracked := agg.cardTotal[card]; !tracked {
			continue
		}

		if !b.exemptionLogged[card] {
			b.exemptionLogged[card] = true

			slog.Info("Exempting card from peer comparison: sibling function is a management NIC",
				"card", card, "linkLayer", b.strategy.linkLayer())
		}

		delete(agg.cardActive, card)
		delete(agg.cardTotal, card)
		delete(agg.cardRole, card)
	}
}

// consumeExemptCardLatches clears outstanding card-anomaly latches for
// cards that are exempt from peer comparison (management sibling).
// Exempt cards are never evaluated again, so a held latch could never
// recover through cardHomogeneityEvents; the card-healthy event keeps
// the downstream FATAL from being orphaned. This also self-heals nodes
// latched before the exemption existed. Runs on the transactional
// candidate latch, so the clear commits only after publication.
func (b *baseStateCheck) consumeExemptCardLatches(managementCards map[string]bool) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for card := range b.anomalousLatch {
		if !managementCards[card] {
			continue
		}

		delete(b.anomalousLatch, card)

		slog.Info("Clearing card-anomaly latch: card is exempt from peer comparison",
			"card", card, "linkLayer", b.strategy.linkLayer())

		events = append(events, checks.NewHealthEvent(
			b.nodeName, b.strategy.checkName(),
			fmt.Sprintf("Card %s exempt from peer comparison (management NIC sibling); clearing anomaly", card),
			checks.DeviceEntities(card),
			false, true, pb.RecommendedAction_NONE, b.processingStrategy,
		))
	}

	return events
}

// consumeDisappearanceRecovery clears and reports a latched device
// disappearance. It is called only while evaluating a healthy port event; the
// transactional candidate maps ensure the clear is committed after publish.
func (b *baseStateCheck) consumeDisappearanceRecovery(device string) bool {
	if !b.disappearedLatch[device] {
		return false
	}

	delete(b.disappearedLatch, device)

	return true
}

// deviceDisappearanceRecoveries emits a device-level healthy event for
// every disappearance latch the current poll's port evaluation consumed.
// The disappearance FATAL is device-scoped (NIC entity only), so its
// recovery must carry the identical entity set: consumers that require
// all of a healthy event's entities to match a stored condition entry
// (platform-connector's node conditions since PR #1468) can never clear
// the device entry from the port-scoped healthy alone. The port healthy
// emitted by the transition path still clears any port-scoped
// conditions; this event clears the device-scoped one.
func (b *baseStateCheck) deviceDisappearanceRecoveries(latchedBefore map[string]bool) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for device := range latchedBefore {
		if b.disappearedLatch[device] {
			continue // still latched: not consumed by this poll's port pass
		}

		slog.Info("Device recovered from disappearance; emitting device-scoped recovery",
			"device", device, "linkLayer", b.strategy.linkLayer())

		events = append(events, checks.NewHealthEvent(
			b.nodeName, b.strategy.checkName(),
			fmt.Sprintf("Device %s re-enumerated in sysfs", device),
			checks.DeviceEntities(device),
			false, true, pb.RecommendedAction_NONE, b.processingStrategy,
		))
	}

	return events
}

// consumeReenumeratedDisappearances clears disappearance latches for
// devices that were fully discovered again but expose no port of this
// check's link layer, emitting a device-level healthy event so the
// outstanding FATAL downstream is not orphaned. The per-port recovery
// path cannot reach such devices — they never produce a healthy port of
// this layer. This covers a device whose ports were reflashed to the
// sibling layer while it was absent, and latches created before device
// tracking became layer-scoped. Absent or unreadable devices are not in
// parsedDevices and hold the latch: no evidence, no verdict.
func (b *baseStateCheck) consumeReenumeratedDisappearances(
	parsedDevices, currentDevices map[string]bool,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for device := range b.disappearedLatch {
		if !parsedDevices[device] || currentDevices[device] {
			continue
		}

		delete(b.disappearedLatch, device)

		slog.Info("Latched device re-enumerated without target-layer ports; emitting device recovery",
			"device", device, "linkLayer", b.strategy.linkLayer())

		events = append(events, checks.NewHealthEvent(
			b.nodeName, b.strategy.checkName(),
			fmt.Sprintf("Device %s re-enumerated in sysfs", device),
			checks.DeviceEntities(device),
			false, true, pb.RecommendedAction_NONE, b.processingStrategy,
		))
	}

	return events
}

// retainUnreadableDevices treats a device that was enumerated but could not
// be parsed as unknown, not absent. Its last committed device/port snapshots
// are carried into the candidate poll so neither disappearance nor a partial
// read can fabricate a state transition.
func (b *baseStateCheck) retainUnreadableDevices(
	unreadable map[string]error,
	seenDevices, currentDevices map[string]bool,
	currentPorts map[string]portSnapshot,
) {
	for device, err := range unreadable {
		seenDevices[device] = true
		delete(b.deviceMissCounts, device)

		if !b.previousDevices[device] {
			continue
		}

		currentDevices[device] = true
		for key, snap := range b.previousPorts {
			if snap.Device == device {
				currentPorts[key] = snap
			}
		}

		slog.Warn("Retaining last-known NIC state after incomplete sysfs read",
			"device", device, "linkLayer", b.strategy.linkLayer(), "error", err)
	}
}

// overrideActive reports whether the explicit inclusion override is
// configured. While active, discovery returns only operator-pinned
// devices: peer-group statistics are meaningless (the "group" is just
// the pinned set), so the card-homogeneity check is skipped and pinned
// devices are reported unconditionally — explicit operator intent is
// the evidence that the device is supposed to be up.
func (b *baseStateCheck) overrideActive() bool {
	return strings.TrimSpace(b.cfg.NicInclusionRegexOverride) != ""
}

// shouldMonitor is the device-level filter applied before any port work.
// Normal discovery excludes VFs; this handles vendor support and management
// NIC classification. An explicit inclusion match bypasses every device filter.
func (b *baseStateCheck) shouldMonitor(dev discovery.IBDevice) bool {
	if dev.IncludedByOverride {
		return true
	}

	if !discovery.IsSupportedVendor(&dev) {
		slog.Debug("Skipping unsupported vendor", "device", dev.Name, "vendor", dev.Vendor)
		return false
	}

	if b.classifier.IsManagementNIC(dev.Name) {
		return false
	}

	return true
}

// portEvent builds a standard port-level HealthEvent (NIC + NICPort entities).
func (b *baseStateCheck) portEvent(
	device string, port int, message string,
	isFatal, isHealthy bool, action pb.RecommendedAction,
) *pb.HealthEvent {
	return checks.NewHealthEvent(
		b.nodeName, b.strategy.checkName(), message,
		checks.PortEntities(device, port),
		isFatal, isHealthy, action, b.processingStrategy,
	)
}

// cardHomogeneityEvents maintains the card-anomaly latch and returns the
// card-level events for this poll. Called every poll (not just the
// first) so card events have a full lifecycle — previously the FATAL
// card event had no recovery counterpart, permanently wedging any
// quarantine held by the card entity once the underlying ports came
// back.
//
//   - A card that just became anomalous (below its role group's decisive
//     mode) latches and emits one FATAL REPLACE_VM.
//   - A latched card that is positively evaluated again (present, group
//     decisive) and no longer below the mode unlatches and emits one
//     card-healthy recovery.
//   - Absent cards and indecisive groups (ties, all-down, <2 peers)
//     hold the latch: no evidence, no verdict. The latch persists
//     across reboots and scope changes, so held entries recover
//     whenever positive evidence finally arrives.
//   - On baseline runs (host reboot or discovery-scope change) every
//     healthy evaluated card additionally emits a card-healthy
//     baseline, clearing stale card entities whose latch was lost
//     (e.g., a corrupt state file).
//
// The anomalies/evaluated maps are computed once per poll by the caller
// (via classifier.EvaluateCardHomogeneity) and shared with the per-port
// first-poll severity decision so both views of "peer evidence" agree.
func (b *baseStateCheck) cardHomogeneityEvents(
	cardActive map[string]int,
	cardRole map[string]topology.Role,
	anomalies map[string]topology.CardAnomaly,
	evaluated map[string]int,
	baselineRun bool,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for card, a := range anomalies {
		if b.anomalousLatch[card] {
			continue // already reported; stay silent until recovery
		}

		b.anomalousLatch[card] = true

		slog.Warn("Card homogeneity anomaly detected",
			"card", card, "role", a.Role.String(),
			"active_ports", a.ActiveSeen, "expected_mode", a.ExpectedModeCount,
		)

		events = append(events, checks.NewHealthEvent(
			b.nodeName, b.strategy.checkName(),
			fmt.Sprintf("Card %s (%s) has %d active ports, expected %d (peer mode)",
				card, a.Role.String(), a.ActiveSeen, a.ExpectedModeCount),
			checks.DeviceEntities(card),
			true, false, pb.RecommendedAction_REPLACE_VM, b.processingStrategy,
		))
	}

	healthyEmitted := make(map[string]bool)

	for card := range b.anomalousLatch {
		mode, judged := evaluated[card]
		if !judged {
			continue // absent or indecisive group: hold the latch
		}

		if _, still := anomalies[card]; still {
			continue
		}

		delete(b.anomalousLatch, card)

		healthyEmitted[card] = true

		events = append(events, b.cardHealthyEvent(card, cardRole[card], cardActive[card], mode))
	}

	if baselineRun {
		for card, mode := range evaluated {
			if _, bad := anomalies[card]; bad || healthyEmitted[card] {
				continue
			}

			events = append(events, b.cardHealthyEvent(card, cardRole[card], cardActive[card], mode))
		}
	}

	return events
}

// cardHealthyEvent builds the IsHealthy card event used for both latch
// recoveries and baseline runs. Like port recoveries, downstream
// consumers treat any healthy event as "clear the stale FATAL on this
// entity".
func (b *baseStateCheck) cardHealthyEvent(
	card string, role topology.Role, active, mode int,
) *pb.HealthEvent {
	slog.Info("Card homogeneity healthy",
		"card", card, "role", role.String(),
		"active_ports", active, "expected_mode", mode,
	)

	return checks.NewHealthEvent(
		b.nodeName, b.strategy.checkName(),
		fmt.Sprintf("Card %s (%s) healthy: %d active ports meet peer mode %d",
			card, role.String(), active, mode),
		checks.DeviceEntities(card),
		false, true, pb.RecommendedAction_NONE, b.processingStrategy,
	)
}

// detectDeviceDisappearance compares the current device set against the
// previous poll's set. Missing devices get a FATAL event with the NIC
// entity.
func (b *baseStateCheck) detectDeviceDisappearance(
	seenDevices map[string]bool,
	currentDevices map[string]bool,
	currentPorts map[string]portSnapshot,
) ([]*pb.HealthEvent, bool) {
	if b.previousDevices == nil {
		return nil, false
	}

	var events []*pb.HealthEvent

	heldMissingState := false

	for device := range b.previousDevices {
		if seenDevices[device] {
			delete(b.deviceMissCounts, device)
			continue
		}

		misses := b.deviceMissCounts[device] + 1
		if misses < deviceMissThreshold {
			heldMissingState = true
			b.deviceMissCounts[device] = misses
			currentDevices[device] = true

			for key, snap := range b.previousPorts {
				if snap.Device == device {
					currentPorts[key] = snap
				}
			}

			slog.Warn("Device absent from sysfs enumeration; holding last-known state",
				"device", device, "linkLayer", b.strategy.linkLayer(),
				"misses", misses, "threshold", deviceMissThreshold)

			continue
		}

		delete(b.deviceMissCounts, device)
		b.disappearedLatch[device] = true

		slog.Warn("Device disappeared from sysfs",
			"device", device, "linkLayer", b.strategy.linkLayer())

		metrics.StateCheckErrors.WithLabelValues(
			b.nodeName, b.strategy.checkName(), device, "",
		).Inc()

		events = append(events, checks.NewHealthEvent(
			b.nodeName, b.strategy.checkName(),
			b.strategy.formatDeviceDisappearance(device),
			checks.DeviceEntities(device),
			true, false, pb.RecommendedAction_REPLACE_VM, b.processingStrategy,
		))
	}

	return events, heldMissingState
}

// detectPortDisappearance handles the case where a device is still
// present but one of its ports is not. Ports on a disappeared device are
// skipped (they are covered by detectDeviceDisappearance).
func (b *baseStateCheck) detectPortDisappearance(
	currentDevices map[string]bool,
	currentPorts map[string]portSnapshot,
) []*pb.HealthEvent {
	if b.previousPorts == nil {
		return nil
	}

	var events []*pb.HealthEvent

	for key, prev := range b.previousPorts {
		if _, exists := currentPorts[key]; exists {
			continue
		}

		if !currentDevices[prev.Device] {
			continue
		}

		slog.Warn("Port disappeared from sysfs",
			"device", prev.Device, "port", prev.Port, "linkLayer", b.strategy.linkLayer())

		metrics.StateCheckErrors.WithLabelValues(
			b.nodeName, b.strategy.checkName(), prev.Device, discovery.PortEntityValue(prev.Port),
		).Inc()

		events = append(events, b.portEvent(
			prev.Device, prev.Port,
			b.strategy.formatPortDisappearance(prev.Device, prev.Port),
			true, false, pb.RecommendedAction_REPLACE_VM,
		))
	}

	return events
}

// persistState writes the current poll state for the given link layer to
// the shared state file. Failures are logged but never bubble up; per
// the design, persistence errors must not halt monitoring.
func (b *baseStateCheck) persistState(
	linkLayer string,
	currentDevices map[string]bool,
	currentPorts map[string]portSnapshot,
) {
	snapshots := make(map[string]statefile.PortStateSnapshot, len(currentPorts))
	for k, v := range currentPorts {
		snapshots[k] = statefile.PortStateSnapshot{
			Device:        v.Device,
			Port:          v.Port,
			State:         v.State,
			PhysicalState: v.PhysicalState,
			LinkLayer:     linkLayer,
		}
	}

	devices := make([]string, 0, len(currentDevices))
	for d := range currentDevices {
		devices = append(devices, d)
	}

	latch := make(map[string]statefile.AnomalousCardFlag, len(b.anomalousLatch))
	for card := range b.anomalousLatch {
		latch[card] = statefile.AnomalousCardFlag{LinkLayer: linkLayer}
	}

	portsChanged := b.state.UpdatePortStates(snapshots, devices, linkLayer)
	latchChanged := b.state.UpdateAnomalousCards(latch, linkLayer)

	disappeared := make(map[string]statefile.DisappearedDeviceFlag, len(b.disappearedLatch))
	for device := range b.disappearedLatch {
		disappeared[device] = statefile.DisappearedDeviceFlag{LinkLayer: linkLayer}
	}

	disappearedChanged := b.state.UpdateDisappearedDevices(disappeared, linkLayer)
	missesChanged := b.state.UpdateDeviceMissCounts(b.deviceMissCounts, linkLayer)

	b.saveState(linkLayer, portsChanged || latchChanged || disappearedChanged || missesChanged)
}

// saveState writes the shared state file when something changed or when
// a previous Save failed. saveFailed keeps a failed Save retrying on
// subsequent polls: the in-memory manager already carries the update, so
// the change flags stay false on an unchanged next poll and the on-disk
// state would otherwise remain stale until an unrelated change or a
// restart.
func (b *baseStateCheck) saveState(linkLayer string, changed bool) {
	if !changed && !b.saveFailed {
		return
	}

	if err := b.state.Save(); err != nil {
		b.saveFailed = true

		slog.Warn("Failed to persist state to disk",
			"linkLayer", linkLayer, "path", b.state.Path(), "error", err)

		return
	}

	b.saveFailed = false
}
