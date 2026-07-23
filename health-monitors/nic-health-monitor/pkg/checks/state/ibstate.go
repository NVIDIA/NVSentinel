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

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

const ibLinkLayer = topology.LinkLayerInfiniBand

// InfiniBandStateCheck polls IB port states and emits raw HealthEvents
// on every healthy↔unhealthy boundary crossing. State survives pod
// restarts via pkg/statefile: on startup the check seeds its in-memory
// port map from the persisted snapshot; after each poll it writes the
// current port map back so a subsequent pod can emit recovery events
// for ports that were DOWN before the restart and are now ACTIVE.
//
// When the persisted state file indicates a boot-ID change (fresh node,
// corrupt file, or host reboot), the check emits a healthy baseline
// event for every currently-healthy port on the first poll to clear
// any stale FATAL conditions carried on the platform from the previous
// boot.
type InfiniBandStateCheck struct {
	baseStateCheck
}

var _ linkLayerStrategy = (*InfiniBandStateCheck)(nil)
var _ checks.TransactionalCheck = (*InfiniBandStateCheck)(nil)

func (c *InfiniBandStateCheck) checkName() string { return checks.InfiniBandStateCheckName }
func (c *InfiniBandStateCheck) linkLayer() string { return ibLinkLayer }

func (c *InfiniBandStateCheck) isTargetPort(port *discovery.IBPort) bool {
	return discovery.IsIBPort(port)
}

func (c *InfiniBandStateCheck) formatDeviceDisappearance(device string) string {
	return fmt.Sprintf("NIC %s disappeared from /sys/class/infiniband/ - hardware failure", device)
}

func (c *InfiniBandStateCheck) formatPortDisappearance(device string, port int) string {
	return fmt.Sprintf("Port %s port %d disappeared from sysfs", device, port)
}

// NewInfiniBandStateCheck wires the dependencies used by the IB state
// check. The check persists its portion of MonitorState to the shared
// file after each poll and seeds its in-memory maps from the file at
// construction time.
//
// The bootIDChanged flag — typically the return value of
// stateManager.BootIDChanged() right after Load — controls whether the
// first poll emits healthy baseline events. Pass false when the
// persisted state is trusted (pod restart, same boot); pass true to
// request the "clear stale platform conditions" behaviour.
func NewInfiniBandStateCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	stateManager *statefile.Manager,
	bootIDChanged bool,
) *InfiniBandStateCheck {
	// A baseline owed by a previous pod (deferred/partial window, then
	// restart) is picked up from the persisted flag; a fresh trigger is
	// registered so it survives partial-window commits.
	pendingBaseline := bootIDChanged || stateManager.PendingBaseline(checks.InfiniBandStateCheckName)
	if pendingBaseline {
		stateManager.SetPendingBaseline(checks.InfiniBandStateCheckName)
	}

	c := &InfiniBandStateCheck{}
	c.baseStateCheck = baseStateCheck{
		nodeName:             nodeName,
		reader:               reader,
		cfg:                  cfg,
		processingStrategy:   processingStrategy,
		classifier:           classifier,
		state:                stateManager,
		emitHealthyBaselines: pendingBaseline,
		strategy:             c,
	}

	c.seedFromPersistedState()

	return c
}

// Name returns the check identifier used by the orchestrator and in events.
func (c *InfiniBandStateCheck) Name() string { return checks.InfiniBandStateCheckName }

// ibPollState collects the per-poll snapshot used by Run to produce
// events. It keeps Run's signature small while letting the loop helpers
// mutate a single struct.
type ibPollState struct {
	seenDevices        map[string]bool
	parsedDevices      map[string]bool
	currentDevices     map[string]bool
	currentPorts       map[string]portSnapshot
	managementCards    map[string]bool
	ibPorts            []discovery.IBPort
	skippedVFs         int
	discoveryUncertain bool

	cardActive   map[string]int
	cardTotal    map[string]int
	cardRole     map[string]topology.Role
	portCard     map[string]string
	portOverride map[string]bool
}

func newIBPollState() *ibPollState {
	return &ibPollState{
		seenDevices:     make(map[string]bool),
		parsedDevices:   make(map[string]bool),
		currentDevices:  make(map[string]bool),
		currentPorts:    make(map[string]portSnapshot),
		managementCards: make(map[string]bool),
		ibPorts:         make([]discovery.IBPort, 0),
		cardActive:      make(map[string]int),
		cardTotal:       make(map[string]int),
		cardRole:        make(map[string]topology.Role),
		portCard:        make(map[string]string),
		portOverride:    make(map[string]bool),
	}
}

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *InfiniBandStateCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err != nil {
		return nil, err
	}

	c.Commit()

	return events, nil
}

