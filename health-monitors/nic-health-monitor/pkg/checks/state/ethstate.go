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
	c := &EthernetStateCheck{}
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
	seenDevices    map[string]bool
	currentDevices map[string]bool
	currentPorts   map[string]portSnapshot
	allPorts       []ethPortInfo

	cardActive map[string]int
	cardTotal  map[string]int
	cardRole   map[string]topology.Role
	portCard   map[string]string
}

func newEthPollState() *ethPollState {
	return &ethPollState{
		seenDevices:    make(map[string]bool),
		currentDevices: make(map[string]bool),
		currentPorts:   make(map[string]portSnapshot),
		allPorts:       nil,
		cardActive:     make(map[string]int),
		cardTotal:      make(map[string]int),
		cardRole:       make(map[string]topology.Role),
		portCard:       make(map[string]string),
	}
}

// Run executes a single poll cycle.
func (c *EthernetStateCheck) Run() ([]*pb.HealthEvent, error) {
	result, err := discovery.DiscoverDevices(c.reader, c.cfg.NicExclusionRegex)
	if err != nil {
		return nil, fmt.Errorf("device discovery failed: %w", err)
	}

	metrics.DevicesDiscovered.WithLabelValues(c.nodeName, c.Name()).Set(float64(len(result.Devices)))

	firstPoll := c.previousDevices == nil
	baselineRun := firstPoll && c.emitHealthyBaselines
	st := newEthPollState()

	c.collectDevicesAndPorts(result.Devices, st)
	events := c.buildEventsForPoll(st, firstPoll, baselineRun)
	c.logDiscoverySummaryIfChanged(st)

	if firstPoll {
		c.classifier.LogClassificationSummary()
	}

	c.previousDevices = st.currentDevices
	c.previousPorts = st.currentPorts
	c.emitHealthyBaselines = false

	c.persistState(ethLinkLayer, st.currentDevices, st.currentPorts)

	return events, nil
}

// collectDevicesAndPorts walks the discovered devices. VFs are already
// excluded by discovery; this filters unsupported vendors and management
// NICs. seenDevices tracks all physical devices for disappearance detection.
func (c *EthernetStateCheck) collectDevicesAndPorts(devices []discovery.IBDevice, st *ethPollState) {
	for _, dev := range devices {
		st.seenDevices[dev.Name] = true

		if !c.shouldMonitor(dev) {
			continue
		}

		st.currentDevices[dev.Name] = true

		card := c.classifier.PCICardOf(dev.Name)
		role := c.classifier.RoleOf(dev.Name)
		st.cardRole[card] = role

		for i := range dev.Ports {
			p := dev.Ports[i]
			if !discovery.IsEthernetPort(&p) {
				continue
			}

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

// buildEventsForPoll runs the per-port transition evaluation,
// disappearance checks, and the first-poll homogeneity check.
//
// baselineRun is true on the first poll after a boot-ID change and
// asks the per-port evaluator to emit healthy baselines for every
// currently-healthy port so stale platform conditions clear.
func (c *EthernetStateCheck) buildEventsForPoll(
	st *ethPollState, firstPoll, baselineRun bool,
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

// portTransitionEvents iterates every recorded port and emits events on
// health-boundary crossings.
func (c *EthernetStateCheck) portTransitionEvents(
	st *ethPollState, firstPoll, baselineRun bool, expectedCards map[string]struct{},
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for _, pi := range st.allPorts {
		if evt := c.evaluatePortTransition(pi, firstPoll, baselineRun, expectedCards, st.portCard); evt != nil {
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
	expectedCards map[string]struct{},
	portCard map[string]string,
) *pb.HealthEvent {
	prev, existed := c.previousPorts[pi.key]

	isHealthy := pi.snap.State == checks.IBStateActive && pi.snap.PhysicalState == checks.IBPhysLinkUp
	wasHealthy := existed && prev.State == checks.IBStateActive && prev.PhysicalState == checks.IBPhysLinkUp

	if existed && isHealthy == wasHealthy {
		return nil
	}

	if isHealthy {
		return c.healthyRecoveryEvent(pi, prev, existed, baselineRun)
	}

	return c.unhealthyEvent(pi, prev, firstPoll, expectedCards, portCard)
}

// healthyRecoveryEvent returns an IsHealthy=true event for port
// recoveries. On first-seen healthy ports it normally emits nothing
// (to avoid spamming healthy events on routine restarts) unless
// baselineRun is true, in which case it emits a healthy baseline so
// the platform clears stale FATAL conditions from the previous boot.
func (c *EthernetStateCheck) healthyRecoveryEvent(
	pi ethPortInfo, prev portSnapshot, existed, baselineRun bool,
) *pb.HealthEvent {
	if !existed && !baselineRun {
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

// unhealthyEvent returns the fatal event for a DOWN transition, or nil
// when the unhealthy state is a transient non-DOWN (INIT, ARMED).
// Expected-down first-poll ports are downgraded to non-fatal
// so the platform sees the state without acting on it.
func (c *EthernetStateCheck) unhealthyEvent(
	pi ethPortInfo, prev portSnapshot,
	firstPoll bool, expectedCards map[string]struct{}, portCard map[string]string,
) *pb.HealthEvent {
	if pi.snap.State != checks.IBStateDown {
		slog.Debug("RoCE port in non-ACTIVE state, ignoring",
			"device", pi.snap.Device, "port", pi.snap.Port,
			"state", pi.snap.State, "physState", pi.snap.PhysicalState,
		)

		return nil
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

	if firstPoll {
		card := portCard[pi.key]
		if _, expected := expectedCards[card]; expected {
			slog.Info("Suppressing first-poll fatal for expected-down RoCE port",
				"device", pi.snap.Device, "port", pi.snap.Port, "card", card)

			return c.portEvent(pi.snap.Device, pi.snap.Port, msg, false, false, pb.RecommendedAction_NONE)
		}
	}

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
