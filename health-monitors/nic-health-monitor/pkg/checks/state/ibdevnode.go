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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

// charDevKind identifies which InfiniBand character-device class an
// expected/observed entry belongs to.
type charDevKind string

const (
	kindIssm   charDevKind = "issm"
	kindUmad   charDevKind = "umad"
	kindUverbs charDevKind = "uverbs"

	// abiVersionEntry is the non-device file the kernel places in each
	// infiniband_mad / infiniband_verbs class directory; it must never be
	// treated as a character-device node.
	abiVersionEntry = "abi_version"

	// noPort is the sentinel port used for device-level entries (uverbs),
	// which are not associated with a specific port.
	noPort = 0
)

// charDevKey uniquely identifies an expected or missing character device.
// device is the IB device name (e.g. "mlx5_0"); port is the IB port for
// per-port kinds (issm/umad) and noPort for the device-level kind (uverbs).
type charDevKey struct {
	kind   charDevKind
	device string
	port   int
}

// InfiniBandCharDeviceCheck detects missing InfiniBand character-device
// nodes (issm/umad/uverbs) that leave a device's ports ACTIVE/LinkUp yet
// unusable by RDMA workloads (e.g. a pod failing with
// "lstat /dev/infiniband/issm9: no such file or directory").
//
// udev creates the /dev/infiniband/{issm,umad,uverbs}N nodes from the
// sysfs class entries under /sys/class/infiniband_mad and
// /sys/class/infiniband_verbs, so a missing class entry is an exact proxy
// for a missing /dev node — detectable via passive sysfs reads, no /dev
// mount required.
//
// The check is a per-device internal-consistency test, NOT an absolute
// expected-count test: for each discovered, in-scope device that exposes
// at least one InfiniBand-mode port it asserts that the device's own
// character devices exist (one uverbs per device; one umad and one issm
// per InfiniBand-mode port). It never assumes a fleet-wide device count,
// so it cannot false-positive the way an absolute expectation would. A
// device that is entirely absent from /sys/class/infiniband is out of
// scope here — that is the InfiniBandStateCheck's device-disappearance
// responsibility.
type InfiniBandCharDeviceCheck struct {
	nodeName           string
	reader             sysfs.Reader
	cfg                *config.Config
	classifier         *topology.Classifier
	processingStrategy pb.ProcessingStrategy
	state              *statefile.Manager

	// emitHealthyBaselines requests a check-scoped baseline clear on the
	// first complete poll after a host reboot (or a still-owed baseline
	// from a previous pod) so stale FATAL conditions from the prior boot
	// are wiped before current-boot faults are re-asserted.
	emitHealthyBaselines bool

	// previousMissing is the committed set of currently-missing character
	// devices; it drives transition (missing↔present) event emission so
	// steady states do not re-emit every poll. It is in-memory only: a
	// reboot re-baselines, and a non-reboot pod restart re-discovers the
	// current state on the first poll.
	previousMissing map[charDevKey]bool

	pending *charDevPollCommit
}

// charDevPollCommit stages a prepared poll until Commit makes it durable.
type charDevPollCommit struct {
	missing     map[charDevKey]bool
	baselineRan bool
}

var _ checks.TransactionalCheck = (*InfiniBandCharDeviceCheck)(nil)

// NewInfiniBandCharDeviceCheck wires the dependencies for the check. The
// bootIDChanged flag — typically stateManager.BootIDChanged() right after
// Load — plus any baseline still owed by a previous pod controls whether
// the first complete poll emits a check-scoped baseline clear.
func NewInfiniBandCharDeviceCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	stateManager *statefile.Manager,
	bootIDChanged bool,
) *InfiniBandCharDeviceCheck {
	pendingBaseline := bootIDChanged || stateManager.PendingBaseline(checks.InfiniBandCharDeviceCheckName)
	if pendingBaseline {
		stateManager.SetPendingBaseline(checks.InfiniBandCharDeviceCheckName)
	}

	return &InfiniBandCharDeviceCheck{
		nodeName:             nodeName,
		reader:               reader,
		cfg:                  cfg,
		classifier:           classifier,
		processingStrategy:   processingStrategy,
		state:                stateManager,
		emitHealthyBaselines: pendingBaseline,
	}
}

