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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// madEntry / verbsEntry describe one character-device class entry as it
// appears in /sys/class/infiniband_mad or /sys/class/infiniband_verbs:
// the directory name (issm3/umad3/uverbs0) plus the ibdev/port it maps to.
type madEntry struct {
	name  string
	ibdev string
	port  int
}

type verbsEntry struct {
	name  string
	ibdev string
}

// charDevFixture layers the two InfiniBand character-device class
// directories on top of a stubNode's sysfs topology. Its fields are read
// live by the MockReader closures, so a test can mutate mad/verbs between
// polls to exercise transition and recovery behaviour.
type charDevFixture struct {
	node     *stubNode
	mad      []madEntry
	verbs    []verbsEntry
	madErr   error
	verbsErr error
	noIBTree bool
}

func (f *charDevFixture) reader() *sysfs.MockReader {
	m := f.node.reader()
	origList := m.ListDirsFunc
	madBase := m.IBMadBasePath()
	verbsBase := m.IBVerbsBasePath()

	m.ListDirsFunc = func(path string) ([]string, error) {
		if f.noIBTree && path == m.IBBasePath() {
			return nil, os.ErrNotExist
		}

		switch path {
		case madBase:
			if f.madErr != nil {
				return nil, f.madErr
			}

			return append([]string{abiVersionEntry}, madNames(f.mad)...), nil
		case verbsBase:
			if f.verbsErr != nil {
				return nil, f.verbsErr
			}

			return append([]string{abiVersionEntry}, verbsNames(f.verbs)...), nil
		}

		return origList(path)
	}

	m.ReadFileFunc = func(path string) (string, error) {
		for _, e := range f.mad {
			if path == filepath.Join(madBase, e.name, "ibdev") {
				return e.ibdev, nil
			}

			if path == filepath.Join(madBase, e.name, "port") {
				return strconv.Itoa(e.port), nil
			}
		}

		for _, e := range f.verbs {
			if path == filepath.Join(verbsBase, e.name, "ibdev") {
				return e.ibdev, nil
			}
		}

		return "", nil
	}

	return m
}

func madNames(entries []madEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}

	return out
}

func verbsNames(entries []verbsEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}

	return out
}

// fullCharDevs returns the complete, correct character-device listing for
// a node: one uverbs per device, one umad per port (IB or Ethernet), and
// one issm per InfiniBand-mode port. Tests start from this and remove an
// entry to model a fault.
func fullCharDevs(node *stubNode) ([]madEntry, []verbsEntry) {
	var (
		mad   []madEntry
		verbs []verbsEntry
		i     int
	)

	for name, d := range node.ib {
		verbs = append(verbs, verbsEntry{name: fmt.Sprintf("uverbs%d", i), ibdev: name})

		for p, port := range d.ports {
			i++
			mad = append(mad, madEntry{name: fmt.Sprintf("umad%d", i), ibdev: name, port: p})

			if strings.EqualFold(port.linkLayer, "InfiniBand") {
				mad = append(mad, madEntry{name: fmt.Sprintf("issm%d", i), ibdev: name, port: p})
			}
		}

		i++
	}

	return mad, verbs
}

// singleIBDevice returns the stubDevice used by singleIBNode, so tests
// that remove/re-add the device between polls can share the definition.
func singleIBDevice() *stubDevice {
	return &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	}
}

// singleIBNode returns a stubNode with one compute IB device (mlx5_0)
// exposing a single ACTIVE/LinkUp InfiniBand port.
func singleIBNode(t *testing.T) (*stubNode, *charDevFixture) {
	t.Helper()

	node := newStubNode().addIB("mlx5_0", singleIBDevice())
	mad, verbs := fullCharDevs(node)

	return node, &charDevFixture{node: node, mad: mad, verbs: verbs}
}

func newCharDevCheck(
	t *testing.T, f *charDevFixture, reader sysfs.Reader, bootIDChanged bool,
) *InfiniBandCharDeviceCheck {
	t.Helper()

	return newCharDevCheckWithManager(t, f, reader, freshStateManager(t), bootIDChanged)
}

