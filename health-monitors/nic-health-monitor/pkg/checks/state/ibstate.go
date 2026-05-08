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
	c := &InfiniBandStateCheck{}
	c.baseStateCheck = baseStateCheck{
		nodeName:             nodeName,
		reader:               reader,
		cfg:                  cfg,
		processingStrategy:   processingStrategy,
		classifier:           classifier,
		state:                stateManager,
		emitHealthyBaselines: bootIDChanged,
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
	seenDevices    map[string]bool
	currentDevices map[string]bool
	currentPorts   map[string]portSnapshot
	ibPorts        []discovery.IBPort
	skippedVFs     int

	cardActive map[string]int
	cardTotal  map[string]int
	cardRole   map[string]topology.Role
	portCard   map[string]string
}

func newIBPollState() *ibPollState {
	return &ibPollState{
		seenDevices:    make(map[string]bool),
		currentDevices: make(map[string]bool),
		currentPorts:   make(map[string]portSnapshot),
		ibPorts:        make([]discovery.IBPort, 0),
		cardActive:     make(map[string]int),
		cardTotal:      make(map[string]int),
		cardRole:       make(map[string]topology.Role),
		portCard:       make(map[string]string),
	}
}

// Run executes a single poll cycle and returns the resulting events.
func (c *InfiniBandStateCheck) Run() ([]*pb.HealthEvent, error) {
	result, err := discovery.DiscoverDevices(c.reader, c.cfg.NicExclusionRegex)
	if err != nil {
		return nil, fmt.Errorf("device discovery failed: %w", err)
	}

	metrics.DevicesDiscovered.WithLabelValues(c.nodeName, c.Name()).Set(float64(len(result.Devices)))

	firstPoll := c.previousDevices == nil
	baselineRun := firstPoll && c.emitHealthyBaselines
	st := newIBPollState()
	st.skippedVFs = result.SkippedVFs

	c.collectDevicesAndPorts(result.Devices, st)

	events := c.buildEventsForPoll(st, firstPoll, baselineRun)
	c.logDiscoverySummaryIfChanged(st)

	c.previousDevices = st.currentDevices
	c.previousPorts = st.currentPorts

	// Baseline emission is a one-shot behaviour: subsequent polls must
	// fall back to normal boundary-crossing semantics.
	c.emitHealthyBaselines = false

	c.persistState(ibLinkLayer, st.currentDevices, st.currentPorts)

	return events, nil
}

// collectDevicesAndPorts iterates discovered devices and records the
// monitored subset in the poll state. VFs are already excluded by
// discovery; this filters unsupported vendors and management NICs.
func (c *InfiniBandStateCheck) collectDevicesAndPorts(devices []discovery.IBDevice, st *ibPollState) {
	for _, dev := range devices {
		st.seenDevices[dev.Name] = true

		if !c.shouldMonitor(dev) {
			continue
		}

		st.currentDevices[dev.Name] = true

		role := c.classifier.RoleOf(dev.Name)
		card := c.classifier.PCICardOf(dev.Name)

		for _, port := range dev.Ports {
			if !discovery.IsIBPort(&port) {
				continue
			}

			c.recordPort(st, dev.Name, card, role, port)
		}
	}
}

// recordPort writes one port's snapshot into the poll state and updates
// the per-card aggregates used by the homogeneity check.
func (c *InfiniBandStateCheck) recordPort(
	st *ibPollState, device, card string, role topology.Role, port discovery.IBPort,
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
}