// Name returns the check identifier used by the orchestrator and in events.
func (c *InfiniBandCharDeviceCheck) Name() string { return checks.InfiniBandCharDeviceCheckName }

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *InfiniBandCharDeviceCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err != nil {
		return nil, err
	}

	c.Commit()

	return events, nil
}

// Prepare observes one poll and stages its candidate state without
// advancing the committed missing-set or persistent state.
func (c *InfiniBandCharDeviceCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	result, err := discovery.DiscoverDevicesWithOverride(
		c.reader, c.cfg.NicExclusionRegex, c.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, fmt.Errorf("device discovery failed: %w", err)
	}

	if !result.Complete {
		if c.previousMissing != nil {
			return nil, fmt.Errorf("device discovery incomplete: InfiniBand sysfs tree unavailable")
		}

		// Nodes without an InfiniBand tree stay quiet until one complete
		// enumeration succeeds.
		return nil, nil
	}

	expected := buildExpectedCharDevices(result.Devices, c.classifier)
	metrics.DevicesDiscovered.WithLabelValues(c.nodeName, c.Name()).Set(float64(expected.deviceCount))

	// The baseline reconciliation waits for a complete enumeration with no
	// unreadable devices: clearing while a device is unreadable would wipe
	// that device's previous-boot condition with nothing able to re-assert
	// it. Until then the baseline stays owed.
	baselineRun := c.emitHealthyBaselines && len(result.UnreadableDevices) == 0

	observed, uncertain, err := c.observeCharDevices(expected)
	if err != nil {
		return nil, err
	}

	if uncertain {
		// A class directory we need was entirely absent while devices that
		// should populate it exist: an uncertain observation, not evidence
		// of mass failure. Hold state (do not stage) so the baseline stays
		// owed and no spurious FATALs are emitted.
		slog.Warn("Holding InfiniBand char-device poll: class directory unavailable",
			"check", c.Name(), "node", c.nodeName)

		return nil, nil
	}

	currentMissing := diffMissing(expected, observed)
	events := c.buildEvents(currentMissing, baselineRun)

	c.pending = &charDevPollCommit{missing: currentMissing, baselineRan: baselineRun}

	return events, nil
}

// Commit installs and persists the most recently prepared state.
func (c *InfiniBandCharDeviceCheck) Commit() {
	if c.pending == nil {
		return
	}

	pending := c.pending
	c.pending = nil
	c.previousMissing = pending.missing

	if pending.baselineRan {
		c.emitHealthyBaselines = false
		c.state.ClearPendingBaseline(checks.InfiniBandCharDeviceCheckName)
	}
}

// Discard abandons a prepared poll after check or publication failure.
func (c *InfiniBandCharDeviceCheck) Discard() {
	c.pending = nil
}

// buildEvents turns the current missing-set into HealthEvents. On a
// baseline run it emits a check-scoped clear (wiping stale prior-boot
// conditions) followed by a FATAL for every still-missing node. On a
// normal run it emits a FATAL for each newly-missing node and a healthy
// recovery for each node that reappeared, staying silent on steady state.
func (c *InfiniBandCharDeviceCheck) buildEvents(
	currentMissing map[charDevKey]bool, baselineRun bool,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	if baselineRun {
		events = append(events, checks.NewBaselineClearEvent(
			c.nodeName, c.Name(),
			"InfiniBand character-device check: clearing stale conditions after reboot",
			c.processingStrategy,
		))

		for key := range currentMissing {
			events = append(events, c.missingEvent(key))
		}

		checks.EnsureClearPrecedesBatch(events)

		return events
	}

	for key := range currentMissing {
		if !c.previousMissing[key] {
			events = append(events, c.missingEvent(key))
		}
	}

	for key := range c.previousMissing {
		if !currentMissing[key] {
			events = append(events, c.recoveryEvent(key))
		}
	}

	return events
}

// expectedCharDevices is the per-device internal-consistency expectation
// derived from discovery: the character devices each in-scope device must
// expose given its InfiniBand-mode ports.
type expectedCharDevices struct {
	keys        map[charDevKey]bool
	needsMad    bool // any issm/umad expected → the mad class dir is required.
	needsVerbs  bool // any uverbs expected → the verbs class dir is required.
	deviceCount int
}