func newCharDevCheckWithManager(
	t *testing.T, f *charDevFixture, reader sysfs.Reader,
	mgr *statefile.Manager, bootIDChanged bool,
) *InfiniBandCharDeviceCheck {
	t.Helper()

	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	return NewInfiniBandCharDeviceCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, bootIDChanged)
}

// runQuietPolls runs the check n times, requiring every poll to emit no
// events (the debounce window before a fatal fires).
func runQuietPolls(t *testing.T, check *InfiniBandCharDeviceCheck, n int) {
	t.Helper()

	for i := range n {
		events, err := check.Run()
		require.NoError(t, err)
		assert.Emptyf(t, events, "poll %d of %d should be silent (debounce window)", i+1, n)
	}
}

func fatalEvents(events []*pb.HealthEvent) []*pb.HealthEvent {
	var out []*pb.HealthEvent

	for _, e := range events {
		if e.IsFatal {
			out = append(out, e)
		}
	}

	return out
}

func TestIBCharDev_AllPresentHealthy(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "a device with all char devices present must not emit events")
}

func TestIBCharDev_IssmMissingIsFatalAfterDebounce(t *testing.T) {
	t.Parallel()

	// The production ticket: an ACTIVE/LinkUp InfiniBand port whose issm
	// character device never materialised, so pods fail with
	// "lstat /dev/infiniband/issm9: no such file or directory". The fatal
	// fires only after charDevMissThreshold consecutive missing polls.
	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.True(t, evt.IsFatal, "missing issm must be fatal")
	assert.False(t, evt.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Equal(t, checks.InfiniBandCharDeviceCheckName, evt.CheckName)
	assert.Equal(t, []string{"issm"}, evt.ErrorCode, "fatal must carry the per-kind error code")
	assert.Contains(t, evt.Message, "issm")
	assertPortEntities(t, evt, "mlx5_0", 1)

	// Steady faulted state stays silent.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "latched fault must not re-emit")
}

func TestIBCharDev_OnePollBlipIsSwallowed(t *testing.T) {
	t.Parallel()

	// A single bad poll (driver teardown between the device-list read and
	// the class-dir reads) must not fatal: the debounce resets when the
	// node is observed again.
	_, f := singleIBNode(t)
	fullMad := f.mad
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run() // one missing poll — inside the debounce window
	require.NoError(t, err)
	assert.Empty(t, events)

	f.mad = fullMad

	for range 2 * charDevMissThreshold {
		events, err = check.Run()
		require.NoError(t, err)
		assert.Empty(t, events, "a one-poll blip must never produce a fatal or recovery")
	}
}

func TestIBCharDev_RoCEPortExpectsNoIssm(t *testing.T) {
	t.Parallel()

	// A pure-RoCE device (single Ethernet-mode port) has no InfiniBand
	// port, so it is out of scope entirely — no issm is expected and the
	// check stays silent even though only umad/uverbs exist.
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"},
		},
	})
	f := &charDevFixture{
		node:  node,
		mad:   []madEntry{{name: "umad0", ibdev: "mlx5_0", port: 1}},
		verbs: []verbsEntry{{name: "uverbs0", ibdev: "mlx5_0"}},
	}
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)
}

func TestIBCharDev_MixedDeviceOnlyIBPortNeedsIssm(t *testing.T) {
	t.Parallel()

	// A device with one IB port and one Ethernet port. issm is expected
	// only for the IB port; the Ethernet port legitimately has umad but no
	// issm. All expected entries are present, so no event fires — proving
	// the per-port IB gate does not false-positive on the Ethernet port.
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
			2: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"},
		},
	})
	f := &charDevFixture{
		node: node,
		mad: []madEntry{
			{name: "umad0", ibdev: "mlx5_0", port: 1},
			{name: "issm0", ibdev: "mlx5_0", port: 1},
			{name: "umad1", ibdev: "mlx5_0", port: 2}, // Ethernet port: umad, no issm.
		},
		verbs: []verbsEntry{{name: "uverbs0", ibdev: "mlx5_0"}},
	}
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)
}

