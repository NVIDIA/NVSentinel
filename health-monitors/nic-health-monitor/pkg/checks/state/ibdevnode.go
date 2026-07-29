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

	// charDevMissThreshold is the number of consecutive polls a character
	// device must be expected-but-missing before a FATAL is emitted. The
	// device list and the mad/verbs class directories are read at slightly
	// different instants, so a driver teardown/reload landing between the
	// reads can make every node of a device look missing for one poll;
	// requiring consecutive misses removes that race, mirroring the device
	// disappearance debounce (deviceMissThreshold).
	charDevMissThreshold = 3
)

// charDevKey uniquely identifies an expected or missing character device.
// device is the IB device name (e.g. "mlx5_0"); port is the IB port for
// per-port kinds (issm/umad) and noPort for the device-level kind (uverbs).
type charDevKey struct {
	kind   charDevKind
	device string
	port   int
}

// flagKey renders the persisted statefile key for a charDevKey.
func (k charDevKey) flagKey() string {
	return fmt.Sprintf("%s/%s_%d", k.kind, k.device, k.port)
}

// flagOf converts a charDevKey to its persisted representation.
func (k charDevKey) flagOf() statefile.MissingCharDeviceFlag {
	return statefile.MissingCharDeviceFlag{Kind: string(k.kind), Device: k.device, Port: k.port}
}

// keyOfFlag converts a persisted flag back to a charDevKey.
func keyOfFlag(f statefile.MissingCharDeviceFlag) charDevKey {
	return charDevKey{kind: charDevKind(f.Kind), device: f.Device, port: f.Port}
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
//
// Fault handling is latched and debounced: a node must be missing for
// charDevMissThreshold consecutive polls before the FATAL fires, the
// resulting latch persists across pod restarts and reboots via
// pkg/statefile, and it is released only on a positive observation of the
// node (recovery) or by the baseline clear. A latched key whose device
// drops out of discovery is HELD, not recovered: absence of the device is
// not evidence the character device healed.
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

	// latched is the committed set of character devices with a
	// missing-node FATAL outstanding downstream. Seeded from the
	// persisted statefile latch at construction so a recovery that
	// happens while the pod is down is still emitted by the next pod.
	latched map[charDevKey]bool

	// missStreak counts consecutive polls each expected key has been
	// missing; a FATAL fires when it reaches charDevMissThreshold.
	// In-memory only: losing it to a restart merely restarts the
	// debounce, which is the safe direction.
	missStreak map[charDevKey]int

	// seeded is true once one complete enumeration has succeeded, making
	// a later incomplete enumeration an error rather than a quiet skip.
	seeded bool

	// uncertainWarned suppresses the held-poll warning after its first
	// emission so an IB node without the ib_umad module does not log at
	// every poll; it resets when a certain observation succeeds.
	uncertainWarned bool

	// saveFailed keeps a failed statefile Save retrying on subsequent
	// commits even when nothing else changed.
	saveFailed bool

	pending *charDevPollCommit
}