// Prepare observes one poll and stages its candidate state without advancing
// the committed transition maps or persistent state.
func (c *InfiniBandStateCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	result, err := discovery.DiscoverDevicesWithOverride(
		c.reader, c.cfg.NicExclusionRegex, c.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, fmt.Errorf("device discovery failed: %w", err)
	}

	if !result.Complete {
		if c.previousDevices != nil {
			return nil, fmt.Errorf("device discovery incomplete: InfiniBand sysfs tree unavailable")
		}

		// Keep first-poll and reboot-baseline behavior pending until one
		// complete enumeration succeeds. Nodes without an IB tree stay quiet.
		return nil, nil
	}

	metrics.DevicesDiscovered.WithLabelValues(c.nodeName, c.Name()).Set(float64(len(result.Devices)))

	firstPoll := c.previousDevices == nil
	if firstPoll && len(result.UnreadableDevices) > 0 &&
		c.deferFirstPoll(len(result.UnreadableDevices)) {
		// Peer homogeneity and first-seen severity gating both prefer a
		// full initial device set, so the first poll is deferred — but
		// only up to the bounded limit (see deferFirstPoll).
		return nil, nil
	}

	// The baseline reconciliation (check-scoped clear + replay) waits
	// for the first COMPLETE enumeration: running it while a device is
	// unreadable would wipe that device's previous-boot conditions with
	// nothing able to re-assert them (homogeneity is suppressed while
	// discovery is uncertain). Until then the check monitors whatever is
	// readable and the baseline stays owed.
	baselineRun := c.emitHealthyBaselines && len(result.UnreadableDevices) == 0
	st := newIBPollState()
	st.skippedVFs = result.SkippedVFs
	st.discoveryUncertain = len(result.UnreadableDevices) > 0

	committedAnomalous := c.anomalousLatch
	committedDisappeared := c.disappearedLatch
	committedPortLatch := c.disappearedPortLatch
	committedMisses := c.deviceMissCounts
	c.anomalousLatch = maps.Clone(committedAnomalous)
	c.disappearedLatch = maps.Clone(committedDisappeared)
	c.disappearedPortLatch = maps.Clone(committedPortLatch)
	c.deviceMissCounts = maps.Clone(committedMisses)

	c.collectDevicesAndPorts(result.Devices, st)
	c.retainUnreadableDevices(
		result.UnreadableDevices, st.seenDevices, st.currentDevices, st.currentPorts,
	)

	events := c.buildEventsForPoll(st, firstPoll, baselineRun)
	c.logDiscoverySummaryIfChanged(st)

	c.pending = &statePollCommit{
		devices:              st.currentDevices,
		ports:                st.currentPorts,
		anomalousLatch:       c.anomalousLatch,
		disappearedLatch:     c.disappearedLatch,
		disappearedPortLatch: c.disappearedPortLatch,
		deviceMissCounts:     c.deviceMissCounts,
		linkLayer:            ibLinkLayer,
		baselineRan:          baselineRun,
	}

	c.anomalousLatch = committedAnomalous
	c.disappearedLatch = committedDisappeared
	c.disappearedPortLatch = committedPortLatch
	c.deviceMissCounts = committedMisses

	return events, nil
}

// Commit installs and persists the most recently prepared state.
func (c *InfiniBandStateCheck) Commit() {
	if c.pending == nil {
		return
	}

	pending := c.pending
	c.pending = nil
	c.previousDevices = pending.devices
	c.previousPorts = pending.ports
	c.anomalousLatch = pending.anomalousLatch
	c.disappearedLatch = pending.disappearedLatch
	c.disappearedPortLatch = pending.disappearedPortLatch
	c.deviceMissCounts = pending.deviceMissCounts

	if pending.baselineRan {
		c.emitHealthyBaselines = false
		c.state.ClearPendingBaseline(checks.InfiniBandStateCheckName)
	}

	c.persistState(pending.linkLayer, pending.devices, pending.ports)
}

// Discard abandons a prepared poll after check or publication failure.
func (c *InfiniBandStateCheck) Discard() {
	c.pending = nil
}