// buildEventsForPoll runs the event-producing logic (per-port transitions,
// disappearance detection, first-poll homogeneity) and returns the
// combined event slice.
//
// baselineRun is true only on the first poll after a boot-ID change
// (fresh node, host reboot, corrupt state file). In that mode the
// per-port evaluator emits healthy baseline events for every currently
// ACTIVE/LinkUp port so the platform can clear stale FATAL conditions
// from the previous boot.
func (c *InfiniBandStateCheck) buildEventsForPoll(
	st *ibPollState, firstPoll, baselineRun bool,
) []*pb.HealthEvent {
	expectedCards := c.classifier.ExpectedDownCards(st.cardActive, st.cardTotal, st.cardRole)
	events := c.portTransitionEvents(st, firstPoll, baselineRun, expectedCards)
	events = append(events, c.detectDeviceDisappearance(st.seenDevices)...)
	events = append(events, c.detectPortDisappearance(st.currentDevices, st.currentPorts)...)

	if firstPoll {
		events = append(events, c.runCardHomogeneityCheck(st.cardActive, st.cardTotal, st.cardRole)...)
	}

	return events
}

// portTransitionEvents produces the boundary-crossing events for every
// port in the current poll, applying the first-poll suppression for
// cards whose active count matches their role's mode (uncabled-port
// convention).
func (c *InfiniBandStateCheck) portTransitionEvents(
	st *ibPollState, firstPoll, baselineRun bool, expectedCards map[string]struct{},
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for _, port := range st.ibPorts {
		key := portKey(port.Device, port.Port)
		prev, hasPrev := c.previousPorts[key]

		evt := c.evaluatePortTransition(st.currentPorts[key], prev, hasPrev, baselineRun)
		if evt == nil {
			continue
		}

		if firstPoll && !evt.IsHealthy {
			c.applyFirstPollSuppression(evt, port, st.portCard[key], expectedCards)
		}

		events = append(events, evt)
	}

	return events
}

// applyFirstPollSuppression downgrades a first-poll fatal event to
// non-fatal when the emitting card matches the role's expected-down
// mode (e.g., dual-port card with only one port cabled by convention).
func (c *InfiniBandStateCheck) applyFirstPollSuppression(
	evt *pb.HealthEvent, port discovery.IBPort, card string, expected map[string]struct{},
) {
	if _, ok := expected[card]; !ok {
		return
	}

	slog.Info("Suppressing first-poll fatal for expected-down card",
		"device", port.Device, "port", port.Port, "card", card)

	evt.IsFatal = false
	evt.RecommendedAction = pb.RecommendedAction_NONE
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
// unhealthy changes (e.g., DOWN/Disabled → DOWN/Polling) are suppressed;
// the monitor only reports boundary crossings.
//
// baselineRun flips the first-seen behaviour for healthy ports from
// "emit nothing" to "emit a healthy baseline event" so the platform can
// clear stale FATAL conditions from the previous boot.
func (c *InfiniBandStateCheck) evaluatePortTransition(
	current, prev portSnapshot,
	hasPrev, baselineRun bool,
) *pb.HealthEvent {
	isHealthy := current.State == checks.IBStateActive && current.PhysicalState == checks.IBPhysLinkUp

	if !hasPrev {
		slog.Info("First-seen IB port",
			"device", current.Device, "port", current.Port,
			"state", current.State, "physState", current.PhysicalState,
			"healthy", isHealthy,
			"baseline_run", baselineRun,
		)

		if isHealthy {
			if !baselineRun {
				return nil
			}

			return c.healthyBaselineEvent(current)
		}

		return c.unhealthyPortEvent(current)
	}

	wasHealthy := prev.State == checks.IBStateActive && prev.PhysicalState == checks.IBPhysLinkUp

	if isHealthy == wasHealthy {
		return nil
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
// already attempting to recover. The card homogeneity check (see
// CheckCardHomogeneity) converts "stuck in a non-fatal intermediate
// state" into a fatal event by observing that the card has fewer
// active ports than its peers.
func (c *InfiniBandStateCheck) unhealthyPortEvent(snap portSnapshot) *pb.HealthEvent {
	isFatal := snap.State == checks.IBStateDown

	switch snap.PhysicalState {
	case checks.IBPhysDisabled:
		isFatal = true
	case checks.IBPhysLinkErrorRecovery, checks.IBPhysPolling:
		isFatal = false
	}

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