func TestIBCharDev_UmadMissingIsFatal(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "umad")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Equal(t, []string{"umad"}, events[0].ErrorCode)
	assert.Contains(t, events[0].Message, "umad")
	assertPortEntities(t, events[0], "mlx5_0", 1)
}

func TestIBCharDev_UverbsMissingIsFatalWithDeviceEntity(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	f.verbs = nil // drop the only uverbs entry.
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.True(t, evt.IsFatal)
	assert.Equal(t, []string{"uverbs"}, evt.ErrorCode)
	assert.Contains(t, evt.Message, "uverbs")
	require.Len(t, evt.EntitiesImpacted, 1, "uverbs is a device-level entity")
	assert.Equal(t, checks.EntityTypeNIC, evt.EntitiesImpacted[0].EntityType)
	assert.Equal(t, "mlx5_0", evt.EntitiesImpacted[0].EntityValue)
}

func TestIBCharDev_TransitionThenRecoveryThenQuiet(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	fullMad := f.mad
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	// Confirmed missing → one FATAL.
	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)

	// issm restored → one healthy recovery, no re-emit of the fault.
	f.mad = fullMad
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assert.Equal(t, []string{"issm"}, events[0].ErrorCode, "recovery must carry the per-kind error code")
	assertPortEntities(t, events[0], "mlx5_0", 1)

	// Steady healthy → silent.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "steady state must not re-emit")
}

func TestIBCharDev_DeviceBlipHoldsLatch(t *testing.T) {
	t.Parallel()

	// The reviewer-reported false-recovery bug: after a fault is latched,
	// the device momentarily drops out of /sys/class/infiniband (firmware
	// reset). Its keys leave the expected set — that absence must HOLD the
	// latch, not emit a "present again" recovery for a fault that never
	// healed.
	node, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, fatalEvents(events), 1, "fault must latch first")

	// Device disappears from enumeration (discovery still Complete).
	delete(node.ib, "mlx5_0")

	for range 2 {
		events, err = check.Run()
		require.NoError(t, err)
		assert.Empty(t, events, "device absence must hold the latch, not fabricate a recovery")
	}

	// Device returns, issm still missing: steady faulted state, silent —
	// the latch survived the blip.
	node.ib["mlx5_0"] = singleIBDevice()
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "still-missing fault must stay latched after the blip, not re-fire")

	// Only a positive observation releases it.
	f.mad, _ = fullCharDevs(node)
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy, "recovery requires positive observation")
}

func TestIBCharDev_LatchSurvivesPodRestart(t *testing.T) {
	t.Parallel()

	// The recovery-while-pod-down orphan: a fault is latched, the pod
	// dies, the char device comes back while no monitor is running. The
	// next pod must seed the latch from the state file and emit the
	// recovery, or the REPLACE_VM condition would stay on the node until
	// the next reboot.
	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")

	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheckWithManager(t, f, reader, mgr, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, fatalEvents(events), 1)

	// Pod restarts on the same boot; the char device healed while it was
	// down.
	f.mad, _ = fullCharDevs(f.node)

	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, mgr2.Load())
	require.False(t, mgr2.BootIDChanged())

	check2 := newCharDevCheckWithManager(t, f, reader, mgr2, false)

	events, err = check2.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "restarted pod must emit the recovery for the persisted latch")
	assert.True(t, events[0].IsHealthy)
	assert.Equal(t, []string{"issm"}, events[0].ErrorCode)
}

