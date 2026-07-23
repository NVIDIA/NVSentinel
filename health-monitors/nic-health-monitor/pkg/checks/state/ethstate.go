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

const ethLinkLayer = topology.LinkLayerEthernet

// EthernetStateCheck is the RoCE counterpart of InfiniBandStateCheck.
// It uses the InfiniBand sysfs interface (state/phys_state) plus the
// associated net device's operstate for richer event messages and
// shares the persistent state file with the IB check (each owns
// entries tagged with its LinkLayer).
type EthernetStateCheck struct {
	baseStateCheck
}

var _ linkLayerStrategy = (*EthernetStateCheck)(nil)
var _ checks.TransactionalCheck = (*EthernetStateCheck)(nil)

func (c *EthernetStateCheck) checkName() string { return checks.EthernetStateCheckName }
func (c *EthernetStateCheck) linkLayer() string { return ethLinkLayer }

func (c *EthernetStateCheck) isTargetPort(port *discovery.IBPort) bool {
	return discovery.IsEthernetPort(port)
}

func (c *EthernetStateCheck) formatDeviceDisappearance(device string) string {
	return fmt.Sprintf("RoCE device %s disappeared from sysfs", device)
}

func (c *EthernetStateCheck) formatPortDisappearance(device string, port int) string {
	return fmt.Sprintf("RoCE port %s port %d disappeared from sysfs", device, port)
}

// NewEthernetStateCheck wires the dependencies used by the RoCE state check.
// Seeds previous-state maps from the file (filtered to Ethernet ports)
// and persists the current state after each poll. bootIDChanged controls
// whether the first poll emits healthy baselines — see
// InfiniBandStateCheck for the same contract.
func NewEthernetStateCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	stateManager *statefile.Manager,
	bootIDChanged bool,
) *EthernetStateCheck {
	// A baseline owed by a previous pod (deferred/partial window, then
	// restart) is picked up from the persisted flag; a fresh trigger is
	// registered so it survives partial-window commits.
	pendingBaseline := bootIDChanged || stateManager.PendingBaseline(checks.EthernetStateCheckName)
	if pendingBaseline {
		stateManager.SetPendingBaseline(checks.EthernetStateCheckName)
	}

	c := &EthernetStateCheck{}
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

// Name returns the check identifier.
func (c *EthernetStateCheck) Name() string { return checks.EthernetStateCheckName }

// ethPortInfo captures the per-port data needed by the transition
// evaluator.
type ethPortInfo struct {
	dev  discovery.IBDevice
	port discovery.IBPort
	key  string
	snap portSnapshot
}

// ethPollState is the poll-level aggregate for EthernetStateCheck.
type ethPollState struct {
	seenDevices        map[string]bool
	parsedDevices      map[string]bool
	currentDevices     map[string]bool
	currentPorts       map[string]portSnapshot
	managementCards    map[string]bool
	allPorts           []ethPortInfo
	discoveryUncertain bool

	cardActive map[string]int
	cardTotal  map[string]int
	cardRole   map[string]topology.Role
	portCard   map[string]string
}

func newEthPollState() *ethPollState {
	return &ethPollState{
		seenDevices:     make(map[string]bool),
		parsedDevices:   make(map[string]bool),
		currentDevices:  make(map[string]bool),
		currentPorts:    make(map[string]portSnapshot),
		managementCards: make(map[string]bool),
		allPorts:        nil,
		cardActive:      make(map[string]int),
		cardTotal:       make(map[string]int),
		cardRole:        make(map[string]topology.Role),
		portCard:        make(map[string]string),
	}
}

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *EthernetStateCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err != nil {
		return nil, err
	}

	c.Commit()

	return events, nil
}

