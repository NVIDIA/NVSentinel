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

// singleIBNode returns a stubNode with one compute IB device (mlx5_0)
// exposing a single ACTIVE/LinkUp InfiniBand port, plus a classifier that
// keeps it in scope.
func singleIBNode(t *testing.T) (*stubNode, *charDevFixture) {
	t.Helper()

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})
	mad, verbs := fullCharDevs(node)

	return node, &charDevFixture{node: node, mad: mad, verbs: verbs}
}

func newCharDevCheck(
	t *testing.T, f *charDevFixture, reader sysfs.Reader, bootIDChanged bool,
) *InfiniBandCharDeviceCheck {
	t.Helper()

	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	return NewInfiniBandCharDeviceCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), bootIDChanged)
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

func TestIBCharDev_IssmMissingIsFatal(t *testing.T) {
	t.Parallel()

	// The production ticket: an ACTIVE/LinkUp InfiniBand port whose issm
	// character device never materialised, so pods fail with
	// "lstat /dev/infiniband/issm9: no such file or directory".
	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.True(t, evt.IsFatal, "missing issm must be fatal")
	assert.False(t, evt.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Equal(t, checks.InfiniBandCharDeviceCheckName, evt.CheckName)
	assert.Contains(t, evt.Message, "issm")
	assertPortEntities(t, evt, "mlx5_0", 1)
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

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "RoCE-only device must not expect issm")
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

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "Ethernet port must not require issm on a mixed device")
}

func TestIBCharDev_UmadMissingIsFatal(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "umad")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Contains(t, events[0].Message, "umad")
	assertPortEntities(t, events[0], "mlx5_0", 1)
}

func TestIBCharDev_UverbsMissingIsFatalWithDeviceEntity(t *testing.T) {
	t.Parallel()

	_, f := singleIBNode(t)
	f.verbs = nil // drop the only uverbs entry.
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.True(t, evt.IsFatal)
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

	// Poll 1: issm missing → one FATAL.
	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)

	// Poll 2: issm restored → one healthy recovery, no re-emit of the fault.
	f.mad = fullMad
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assertPortEntities(t, events[0], "mlx5_0", 1)

	// Poll 3: steady healthy → silent.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "steady state must not re-emit")
}

func TestIBCharDev_AbiVersionEntryIgnored(t *testing.T) {
	t.Parallel()

	// fullCharDevs plus the abi_version file the fixture always lists must
	// not be mistaken for a device node.
	_, f := singleIBNode(t)
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "abi_version must not be treated as a char device")
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

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "absent class dir must be treated as uncertain, not mass-missing")

	// When the directory returns (with issm still missing) the fault is
	// then reported — proving the earlier poll held rather than latched.
	f.madErr = nil
	f.mad = dropKind(f.mad, "issm")
	events, err = check.Run()
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

func TestIBCharDev_BaselineRunClearsThenReasserts(t *testing.T) {
	t.Parallel()

	// A reboot (bootIDChanged=true) with issm still missing: the batch must
	// lead with a check-scoped clear (empty entities) that wipes stale
	// prior-boot conditions, followed by the current-boot FATAL.
	_, f := singleIBNode(t)
	f.mad = dropKind(f.mad, "issm")
	reader := f.reader()
	check := newCharDevCheck(t, f, reader, true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 2)

	clear := events[0]
	assert.True(t, clear.IsHealthy, "baseline clear must be healthy")
	assert.Empty(t, clear.EntitiesImpacted, "baseline clear is check-scoped (no entities)")
	assert.True(t, clear.GeneratedTimestamp.AsTime().Before(events[1].GeneratedTimestamp.AsTime()),
		"clear must sort before the fault it precedes")

	assert.Len(t, fatalEvents(events), 1, "the still-missing issm must be re-asserted")

	// The baseline is consumed after the first poll: a subsequent poll is a
	// normal run with no clear.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "second poll must be a normal (non-baseline) steady state")
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

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "VFs are excluded from discovery and must not be checked")
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

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "unsupported vendor must be out of scope")
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