func TestIBCharDev_LatchSurvivesScopeChange(t *testing.T) {
	t.Parallel()

	// A discovery-scope change (e.g. enabling the inclusion override)
	// resets port/device state but must preserve the missing-char-device
	// latch: the FATAL is still holding a condition downstream, and the
	// check rebuilds its latch map from the persisted state at
	// construction.
	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")

	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheckWithManager(t, f, reader, mgr, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, fatalEvents(events), 1)

	// Reload under a different discovery scope, same boot.
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope("incl=mlx5_.*;excl=")
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.ScopeChanged())
	require.False(t, mgr2.BootIDChanged())

	assert.NotEmpty(t, mgr2.MissingCharDevices(),
		"scope change must preserve the missing-char-device latch")

	// The node healed while the scope changed: the new pod's check must
	// still emit the recovery for the preserved latch.
	f.mad, _ = fullCharDevs(f.node)
	check2 := newCharDevCheckWithManager(t, f, reader, mgr2, mgr2.BootIDChanged())

	events, err = check2.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy, "preserved latch must release via recovery")
	assert.Equal(t, []string{"issm"}, events[0].ErrorCode)
}

func TestIBCharDev_NoDuplicateBaselineAfterRestart(t *testing.T) {
	t.Parallel()

	// Commit must persist the consumed pending-baseline flag: without the
	// Save, every pod restart within the same boot would re-emit the
	// baseline clear.
	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")

	_, f := singleIBNode(t)
	reader := f.reader()
	check := newCharDevCheckWithManager(t, f, reader, mgr, true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "baseline poll emits the check-scoped clear")
	assert.True(t, events[0].IsHealthy)
	assert.Empty(t, events[0].EntitiesImpacted)

	// Same boot, new pod: the persisted flag must be consumed.
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, mgr2.Load())
	require.False(t, mgr2.BootIDChanged())
	assert.False(t, mgr2.PendingBaseline(checks.InfiniBandCharDeviceCheckName),
		"consumed baseline must be persisted by Commit")

	check2 := newCharDevCheckWithManager(t, f, reader, mgr2, false)

	events, err = check2.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "restart on the same boot must not re-emit the baseline clear")
}

func TestIBCharDev_AbiVersionEntryIgnored(t *testing.T) {
	t.Parallel()

	// fullCharDevs plus the abi_version file the fixture always lists must
	// not be mistaken for a device node.
	_, f := singleIBNode(t)
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)
}

func TestIBCharDev_ClassDirAbsentIsUncertain(t *testing.T) {
	t.Parallel()

	// The whole mad class directory is absent while an IB device exists:
	// an uncertain observation, not evidence of failure. The check must
	// hold — emit nothing — rather than fabricate mass-missing FATALs.
	_, f := singleIBNode(t)
	f.madErr = os.ErrNotExist
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)

	// When the directory returns (with issm still missing) the fault is
	// then reported after the debounce — proving the earlier polls held
	// rather than latched.
	f.madErr = nil
	f.mad = dropKind(f.mad, "issm")

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

func TestIBCharDev_ClassDirListingErrorPropagates(t *testing.T) {
	t.Parallel()

	// A non-ENOENT error listing the mad directory is a genuine read
	// failure (not "no IB MAD devices"); it must propagate so the poll is
	// discarded rather than treated as an empty/uncertain observation.
	_, f := singleIBNode(t)
	f.madErr = fmt.Errorf("permission denied")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	_, err := check.Run()
	require.Error(t, err, "a non-ENOENT class-dir listing error must propagate")
}

func TestIBCharDev_IncompleteDiscoveryBail(t *testing.T) {
	t.Parallel()

	// First poll on a node whose IB tree is absent: stay quiet, no error.
	_, f := singleIBNode(t)
	f.noIBTree = true
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// After a complete poll seeds state, a later disappearance of the tree
	// is an incomplete observation and must error (so the poll is discarded
	// rather than advancing state on a partial read).
	f.noIBTree = false
	_, err = check.Run()
	require.NoError(t, err)

	f.noIBTree = true
	_, err = check.Run()
	require.Error(t, err, "losing the IB tree after seeding state must error")
}

