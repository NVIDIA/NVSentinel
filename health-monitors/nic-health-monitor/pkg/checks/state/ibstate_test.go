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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// stubNode is a fluent builder for a MockReader that describes a simple
// single-NIC, single-port InfiniBand host in sysfs.
type stubNode struct {
	ib   map[string]*stubDevice
	nets map[string]string
}

type stubDevice struct {
	vendor     string
	hca        string
	fw         string
	netDev     string
	numaNode   int
	pciAddress string
	ports      map[int]stubPort
	isVF       bool

	// portsUnreadable makes the device enumerable but unparseable: the
	// ports directory listing fails, so discovery reports it in
	// UnreadableDevices (exercises the first-poll deferral).
	portsUnreadable bool

	// attrReadsFail fails the per-port attribute reads (state,
	// phys_state, link_layer) while the ports directory still lists —
	// a transient sysfs error mid-poll. Discovery must classify the
	// device unreadable rather than fabricate empty observations.
	attrReadsFail bool
}

type stubPort struct {
	state     string
	physState string
	linkLayer string
}

func newStubNode() *stubNode {
	return &stubNode{
		ib:   make(map[string]*stubDevice),
		nets: make(map[string]string),
	}
}

// addIB adds an InfiniBand device with one or more ports.
func (n *stubNode) addIB(name string, d *stubDevice) *stubNode {
	n.ib[name] = d
	return n
}

// reader builds a MockReader driving the stub topology.
func (n *stubNode) reader() *sysfs.MockReader {
	m := &sysfs.MockReader{IBBase: "/sys/class/infiniband", NetBase: "/sys/class/net"}
	n.wireDirectoryListing(m)
	n.wirePortReads(m)
	n.wireDeviceReads(m)
	n.wireNetReads(m)

	return m
}

func (n *stubNode) wireDirectoryListing(m *sysfs.MockReader) {
	m.ListDirsFunc = func(path string) ([]string, error) {
		switch {
		case path == "/sys/class/infiniband":
			names := make([]string, 0, len(n.ib))
			for k := range n.ib {
				names = append(names, k)
			}

			return names, nil

		case strings.HasSuffix(path, "/ports"):
			return n.listPortsFor(path)

		case strings.HasSuffix(path, "/device/net"):
			return n.listNetDevFor(path)

		case strings.HasSuffix(path, "/device/infiniband"):
			return n.listIBDeviceForNetDev(path)
		}

		return nil, nil
	}
}

func (n *stubNode) listPortsFor(path string) ([]string, error) {
	parts := strings.Split(path, "/")
	dev := parts[len(parts)-2]

	d := n.ib[dev]
	if d == nil {
		return nil, nil
	}

	if d.portsUnreadable {
		return nil, fmt.Errorf("ports directory unreadable for %s", dev)
	}

	ports := make([]string, 0, len(d.ports))
	for p := range d.ports {
		ports = append(ports, strconv.Itoa(p))
	}

	return ports, nil
}

func (n *stubNode) listNetDevFor(path string) ([]string, error) {
	parts := strings.Split(path, "/")
	dev := parts[len(parts)-3]

	d := n.ib[dev]
	if d == nil || d.netDev == "" {
		return nil, nil
	}

	return []string{d.netDev}, nil
}

func (n *stubNode) listIBDeviceForNetDev(path string) ([]string, error) {
	parts := strings.Split(path, "/")
	iface := parts[len(parts)-3]

	for dev, d := range n.ib {
		if d != nil && d.netDev == iface {
			return []string{dev}, nil
		}
	}

	return nil, nil
}

func (n *stubNode) wirePortReads(m *sysfs.MockReader) {
	m.ReadIBPortStateFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		if d.attrReadsFail {
			return "", fmt.Errorf("state read failed for %s port %d", device, port)
		}

		return fmt.Sprintf("4: %s", d.ports[port].state), nil
	}

	m.ReadIBPortPhysStateFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		if d.attrReadsFail {
			return "", fmt.Errorf("phys_state read failed for %s port %d", device, port)
		}

		return fmt.Sprintf("5: %s", d.ports[port].physState), nil
	}

	m.ReadIBPortLinkLayerFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		if d.attrReadsFail {
			return "", fmt.Errorf("link_layer read failed for %s port %d", device, port)
		}

		return d.ports[port].linkLayer, nil
	}
}