// collectDevicesAndPorts iterates discovered devices and records the
// monitored subset in the poll state. VFs are already excluded by
// discovery; this filters unsupported vendors and management NICs.
//
// Device-level lifecycle (disappearance detection and its latch) is
// scoped to this check's link layer: a device joins currentDevices only
// while it exposes at least one InfiniBand port. Without this scoping a
// sibling-layer device (e.g., a pure-RoCE NIC) would be latched by this
// check on disappearance and could never recover — latch consumption is
// driven by this layer's port events, which such a device never emits.
func (c *InfiniBandStateCheck) collectDevicesAndPorts(devices []discovery.IBDevice, st *ibPollState) {
	for _, dev := range devices {
		st.seenDevices[dev.Name] = true
		st.parsedDevices[dev.Name] = true

		if !c.shouldMonitor(dev) {
			// A management-classified function marks its whole card as
			// frontend plumbing — see exemptManagementSiblingCards.
			if c.classifier.IsManagementNIC(dev.Name) {
				st.managementCards[c.classifier.PCICardOf(dev.Name)] = true
			}

			continue
		}

		role := c.classifier.RoleOf(dev.Name)
		card := c.classifier.PCICardOf(dev.Name)

		for _, port := range dev.Ports {
			if !discovery.IsIBPort(&port) {
				continue
			}

			st.currentDevices[dev.Name] = true

			c.recordPort(st, dev.Name, card, role, port, dev.IncludedByOverride)
		}
	}
}

// recordPort writes one port's snapshot into the poll state and updates
// the per-card aggregates used by the homogeneity check.
func (c *InfiniBandStateCheck) recordPort(
	st *ibPollState, device, card string, role topology.Role, port discovery.IBPort,
	includedByOverride bool,
) {
	key := portKey(device, port.Port)
	snap := portSnapshot{
		State:         port.State,
		PhysicalState: port.PhysicalState,
		Device:        port.Device,
		Port:          port.Port,
	}

	st.currentPorts[key] = snap
	st.ibPorts = append(st.ibPorts, port)

	st.cardTotal[card]++
	if port.State == checks.IBStateActive && port.PhysicalState == checks.IBPhysLinkUp {
		st.cardActive[card]++
	}

	st.cardRole[card] = role
	st.portCard[key] = card
	st.portOverride[key] = includedByOverride
}

// buildEventsForPoll adapts the IB poll state to the shared event
// pipeline in baseStateCheck.buildEvents.
//
// baselineRun is true only on the first poll after a boot-ID change
// (fresh node, host reboot, corrupt state file). In that mode the
// per-port evaluator emits healthy baseline events for every currently
// ACTIVE/LinkUp port so the platform can clear stale FATAL conditions
// from the previous boot.
func (c *InfiniBandStateCheck) buildEventsForPoll(
	st *ibPollState, firstPoll, baselineRun bool,
) []*pb.HealthEvent {
	agg := pollAggregates{
		seenDevices:     st.seenDevices,
		parsedDevices:   st.parsedDevices,
		currentDevices:  st.currentDevices,
		currentPorts:    st.currentPorts,
		cardActive:      st.cardActive,
		cardTotal:       st.cardTotal,
		cardRole:        st.cardRole,
		managementCards: st.managementCards,
		uncertain:       st.discoveryUncertain,
	}

	return c.buildEvents(agg, baselineRun,
		func(anomalousCards map[string]topology.CardAnomaly) []*pb.HealthEvent {
			return c.portTransitionEvents(st, firstPoll, baselineRun, anomalousCards)
		})
}

// portTransitionEvents produces the boundary-crossing events for every
// port in the current poll. On the first poll, unhealthy ports are emitted
// only when the emitting card is positively anomalous (below its role
// group's decisive mode); otherwise they are logged and suppressed.
func (c *InfiniBandStateCheck) portTransitionEvents(
	st *ibPollState, firstPoll, baselineRun bool, anomalousCards map[string]topology.CardAnomaly,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for _, port := range st.ibPorts {
		key := portKey(port.Device, port.Port)
		prev, hasPrev := c.previousPorts[key]
		current := st.currentPorts[key]
		card := st.portCard[key]

		// The baseline poll re-runs first-poll semantics (see
		// evaluatePortTransition), so its unhealthy re-assertions go
		// through the same peer-evidence gate.
		if !st.portOverride[key] &&
			c.shouldSuppressFirstPollWithoutPeerEvidence(firstPoll || baselineRun, current, port, card, anomalousCards) {
			continue
		}

		evt := c.evaluatePortTransition(key, current, prev, hasPrev, baselineRun)
		if evt == nil {
			continue
		}

		events = append(events, evt)
	}

	return events
}