// charDevPollCommit stages a prepared poll until Commit makes it durable.
type charDevPollCommit struct {
	latched     map[charDevKey]bool
	missStreak  map[charDevKey]int
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

	latched := make(map[charDevKey]bool)
	for _, flag := range stateManager.MissingCharDevices() {
		latched[keyOfFlag(flag)] = true
	}

	return &InfiniBandCharDeviceCheck{
		nodeName:             nodeName,
		reader:               reader,
		cfg:                  cfg,
		classifier:           classifier,
		processingStrategy:   processingStrategy,
		state:                stateManager,
		emitHealthyBaselines: pendingBaseline,
		latched:              latched,
		missStreak:           make(map[charDevKey]int),
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
// advancing the committed latch or persistent state.
func (c *InfiniBandCharDeviceCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	result, err := discovery.DiscoverDevicesWithOverride(
		c.reader, c.cfg.NicExclusionRegex, c.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, fmt.Errorf("device discovery failed: %w", err)
	}

	if !result.Complete {
		if c.seeded {
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
		// owed and no spurious FATALs are emitted. Warn once per
		// transition, not per poll: on a node without the ib_umad module
		// this state is permanent and would otherwise log every second.
		if !c.uncertainWarned {
			c.uncertainWarned = true

			slog.Warn("Holding InfiniBand char-device poll: class directory unavailable",
				"check", c.Name(), "node", c.nodeName)
		}

		return nil, nil
	}

	c.uncertainWarned = false

	events, nextLatched, nextStreak := c.evaluatePoll(expected, observed, baselineRun)
	c.pending = &charDevPollCommit{latched: nextLatched, missStreak: nextStreak, baselineRan: baselineRun}

	return events, nil
}

// Commit installs and persists the most recently prepared state.
func (c *InfiniBandCharDeviceCheck) Commit() {
	if c.pending == nil {
		return
	}

	pending := c.pending
	c.pending = nil
	c.latched = pending.latched
	c.missStreak = pending.missStreak
	c.seeded = true

	flags := make(map[string]statefile.MissingCharDeviceFlag, len(pending.latched))
	for key := range pending.latched {
		flags[key.flagKey()] = key.flagOf()
	}

	changed := c.state.UpdateMissingCharDevices(flags)

	if pending.baselineRan {
		c.emitHealthyBaselines = false
		c.state.ClearPendingBaseline(checks.InfiniBandCharDeviceCheckName)

		changed = true
	}

	// Persist when something changed or a previous Save failed: the
	// in-memory manager already carries the update, so without the retry
	// the on-disk state would stay stale until an unrelated change.
	if !changed && !c.saveFailed {
		return
	}

	if err := c.state.Save(); err != nil {
		c.saveFailed = true

		slog.Warn("Failed to persist state to disk",
			"check", c.Name(), "path", c.state.Path(), "error", err)

		return
	}

	c.saveFailed = false
}

// Discard abandons a prepared poll after check or publication failure.
func (c *InfiniBandCharDeviceCheck) Discard() {
	c.pending = nil
}

// evaluatePoll compares the expected and observed sets and produces this
// poll's events plus the candidate latch and debounce state.
//
// Transitions:
//   - expected + observed + latched   → recovery event, latch released
//     (the only path that releases a latch outside a baseline: recovery
//     requires the node to be positively observed, so a device that
//     drops out of discovery HOLDS its latch instead of fabricating a
//     recovery for a fault that never healed).
//   - expected + missing + unlatched  → debounce; FATAL + latch once the
//     key has been missing charDevMissThreshold consecutive polls.
//   - expected + missing + latched    → steady faulted state, silent.
//   - not expected + latched          → held: no event, latch kept.
//
// On a baseline run the check-scoped clear voids every downstream
// condition, so still-missing latched keys immediately re-assert their
// FATAL with fresh events, and latches whose keys are no longer expected
// are dropped with the clear — if their device returns still broken, the
// debounce re-fatals it within charDevMissThreshold polls.
func (c *InfiniBandCharDeviceCheck) evaluatePoll(
	expected expectedCharDevices, observed observedCharDevices, baselineRun bool,
) ([]*pb.HealthEvent, map[charDevKey]bool, map[charDevKey]int) {
	var events []*pb.HealthEvent

	nextLatched := make(map[charDevKey]bool)
	nextStreak := make(map[charDevKey]int)

	if baselineRun {
		events = append(events, checks.NewBaselineClearEvent(
			c.nodeName, c.Name(),
			"InfiniBand character-device check: clearing stale conditions after reboot",
			c.processingStrategy,
		))
	}

	for key := range expected.keys {
		if evt := c.evaluateKey(key, observed.present[key], baselineRun, nextLatched, nextStreak); evt != nil {
			events = append(events, evt)
		}
	}

	// Latched keys not expected this poll (device unreadable, absent, or
	// without IB ports): hold them — absence is not positive evidence of
	// recovery. On a baseline run they are dropped instead: the clear has
	// voided their downstream conditions.
	if !baselineRun {
		for key := range c.latched {
			if _, isExpected := expected.keys[key]; !isExpected {
				nextLatched[key] = true
			}
		}
	}

	if baselineRun {
		checks.EnsureClearPrecedesBatch(events)
	}

	return events, nextLatched, nextStreak
}

// evaluateKey applies the transition rules to one expected key, updating
// the candidate latch/streak maps and returning the event to emit, if any.
func (c *InfiniBandCharDeviceCheck) evaluateKey(
	key charDevKey, present, baselineRun bool,
	nextLatched map[charDevKey]bool, nextStreak map[charDevKey]int,
) *pb.HealthEvent {
	if present {
		if c.latched[key] && !baselineRun {
			return c.recoveryEvent(key)
		}

		return nil
	}

	if c.latched[key] {
		nextLatched[key] = true

		if baselineRun {
			// The clear just voided this key's condition; re-assert the
			// confirmed fault immediately rather than re-running the
			// debounce.
			return c.missingEvent(key)
		}

		return nil
	}

	streak := c.missStreak[key] + 1
	if streak >= charDevMissThreshold {
		nextLatched[key] = true

		return c.missingEvent(key)
	}

	nextStreak[key] = streak

	return nil
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

// missingEvent builds the FATAL event for a missing character device. A
// missing node cannot be repaired from inside the workload — it requires
// host-level driver/udev/reboot intervention and pods hard-fail to start —
// so it recommends node replacement, matching how the real incident was
// resolved (spare migration).
func (c *InfiniBandCharDeviceCheck) missingEvent(key charDevKey) *pb.HealthEvent {
	metrics.StateCheckErrors.WithLabelValues(
		c.nodeName, c.Name(), key.device, discovery.PortEntityValue(key.port),
	).Inc()

	return withCharDevCode(checks.NewHealthEvent(
		c.nodeName, c.Name(), c.missingMessage(key), c.entitiesFor(key),
		true, false, pb.RecommendedAction_REPLACE_VM, c.processingStrategy,
	), key.kind)
}

// recoveryEvent builds the healthy event emitted when a previously-missing
// character device reappears, so the platform can clear the node condition.
func (c *InfiniBandCharDeviceCheck) recoveryEvent(key charDevKey) *pb.HealthEvent {
	msg := fmt.Sprintf("InfiniBand character device %s for %s is present again", key.kind, c.entityDesc(key))

	return withCharDevCode(checks.NewHealthEvent(
		c.nodeName, c.Name(), msg, c.entitiesFor(key),
		false, true, pb.RecommendedAction_NONE, c.processingStrategy,
	), key.kind)
}

// withCharDevCode stamps the character-device kind onto an event as its
// ErrorCode. Downstream consumers scope condition clearing by ErrorCode:
// without it the issm and umad events for a port share one identity
// (check name + entities), so a umad recovery would also wipe a still-open
// issm condition — and since the check reports transitions, the issm fault
// would never be re-added until reboot. Only the check-scoped baseline
// clear is deliberately code-less, because it means "clear everything".
func withCharDevCode(evt *pb.HealthEvent, kind charDevKind) *pb.HealthEvent {
	evt.ErrorCode = []string{string(kind)}

	return evt
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