func (n *stubNode) wireDeviceReads(m *sysfs.MockReader) {
	m.ReadIBDeviceFieldFunc = func(device, field string) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		switch field {
		case "hca_type":
			return d.hca, nil
		case "fw_ver":
			return d.fw, nil
		case "device/vendor":
			if d.vendor == "" {
				return "0x15b3", nil
			}

			return d.vendor, nil
		}

		return "", nil
	}

	m.ReadIBDeviceNUMAFunc = func(device string) (int, error) {
		d := n.ib[device]
		if d == nil {
			return -1, nil
		}

		return d.numaNode, nil
	}

	m.ReadPCIAddressFunc = func(device string) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		return d.pciAddress, nil
	}

	// GPU PCI NUMA lookup: we assume all GPUs are on NUMA 0 unless a test
	m.IsVirtualFunctionFunc = func(device string) bool {
		d := n.ib[device]
		return d != nil && d.isVF
	}
}

func (n *stubNode) wireNetReads(m *sysfs.MockReader) {
	m.ReadNetOperStateFunc = func(iface string) (string, error) {
		return n.nets[iface], nil
	}
}

// freshStateManager returns a temp-file-backed statefile.Manager for
// unit tests that don't exercise persistence directly.
func freshStateManager(t *testing.T) *statefile.Manager {
	t.Helper()
	mgr, _, _ := newStateManagerForTest(t, "unit")
	return mgr
}

// Classifier, using the given reader's PCI NUMA reads to populate
// gpuNUMASet.
func buildClassifier(
	t *testing.T,
	reader sysfs.Reader,
	gpuPCIs []string,
	topo map[string][]string,
	procNetRoutePath ...string,
) *topology.Classifier {
	t.Helper()

	gpus := make([]model.GPUInfo, 0, len(gpuPCIs))
	for _, pci := range gpuPCIs {
		gpus = append(gpus, model.GPUInfo{PCIAddress: pci, NUMANode: 0})
	}

	meta := &model.GPUMetadata{
		GPUs:        gpus,
		NICTopology: topo,
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "gpu_metadata.json")
	data, err := json.MarshalIndent(meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	c, err := topology.LoadFromMetadata(path, reader, procNetRoutePath...)
	require.NoError(t, err)

	return c
}

func writeProcNetRoute(t *testing.T, defaultIface string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "route")

	content := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
	if defaultIface != "" {
		content += defaultIface + "\t00000000\t01000A0A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	}

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	return path
}

func TestIBState_NoEventOnFirstHealthyPoll(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "first poll on a healthy port should not emit events")
}

func TestIBState_FirstPollDownSingletonIsSuppressed(t *testing.T) {
	// A single card with a never-seen-healthy DOWN port has no peers to
	// compare against, so there is no evidence the port is supposed to be
	// up (it may be intentionally disabled/unprovisioned, like the unused
	// Aux frontend port on OCI BM.GPU.H100.8). The state is logged locally
	// but no external HealthEvent is emitted.
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "singleton card has no peer evidence; first-poll DOWN must be suppressed")
}

func TestIBState_FirstPollDownBelowModeStaysFatal(t *testing.T) {
	// Three single-port compute cards, one DOWN at first poll. The DOWN
	// card is below the decisive mode (1 active) of its role group —
	// positive peer evidence of failure — so both the per-port event and
	// the card homogeneity event must stay fatal.
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_2", &stubDevice{
			pciAddress: "0000:49:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}},
		})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}, "mlx5_2": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)

	var portFatal, cardFatal bool

	for _, e := range events {
		if !e.IsFatal {
			continue
		}

		if strings.Contains(e.Message, "active ports, expected") {
			cardFatal = true
			continue
		}

		for _, ent := range e.EntitiesImpacted {
			if ent.EntityType == checks.EntityTypeNIC && ent.EntityValue == "mlx5_2" {
				portFatal = true
			}
		}
	}

	assert.True(t, portFatal, "below-mode card's DOWN port must stay fatal on first poll")
	assert.True(t, cardFatal, "below-mode card must also raise the homogeneity fatal")
}