// shouldSuppressFirstPollWithoutPeerEvidence reports whether a first-poll
// unhealthy port should be kept local to monitor logs because there is no
// positive peer evidence that the port should be up.
//
// A port that has never been observed healthy carries no evidence that it
// is supposed to be up: it may be the uncabled second port of a dual-port
// card, or an intentionally-disabled/unprovisioned port (e.g., the unused
// Aux frontend port on OCI BM.GPU.H100.8, which is Disabled by design and
// has no peer left once its Prime twin is excluded as the default-route
// NIC). Peer comparison is the only safe evidence at this point, so no
// external HealthEvent is emitted without it. Singleton role groups, tied
// modes, and all-down groups therefore suppress first-poll unhealthy
// ports; runtime healthy→unhealthy transitions are unaffected because
// firstPoll is false once previous state exists.
//
// Devices pinned by the explicit inclusion override never reach this
// gate (the caller checks portOverride first): the operator asked to
// watch exactly that device, and that intent replaces peer evidence.
func (c *InfiniBandStateCheck) shouldSuppressFirstPollWithoutPeerEvidence(
	firstPoll bool,
	current portSnapshot,
	port discovery.IBPort,
	card string,
	anomalousCards map[string]topology.CardAnomaly,
) bool {
	if !firstPoll {
		return false
	}

	isHealthy := current.State == checks.IBStateActive && current.PhysicalState == checks.IBPhysLinkUp
	if isHealthy {
		return false
	}

	if _, anomalous := anomalousCards[card]; anomalous {
		return false
	}

	slog.Info("Suppressing first-poll unhealthy IB port: no peer evidence of failure",
		"device", port.Device, "port", port.Port, "card", card,
		"state", current.State, "physState", current.PhysicalState)

	return true
}

// logDiscoverySummaryIfChanged emits a one-line summary whenever the
// discovered set of devices/ports changes size. On the first poll
// previousDevices is nil (len 0), so the size always differs and the
// summary is logged unconditionally.
func (c *InfiniBandStateCheck) logDiscoverySummaryIfChanged(st *ibPollState) {
	if len(st.currentDevices) == len(c.previousDevices) &&
		len(st.currentPorts) == len(c.previousPorts) {
		return
	}

	slog.Info("IB discovery summary",
		"check", c.Name(),
		"devices", len(st.currentDevices),
		"ib_ports", len(st.currentPorts),
		"skipped_vfs", st.skippedVFs,
	)
}

// evaluatePortTransition decides whether to emit an event for a port
// given its current and previous snapshots. Intermediate unhealthy→
// unhealthy changes are suppressed unless severity escalates from non-fatal
// to fatal; otherwise the monitor reports health-boundary crossings.
//
// baselineRun flips the first-seen behaviour for healthy ports from
// "emit nothing" to "emit a healthy baseline event" so the platform can
// clear stale FATAL conditions from the previous boot.
func (c *InfiniBandStateCheck) evaluatePortTransition(
	key string,
	current, prev portSnapshot,
	hasPrev, baselineRun bool,
) *pb.HealthEvent {
	isHealthy := portIsHealthy(current)

	var disappearanceRecovery bool

	if isHealthy {
		// Consume both latch levels independently: a device latch and a
		// port latch can coexist for the same port.
		deviceRecovery := c.consumeDisappearanceRecovery(current.Device)
		portRecovery := c.consumePortDisappearanceRecovery(key)
		disappearanceRecovery = deviceRecovery || portRecovery
	}

	if baselineRun {
		// The baseline poll is the authoritative first observation of
		// this boot: the check-scoped clear just wiped every prior
		// condition, so steady states must re-assert themselves rather
		// than rely on transition edges. Healthy ports emit their
		// baseline; unhealthy ports were already passed through the
		// first-poll peer-evidence gate by the caller.
		if isHealthy {
			return c.healthyBaselineEvent(current)
		}

		return c.unhealthyPortEvent(current)
	}

	if !hasPrev {
		return c.firstSeenPortEvent(current, isHealthy, baselineRun, disappearanceRecovery)
	}

	wasHealthy := portIsHealthy(prev)

	if isHealthy == wasHealthy && !disappearanceRecovery {
		return c.escalationEvent(current, prev, isHealthy)
	}

	slog.Info("IB port state transition",
		"device", current.Device, "port", current.Port,
		"prevState", prev.State, "prevPhysState", prev.PhysicalState,
		"state", current.State, "physState", current.PhysicalState,
		"wasHealthy", wasHealthy, "isHealthy", isHealthy,
	)

	if isHealthy {
		return c.healthyBaselineEvent(current)
	}

	return c.unhealthyPortEvent(current)
}