// buildExpectedCharDevices computes, for each eligible device that exposes
// at least one InfiniBand-mode port: one uverbs (device-level) plus one
// umad and one issm per InfiniBand-mode port. umad/issm are gated on the
// InfiniBand link layer because RoCE/Ethernet-mode ports legitimately have
// no issm node, so expecting them there would false-positive.
func buildExpectedCharDevices(
	devices []discovery.IBDevice, classifier *topology.Classifier,
) expectedCharDevices {
	exp := expectedCharDevices{keys: make(map[charDevKey]bool)}

	for i := range devices {
		dev := &devices[i]
		if !checks.EligibleDevice(dev, classifier) {
			continue
		}

		if !hasInfiniBandPort(dev) {
			continue
		}

		exp.deviceCount++
		exp.keys[charDevKey{kind: kindUverbs, device: dev.Name, port: noPort}] = true
		exp.needsVerbs = true

		for j := range dev.Ports {
			port := &dev.Ports[j]
			if !discovery.IsIBPort(port) {
				continue
			}

			exp.keys[charDevKey{kind: kindUmad, device: dev.Name, port: port.Port}] = true
			exp.keys[charDevKey{kind: kindIssm, device: dev.Name, port: port.Port}] = true
			exp.needsMad = true
		}
	}

	return exp
}

// hasInfiniBandPort reports whether the device exposes at least one
// InfiniBand-mode port (making it in-scope for this check).
func hasInfiniBandPort(dev *discovery.IBDevice) bool {
	for i := range dev.Ports {
		if discovery.IsIBPort(&dev.Ports[i]) {
			return true
		}
	}

	return false
}

// observedCharDevices is the set of character devices actually present in
// the two sysfs class directories, keyed identically to the expected set.
type observedCharDevices struct {
	present map[charDevKey]bool
}

// observeCharDevices reads the mad and verbs class directories and returns
// the present character devices. uncertain is true when a class directory
// that expected entries depend on is entirely absent (ErrNotExist): the
// observation cannot be trusted and the caller must hold rather than emit
// mass-missing FATALs. A non-ErrNotExist listing error is returned as err.
func (c *InfiniBandCharDeviceCheck) observeCharDevices(
	expected expectedCharDevices,
) (observedCharDevices, bool, error) {
	observed := observedCharDevices{present: make(map[charDevKey]bool)}

	madUncertain, err := c.readMadDir(observed.present)
	if err != nil {
		return observed, false, err
	}

	verbsUncertain, err := c.readVerbsDir(observed.present)
	if err != nil {
		return observed, false, err
	}

	uncertain := (madUncertain && expected.needsMad) || (verbsUncertain && expected.needsVerbs)

	return observed, uncertain, nil
}

// readMadDir enumerates /sys/class/infiniband_mad, recording each issm*/umad*
// entry as present under its (device, port) key. It returns uncertain=true
// when the directory does not exist.
func (c *InfiniBandCharDeviceCheck) readMadDir(present map[charDevKey]bool) (bool, error) {
	base := c.reader.IBMadBasePath()

	entries, err := c.reader.ListDirs(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}

		return false, fmt.Errorf("failed to list %s: %w", base, err)
	}

	for _, entry := range entries {
		var kind charDevKind

		switch {
		case entry == abiVersionEntry:
			continue
		case strings.HasPrefix(entry, string(kindIssm)):
			kind = kindIssm
		case strings.HasPrefix(entry, string(kindUmad)):
			kind = kindUmad
		default:
			continue
		}

		device, port, ok := c.readMadEntry(base, entry)
		if !ok {
			continue
		}

		present[charDevKey{kind: kind, device: device, port: port}] = true
	}

	return false, nil
}

// readMadEntry reads the ibdev and port files of a mad class entry. ok is
// false when either is unreadable (e.g. the abi_version file, or a
// transient race) so the caller skips it rather than fabricating a key.
func (c *InfiniBandCharDeviceCheck) readMadEntry(base, entry string) (string, int, bool) {
	device, err := c.reader.ReadFile(filepath.Join(base, entry, "ibdev"))
	if err != nil {
		return "", 0, false
	}

	portStr, err := c.reader.ReadFile(filepath.Join(base, entry, "port"))
	if err != nil {
		return "", 0, false
	}

	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil {
		return "", 0, false
	}

	return strings.TrimSpace(device), port, true
}