func TestIBState_HealthyToUnhealthyBoundaryEmitsEvent(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0",
		numaNode:   0,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	// Poll 1: healthy — no events expected.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Update the mock to reflect a DOWN port, then poll again.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.False(t, events[0].IsHealthy)

	// Bring the port back up — expect a single healthy event.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.False(t, events[0].IsFatal)
	assert.True(t, events[0].IsHealthy)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)

	// Poll again with the same healthy state — no new events.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestIBState_UnhealthySeverityEscalationEmitsFatal(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	node.ib["mlx5_0"].ports[1] = stubPort{state: "INIT", physState: "Polling", linkLayer: "InfiniBand"}
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.False(t, events[0].IsFatal)

	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

func TestIBState_UnreadableDeviceRetainsLastKnownState(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	})

	reader := node.reader()
	originalListDirs := reader.ListDirsFunc
	unreadable := false
	reader.ListDirsFunc = func(path string) ([]string, error) {
		if unreadable && strings.HasSuffix(path, "/mlx5_0/ports") {
			return nil, errors.New("transient sysfs read failure")
		}

		return originalListDirs(path)
	}

	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	unreadable = true
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "an unreadable listed device is unknown, not disappeared")

	unreadable = false
	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "last-known healthy state must survive the incomplete poll")
	assert.True(t, events[0].IsFatal)
}

func TestIBState_TopLevelDiscoveryGapDoesNotAdvanceState(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	})

	reader := node.reader()
	originalListDirs := reader.ListDirsFunc
	unavailable := false
	reader.ListDirsFunc = func(path string) ([]string, error) {
		if unavailable && path == reader.IBBase {
			return nil, os.ErrNotExist
		}

		return originalListDirs(path)
	}

	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	_, err := check.Run()
	require.NoError(t, err)

	unavailable = true
	_, err = check.Run()
	require.ErrorContains(t, err, "device discovery incomplete")

	unavailable = false
	node.ib["mlx5_0"].ports[1] = stubPort{
		state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand",
	}
	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "the incomplete poll must not consume the last-known healthy state")
	assert.True(t, events[0].IsFatal)
}

func TestIBState_PartialFirstPollPreservesFirstPollSeverityGating(t *testing.T) {
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}},
		})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{
			"mlx5_0": {"PIX"}, "mlx5_1": {"NODE"},
		})
	originalListDirs := reader.ListDirsFunc
	partial := true
	reader.ListDirsFunc = func(path string) ([]string, error) {
		if partial && strings.HasSuffix(path, "/mlx5_0/ports") {
			return nil, errors.New("transient initial read failure")
		}

		return originalListDirs(path)
	}

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Nil(t, check.previousDevices, "a partial initial poll must not become the baseline")

	partial = false
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events,
		"the first complete poll must still apply singleton/peer severity gating")
}

func TestIBState_DiscardedPollReemitsTransition(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	_, err := check.Run()
	require.NoError(t, err)

	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}
	events, err := check.Prepare()
	require.NoError(t, err)
	require.Len(t, events, 1)
	check.Discard()

	events, err = check.Prepare()
	require.NoError(t, err)
	require.Len(t, events, 1, "discarded publication must not consume the transition")
	assert.True(t, events[0].IsFatal)
	check.Commit()

	events, err = check.Prepare()
	require.NoError(t, err)
	assert.Empty(t, events)
	check.Commit()
}

