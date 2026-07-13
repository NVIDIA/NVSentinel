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

	strategy linkLayerStrategy
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
func (b *baseStateCheck) detectDeviceDisappearance(current map[string]bool) []*pb.HealthEvent {
	if b.previousDevices == nil {
		return nil
	}

	var events []*pb.HealthEvent

	for device := range b.previousDevices {
		if current[device] {
			continue
		}

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

	return events
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

	if !portsChanged && !latchChanged {
		return
	}

	if err := b.state.Save(); err != nil {
		slog.Warn("Failed to persist state to disk",
			"linkLayer", linkLayer, "path", b.state.Path(), "error", err)
	}
}