func TestIBCharDev_BaselineClearThenDebouncedReassert(t *testing.T) {
	t.Parallel()

	// A reboot (bootIDChanged=true) with issm missing and no prior latch:
	// the first complete poll emits the check-scoped clear; the fault then
	// confirms through the normal debounce.
	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "baseline poll emits only the clear (fault not yet confirmed)")
	assert.True(t, events[0].IsHealthy)
	assert.Empty(t, events[0].EntitiesImpacted, "baseline clear is check-scoped (no entities)")

	runQuietPolls(t, check, charDevMissThreshold-2)

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "fault confirms through the debounce after the clear")
}

func TestIBCharDev_BaselineReassertsLatchedFaultImmediately(t *testing.T) {
	t.Parallel()

	// A fault confirmed before a reboot: the persisted latch survives, so
	// the baseline poll emits the clear followed immediately by the
	// re-asserted FATAL (no debounce — the fault was already confirmed).
	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")

	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheckWithManager(t, f, reader, mgr, false)

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, fatalEvents(events), 1)

	// Reboot: new manager on the same state file sees a new boot ID.
	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-2\n"), 0o644))

	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.BootIDChanged())

	check2 := newCharDevCheckWithManager(t, f, reader, mgr2, mgr2.BootIDChanged())

	events, err = check2.Run()
	require.NoError(t, err)
	require.Len(t, events, 2, "baseline must emit clear + immediate re-assert of the latched fault")

	clear := events[0]
	assert.True(t, clear.IsHealthy, "baseline clear must be healthy")
	assert.Empty(t, clear.EntitiesImpacted)
	assert.True(t, clear.GeneratedTimestamp.AsTime().Before(events[1].GeneratedTimestamp.AsTime()),
		"clear must sort before the fault it precedes")

	require.Len(t, fatalEvents(events), 1)
	assert.Equal(t, []string{"issm"}, fatalEvents(events)[0].ErrorCode)

	// The baseline is consumed: a subsequent poll is a quiet steady state.
	events, err = check2.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestIBCharDev_VirtualFunctionSkipped(t *testing.T) {
	t.Parallel()

	// A VF with a missing issm must not fire — discovery excludes VFs.
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.1",
		numaNode:   0,
		isVF:       true,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})
	f := &charDevFixture{
		node:  node,
		mad:   []madEntry{{name: "umad0", ibdev: "mlx5_0", port: 1}}, // no issm.
		verbs: []verbsEntry{{name: "uverbs0", ibdev: "mlx5_0"}},
	}
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)
}

func TestIBCharDev_UnsupportedVendorExcluded(t *testing.T) {
	t.Parallel()

	// EligibleDevice drops non-Mellanox vendors before any per-device work,
	// so a missing issm on an unsupported card must not fire.
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		vendor:     "0x8086", // Intel — unsupported.
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})
	f := &charDevFixture{
		node:  node,
		mad:   []madEntry{{name: "umad0", ibdev: "mlx5_0", port: 1}}, // no issm.
		verbs: []verbsEntry{{name: "uverbs0", ibdev: "mlx5_0"}},
	}
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	runQuietPolls(t, check, 2*charDevMissThreshold)
}

// dropKind removes every mad entry whose directory name starts with the
// given kind prefix (e.g. "issm" or "umad").
func dropKind(entries []madEntry, kind string) []madEntry {
	out := make([]madEntry, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.name, kind) {
			out = append(out, e)
		}
	}

	return out
}

func assertPortEntities(t *testing.T, evt *pb.HealthEvent, device string, port int) {
	t.Helper()
	require.Len(t, evt.EntitiesImpacted, 2)
	assert.Equal(t, checks.EntityTypeNIC, evt.EntitiesImpacted[0].EntityType)
	assert.Equal(t, device, evt.EntitiesImpacted[0].EntityValue)
	assert.Equal(t, checks.EntityTypePort, evt.EntitiesImpacted[1].EntityType)
	assert.Equal(t, strconv.Itoa(port), evt.EntitiesImpacted[1].EntityValue)
}