func TestIBState_VFIsSkipped(t *testing.T) {
	node := newStubNode().addIB("mlx5_vf", &stubDevice{
		pciAddress: "0000:47:00.2",
		numaNode:   0,
		isVF:       true,
		ports: map[int]stubPort{
			1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_vf": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "VF devices should never produce events")
}

func TestIBState_ManagementNICIsExcluded(t *testing.T) {
	// NIC on NUMA 2 with all-SYS relationships should classify as
	// Management and thus never emit events.
	node := newStubNode().addIB("mlx5_mgmt", &stubDevice{
		pciAddress: "0000:02:00.0",
		numaNode:   2,
		ports: map[int]stubPort{
			1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_mgmt": {"SYS"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "Management NIC DOWN should not produce events")
}

func TestIBState_InclusionOverrideBypassesAllDeviceFilters(t *testing.T) {
	node := newStubNode().addIB("mlx5_forced", &stubDevice{
		vendor:     "0x9999",
		pciAddress: "0000:02:00.1",
		numaNode:   2,
		isVF:       true,
		ports: map[int]stubPort{
			1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
		},
	})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_forced": {"SYS"}},
	)
	assert.True(t, classifier.IsManagementNIC("mlx5_forced"))

	cfg := &config.Config{
		NicExclusionRegex:         "^mlx5_forced$",
		NicInclusionRegexOverride: "^mlx5_forced$",
	}
	check := NewInfiniBandStateCheck("node1", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	// Establish a healthy baseline, then verify that the explicitly included
	// device remains monitored when it crosses into an unhealthy state.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	node.ib["mlx5_forced"].ports[1] = stubPort{
		state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand",
	}

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Contains(t, events[0].Message, "mlx5_forced")
}

func TestIBState_DeviceDisappearanceEmitsFatal(t *testing.T) {
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	_, err := check.Run()
	require.NoError(t, err)

	// Remove mlx5_1 from the node and re-poll.
	removedDevice := node.ib["mlx5_1"]
	delete(node.ib, "mlx5_1")

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "first miss must retain last-known state")

	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "second miss must retain last-known state")

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Contains(t, events[0].Message, "mlx5_1")
	assert.Contains(t, events[0].Message, "disappeared")

	found := false
	for _, e := range events[0].EntitiesImpacted {
		if e.EntityType == checks.EntityTypeNIC && e.EntityValue == "mlx5_1" {
			found = true
		}
	}
	assert.True(t, found, "device-level event should include the NIC entity")

	// Re-enumeration as healthy must clear the device-level disappearance
	// with BOTH scopes: a port-level healthy (clears port conditions) and
	// a device-level healthy whose entity set mirrors the FATAL's, so
	// consumers requiring all entities to match can clear the NIC entry.
	node.ib["mlx5_1"] = removedDevice
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 2)

	var sawPortHealthy, sawDeviceHealthy bool

	for _, evt := range events {
		require.True(t, evt.IsHealthy)

		hasPortEntity := false

		for _, e := range evt.EntitiesImpacted {
			if e.EntityType == checks.EntityTypePort {
				hasPortEntity = true
			}
		}

		if hasPortEntity {
			sawPortHealthy = true
		} else {
			sawDeviceHealthy = true

			require.Len(t, evt.EntitiesImpacted, 1, "device recovery must carry only the NIC entity")
			assert.Equal(t, "mlx5_1", evt.EntitiesImpacted[0].EntityValue)
			assert.Contains(t, evt.Message, "re-enumerated")
		}
	}

	assert.True(t, sawPortHealthy, "port-scoped recovery must be emitted")
	assert.True(t, sawDeviceHealthy, "device-scoped recovery must be emitted")
}

func TestIBState_ExpectedDownCardSuppressedOnFirstPoll(t *testing.T) {
	// Two compute cards, both dual-port, one port cabled each. The mode
	// active count is 1, so the DOWN port should be suppressed on the
	// first poll (uncabled port, not a failure).
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{
				1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
				2: {state: "DOWN", physState: "Polling", linkLayer: "InfiniBand"},
			},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{
				1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"},
				2: {state: "DOWN", physState: "Polling", linkLayer: "InfiniBand"},
			},
		})

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}},
	)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "expected-down first-poll ports should be suppressed, not published")
}