// firstSeenPortEvent handles a port with no previous snapshot. Healthy
// first-seen ports are silent unless this is a baseline run (boot-ID
// change) or the port belongs to a device recovering from a latched
// disappearance; unhealthy ones are classified by severity as usual.
func (c *InfiniBandStateCheck) firstSeenPortEvent(
	current portSnapshot,
	isHealthy, baselineRun, disappearanceRecovery bool,
) *pb.HealthEvent {
	slog.Info("First-seen IB port",
		"device", current.Device, "port", current.Port,
		"state", current.State, "physState", current.PhysicalState,
		"healthy", isHealthy,
		"baseline_run", baselineRun,
	)

	if !isHealthy {
		return c.unhealthyPortEvent(current)
	}

	if !baselineRun && !disappearanceRecovery {
		return nil
	}

	return c.healthyBaselineEvent(current)
}

// escalationEvent emits when a still-unhealthy port escalates from a
// non-fatal to a fatal sub-state (e.g., DOWN/Polling → DOWN/Disabled);
// all other unhealthy→unhealthy and steady-state polls stay silent.
func (c *InfiniBandStateCheck) escalationEvent(current, prev portSnapshot, isHealthy bool) *pb.HealthEvent {
	if !isHealthy && ibPortIsFatal(current) && !ibPortIsFatal(prev) {
		return c.unhealthyPortEvent(current)
	}

	return nil
}

// healthyBaselineEvent builds the IsHealthy=true event used for both
// recovery transitions and boot-ID-change baselines. The message format
// is identical in both cases so downstream consumers don't need to
// distinguish the two — the analyzer treats any healthy event as a
// "clear the stale FATAL on this entity" signal.
func (c *InfiniBandStateCheck) healthyBaselineEvent(current portSnapshot) *pb.HealthEvent {
	msg := fmt.Sprintf("Port %s port %d: healthy (%s, %s)",
		current.Device, current.Port, current.State, current.PhysicalState)

	return c.portEvent(current.Device, current.Port, msg, false, true, pb.RecommendedAction_NONE)
}

// unhealthyPortEvent classifies an unhealthy port's severity:
// state=DOWN or phys_state=Disabled are fatal because the workload
// cannot reach the fabric; INIT and ARMED are non-fatal because they
// are normal transient states during Subnet Manager configuration;
// Polling and LinkErrorRecovery are non-fatal because the driver is
// already attempting to recover. On the first poll, the card
// homogeneity check (see CheckCardHomogeneity) escalates "stuck in a
// non-fatal intermediate state" to fatal when the card has fewer
// active ports than its role peers.
func (c *InfiniBandStateCheck) unhealthyPortEvent(snap portSnapshot) *pb.HealthEvent {
	isFatal := ibPortIsFatal(snap)

	action := pb.RecommendedAction_NONE
	if isFatal {
		action = pb.RecommendedAction_REPLACE_VM
	}

	metrics.StateCheckErrors.WithLabelValues(
		c.nodeName, c.Name(), snap.Device, discovery.PortEntityValue(snap.Port),
	).Inc()

	msg := fmt.Sprintf("Port %s port %d: state %s, phys_state %s",
		snap.Device, snap.Port, snap.State, snap.PhysicalState)

	return c.portEvent(snap.Device, snap.Port, msg, isFatal, false, action)
}

func ibPortIsFatal(snap portSnapshot) bool {
	isFatal := snap.State == checks.IBStateDown

	switch snap.PhysicalState {
	case checks.IBPhysDisabled:
		isFatal = true
	case checks.IBPhysLinkErrorRecovery, checks.IBPhysPolling:
		isFatal = false
	}

	return isFatal
}