// Prepare observes one poll and stages its candidate state without advancing
// the committed transition maps or persistent state.
func (c *EthernetStateCheck) Prepare() ([]*pb.HealthEvent, error) {
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

		return nil, nil
	}

	metrics.DevicesDiscovered.WithLabelValues(c.nodeName, c.Name()).Set(float64(len(result.Devices)))

	firstPoll := c.previousDevices == nil
	if firstPoll && len(result.UnreadableDevices) > 0 &&
		c.deferFirstPoll(len(result.UnreadableDevices)) {
		return nil, nil
	}

	// The baseline reconciliation (check-scoped clear + replay) waits
	// for the first COMPLETE enumeration: running it while a device is
	// unreadable would wipe that device's previous-boot conditions with
	// nothing able to re-assert them (homogeneity is suppressed while
	// discovery is uncertain). Until then the check monitors whatever is
	// readable and the baseline stays owed.
	baselineRun := c.emitHealthyBaselines && len(result.UnreadableDevices) == 0
	st := newEthPollState()
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

	if firstPoll {
		c.classifier.LogClassificationSummary()
	}

	c.pending = &statePollCommit{
		devices:              st.currentDevices,
		ports:                st.currentPorts,
		anomalousLatch:       c.anomalousLatch,
		disappearedLatch:     c.disappearedLatch,
		disappearedPortLatch: c.disappearedPortLatch,
		deviceMissCounts:     c.deviceMissCounts,
		linkLayer:            ethLinkLayer,
		baselineRan:          baselineRun,
	}

	c.anomalousLatch = committedAnomalous
	c.disappearedLatch = committedDisappeared
	c.disappearedPortLatch = committedPortLatch
	c.deviceMissCounts = committedMisses

	return events, nil
}

// Commit installs and persists the most recently prepared state.
func (c *EthernetStateCheck) Commit() {
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
		c.state.ClearPendingBaseline(checks.EthernetStateCheckName)
	}

	c.persistState(pending.linkLayer, pending.devices, pending.ports)
}

// Discard abandons a prepared poll after check or publication failure.
func (c *EthernetStateCheck) Discard() {
	c.pending = nil
}

// collectDevicesAndPorts walks the discovered devices. VFs are already
// excluded by discovery; this filters unsupported vendors and management
// NICs. seenDevices tracks all physical devices for disappearance detection.
//
// Device-level lifecycle (disappearance detection and its latch) is
// scoped to this check's link layer: a device joins currentDevices only
// while it exposes at least one Ethernet port. Without this scoping a
// sibling-layer device (e.g., a pure-IB NIC) would be latched by this
// check on disappearance and could never recover — latch consumption is
// driven by this layer's port events, which such a device never emits.
func (c *EthernetStateCheck) collectDevicesAndPorts(devices []discovery.IBDevice, st *ethPollState) {
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

		card := c.classifier.PCICardOf(dev.Name)
		role := c.classifier.RoleOf(dev.Name)

		for i := range dev.Ports {
			p := dev.Ports[i]
			if !discovery.IsEthernetPort(&p) {
				continue
			}

			st.currentDevices[dev.Name] = true
			st.cardRole[card] = role

			c.recordPort(st, dev, card, p)
		}
	}
}

// recordPort writes one port into the poll state and bumps the card
// aggregates used by the homogeneity check.
func (c *EthernetStateCheck) recordPort(
	st *ethPollState, dev discovery.IBDevice, card string, p discovery.IBPort,
) {
	key := portKey(dev.Name, p.Port)
	snap := portSnapshot{
		State:         p.State,
		PhysicalState: p.PhysicalState,
		Device:        dev.Name,
		Port:          p.Port,
	}

	st.currentPorts[key] = snap
	st.cardTotal[card]++

	if p.State == checks.IBStateActive && p.PhysicalState == checks.IBPhysLinkUp {
		st.cardActive[card]++
	}

	st.portCard[key] = card

	st.allPorts = append(st.allPorts, ethPortInfo{dev: dev, port: p, key: key, snap: snap})
}