// readVerbsDir enumerates /sys/class/infiniband_verbs, recording each
// uverbs* entry as present under its device key. It returns uncertain=true
// when the directory does not exist.
func (c *InfiniBandCharDeviceCheck) readVerbsDir(present map[charDevKey]bool) (bool, error) {
	base := c.reader.IBVerbsBasePath()

	entries, err := c.reader.ListDirs(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}

		return false, fmt.Errorf("failed to list %s: %w", base, err)
	}

	for _, entry := range entries {
		if entry == abiVersionEntry || !strings.HasPrefix(entry, string(kindUverbs)) {
			continue
		}

		device, err := c.reader.ReadFile(filepath.Join(base, entry, "ibdev"))
		if err != nil {
			continue
		}

		present[charDevKey{kind: kindUverbs, device: strings.TrimSpace(device), port: noPort}] = true
	}

	return false, nil
}

// diffMissing returns the expected keys that are absent from the observed set.
func diffMissing(expected expectedCharDevices, observed observedCharDevices) map[charDevKey]bool {
	missing := make(map[charDevKey]bool)

	for key := range expected.keys {
		if !observed.present[key] {
			missing[key] = true
		}
	}

	return missing
}

// missingEvent builds the FATAL event for a missing character device. A
// missing node cannot be repaired from inside the workload — it requires
// host-level driver/udev/reboot intervention and pods hard-fail to start —
// so it recommends node replacement, matching how the real incident was
// resolved (spare migration).
func (c *InfiniBandCharDeviceCheck) missingEvent(key charDevKey) *pb.HealthEvent {
	metrics.StateCheckErrors.WithLabelValues(
		c.nodeName, c.Name(), key.device, discovery.PortEntityValue(key.port),
	).Inc()

	return checks.NewHealthEvent(
		c.nodeName, c.Name(), c.missingMessage(key), c.entitiesFor(key),
		true, false, pb.RecommendedAction_REPLACE_VM, c.processingStrategy,
	)
}

// recoveryEvent builds the healthy event emitted when a previously-missing
// character device reappears, so the platform can clear the node condition.
func (c *InfiniBandCharDeviceCheck) recoveryEvent(key charDevKey) *pb.HealthEvent {
	msg := fmt.Sprintf("InfiniBand character device %s for %s is present again", key.kind, c.entityDesc(key))

	return checks.NewHealthEvent(
		c.nodeName, c.Name(), msg, c.entitiesFor(key),
		false, true, pb.RecommendedAction_NONE, c.processingStrategy,
	)
}

// missingMessage renders the operator-facing description of a missing node.
func (c *InfiniBandCharDeviceCheck) missingMessage(key charDevKey) string {
	switch key.kind {
	case kindUverbs:
		return fmt.Sprintf(
			"Device %s: verbs character device (uverbs) missing from /sys/class/infiniband_verbs; "+
				"RDMA workloads cannot open /dev/infiniband/uverbs*", key.device)
	case kindIssm:
		return fmt.Sprintf(
			"Device %s port %d: issm character device missing from /sys/class/infiniband_mad "+
				"(expected for an InfiniBand-mode port); pods cannot open /dev/infiniband/issm*",
			key.device, key.port)
	case kindUmad:
		return fmt.Sprintf(
			"Device %s port %d: umad character device missing from /sys/class/infiniband_mad; "+
				"pods cannot open /dev/infiniband/umad*", key.device, key.port)
	default:
		return fmt.Sprintf("Device %s: character device %s missing", key.device, key.kind)
	}
}

// entitiesFor returns the entity references for an event: per-port kinds
// (issm/umad) pinpoint both card and port; the device-level kind (uverbs)
// references only the card.
func (c *InfiniBandCharDeviceCheck) entitiesFor(key charDevKey) []*pb.Entity {
	if key.kind == kindUverbs {
		return checks.DeviceEntities(key.device)
	}

	return checks.PortEntities(key.device, key.port)
}

// entityDesc renders a short human description of the entity for messages.
func (c *InfiniBandCharDeviceCheck) entityDesc(key charDevKey) string {
	if key.kind == kindUverbs {
		return fmt.Sprintf("device %s", key.device)
	}

	return fmt.Sprintf("device %s port %d", key.device, key.port)
}