// buildEventsForPoll adapts the Ethernet poll state to the shared event
// pipeline in baseStateCheck.buildEvents.
//
// baselineRun is true on the first poll after a boot-ID change and
// asks the per-port evaluator to emit healthy baselines for every
// currently-healthy port so stale platform conditions clear.
func (c *EthernetStateCheck) buildEventsForPoll(
	st *ethPollState, firstPoll, baselineRun bool,
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

// portTransitionEvents iterates every recorded port and emits events on
// health-boundary crossings.
func (c *EthernetStateCheck) portTransitionEvents(
	st *ethPollState, firstPoll, baselineRun bool, anomalousCards map[string]topology.CardAnomaly,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for _, pi := range st.allPorts {
		if evt := c.evaluatePortTransition(pi, firstPoll, baselineRun, anomalousCards, st.portCard); evt != nil {
			events = append(events, evt)
		}
	}

	return events
}

// evaluatePortTransition is the Ethernet equivalent of its IB sibling,
// with two differences: (1) the operstate from /sys/class/net is
// included in DOWN messages when available, and (2) intermediate
// logical-state changes (INIT, ARMED) are debug-logged and not reported.
//
// baselineRun flips the first-seen-healthy path from "emit nothing" to
// "emit a healthy baseline" to clear stale FATAL conditions after a
// host reboot.
func (c *EthernetStateCheck) evaluatePortTransition(
	pi ethPortInfo,
	firstPoll, baselineRun bool,
	anomalousCards map[string]topology.CardAnomaly,
	portCard map[string]string,
) *pb.HealthEvent {
	prev, existed := c.previousPorts[pi.key]

	isHealthy := portIsHealthy(pi.snap)
	wasHealthy := existed && portIsHealthy(prev)

	var disappearanceRecovery bool

	if isHealthy {
		// Consume both latch levels independently: a device latch and a
		// port latch can coexist for the same port.
		deviceRecovery := c.consumeDisappearanceRecovery(pi.snap.Device)
		portRecovery := c.consumePortDisappearanceRecovery(pi.key)
		disappearanceRecovery = deviceRecovery || portRecovery
	}

	if baselineRun {
		// The baseline poll is the authoritative first observation of
		// this boot: the check-scoped clear just wiped every prior
		// condition, so steady states must re-assert themselves rather
		// than rely on transition edges. Healthy ports emit their
		// baseline; unhealthy ports pass through the first-poll severity
		// gate (peer evidence via card anomalies, override pins exempt) —
		// exactly the verdict a fresh first poll would reach for the
		// same physical state.
		if isHealthy {
			return c.healthyRecoveryEvent(pi, prev, existed, true, disappearanceRecovery)
		}

		return c.unhealthyEvent(pi, prev, true, anomalousCards, portCard)
	}

	if existed && isHealthy == wasHealthy && !disappearanceRecovery {
		return c.escalationEvent(pi, prev, isHealthy, firstPoll, anomalousCards, portCard)
	}

	if isHealthy {
		return c.healthyRecoveryEvent(pi, prev, existed, baselineRun, disappearanceRecovery)
	}

	return c.unhealthyEvent(pi, prev, firstPoll, anomalousCards, portCard)
}

// escalationEvent emits when a still-unhealthy port escalates from a
// non-fatal state to DOWN; other unhealthy→unhealthy and steady-state
// polls stay silent.
func (c *EthernetStateCheck) escalationEvent(
	pi ethPortInfo, prev portSnapshot, isHealthy, firstPoll bool,
	anomalousCards map[string]topology.CardAnomaly, portCard map[string]string,
) *pb.HealthEvent {
	if !isHealthy && ethernetPortIsFatal(pi.snap) && !ethernetPortIsFatal(prev) {
		return c.unhealthyEvent(pi, prev, firstPoll, anomalousCards, portCard)
	}

	return nil
}

// healthyRecoveryEvent returns an IsHealthy=true event for port
// recoveries. On first-seen healthy ports it normally emits nothing
// (to avoid spamming healthy events on routine restarts) unless
// baselineRun is true, in which case it emits a healthy baseline so
// the platform clears stale FATAL conditions from the previous boot.
func (c *EthernetStateCheck) healthyRecoveryEvent(
	pi ethPortInfo, prev portSnapshot, existed, baselineRun, disappearanceRecovery bool,
) *pb.HealthEvent {
	if !existed && !baselineRun && !disappearanceRecovery {
		return nil
	}

	msg := fmt.Sprintf("RoCE port %s port %d: healthy (%s, %s)",
		pi.snap.Device, pi.snap.Port, pi.snap.State, pi.snap.PhysicalState)

	slog.Info(msg,
		"prevState", prev.State, "newState", pi.snap.State,
		"prevPhysState", prev.PhysicalState, "newPhysState", pi.snap.PhysicalState,
		"baseline_run", baselineRun,
	)

	return c.portEvent(pi.snap.Device, pi.snap.Port, msg, false, true, pb.RecommendedAction_NONE)
}

func ethernetPortIsFatal(snap portSnapshot) bool {
	return snap.State == checks.IBStateDown
}

// unhealthyEvent returns the event for a DOWN transition, or nil when
// the unhealthy state is a transient non-DOWN (INIT, ARMED) or when a
// first-poll unhealthy port has no peer evidence of failure.
//
// On the first poll, DOWN is fatal only when the emitting card is
// positively anomalous (active-port count below its role group's
// decisive mode). A port that has never been observed healthy carries no
// evidence it is supposed to be up: it may be an uncabled second port or
// an intentionally-disabled/unprovisioned one (e.g., the unused Aux
// frontend port on OCI BM.GPU.H100.8, left as a singleton storage card
// after its Prime twin is excluded as the default-route NIC). Without
// peer evidence the monitor logs and suppresses the event instead of
// publishing an external HealthEvent. Runtime healthy→DOWN transitions
// are always fatal (firstPoll is false once previous state exists).
//
// Devices pinned by the explicit inclusion override are never
// suppressed: the operator asked to watch exactly this device, and that
// intent replaces peer evidence.
func (c *EthernetStateCheck) unhealthyEvent(
	pi ethPortInfo, prev portSnapshot,
	firstPoll bool, anomalousCards map[string]topology.CardAnomaly, portCard map[string]string,
) *pb.HealthEvent {
	if pi.snap.State != checks.IBStateDown {
		slog.Debug("RoCE port in non-ACTIVE state, ignoring",
			"device", pi.snap.Device, "port", pi.snap.Port,
			"state", pi.snap.State, "physState", pi.snap.PhysicalState,
		)

		return nil
	}

	if firstPoll && !pi.dev.IncludedByOverride {
		card := portCard[pi.key]
		if _, anomalous := anomalousCards[card]; !anomalous {
			slog.Info("Suppressing first-poll unhealthy RoCE port: no peer evidence of failure",
				"device", pi.snap.Device, "port", pi.snap.Port, "card", card,
				"state", pi.snap.State, "physState", pi.snap.PhysicalState)

			return nil
		}
	}

	metrics.StateCheckErrors.WithLabelValues(
		c.nodeName, c.Name(), pi.snap.Device, discovery.PortEntityValue(pi.snap.Port),
	).Inc()

	msg := c.buildDownMessage(pi)

	slog.Warn("RoCE port DOWN detected",
		"device", pi.snap.Device, "port", pi.snap.Port,
		"prevState", prev.State, "newState", pi.snap.State,
		"prevPhysState", prev.PhysicalState, "newPhysState", pi.snap.PhysicalState,
	)

	return c.portEvent(pi.snap.Device, pi.snap.Port, msg, true, false, pb.RecommendedAction_REPLACE_VM)
}

// logDiscoverySummaryIfChanged emits a one-line summary whenever the
// discovered set of devices/ports changes size.
func (c *EthernetStateCheck) logDiscoverySummaryIfChanged(st *ethPollState) {
	if len(st.currentDevices) == len(c.previousDevices) &&
		len(st.currentPorts) == len(c.previousPorts) {
		return
	}

	slog.Info("Ethernet discovery summary",
		"check", c.Name(),
		"devices", len(st.currentDevices),
		"eth_ports", len(st.currentPorts),
	)
}

// buildDownMessage composes the fatal event message, enriching it with
// operstate when the associated net device is known and readable.
func (c *EthernetStateCheck) buildDownMessage(pi ethPortInfo) string {
	if pi.dev.NetDev == "" {
		return fmt.Sprintf("RoCE port %s port %d: state %s, phys_state %s",
			pi.snap.Device, pi.snap.Port, pi.snap.State, pi.snap.PhysicalState)
	}

	oper, err := c.reader.ReadNetOperState(pi.dev.NetDev)
	if err != nil {
		return fmt.Sprintf("RoCE port %s port %d: state %s, phys_state %s",
			pi.snap.Device, pi.snap.Port, pi.snap.State, pi.snap.PhysicalState)
	}

	return fmt.Sprintf("RoCE port %s port %d: state %s, phys_state %s, operstate %s",
		pi.snap.Device, pi.snap.Port, pi.snap.State, pi.snap.PhysicalState, oper)
}
