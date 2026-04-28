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
}

type stubPort struct {
	state      string
	physState  string
	linkLayer  string
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

func (n *stubNode) wirePortReads(m *sysfs.MockReader) {
	m.ReadIBPortStateFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		return fmt.Sprintf("4: %s", d.ports[port].state), nil
	}

	m.ReadIBPortPhysStateFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
		}

		return fmt.Sprintf("5: %s", d.ports[port].physState), nil
	}

	m.ReadIBPortLinkLayerFunc = func(device string, port int) (string, error) {
		d := n.ib[device]
		if d == nil {
			return "", nil
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
func buildClassifier(t *testing.T, reader sysfs.Reader, gpuPCIs []string, topo map[string][]string) *topology.Classifier {
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

	c, err := topology.LoadFromMetadata(path, reader)
	require.NoError(t, err)

	return c
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

func TestIBState_FirstPollDownEmitsFatal(t *testing.T) {
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
	require.Len(t, events, 1)

	evt := events[0]
	assert.True(t, evt.IsFatal)
	assert.False(t, evt.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Contains(t, evt.Message, "DOWN")
	assert.Contains(t, evt.Message, "Disabled")
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
	delete(node.ib, "mlx5_1")

	events, err := check.Run()
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
	require.NotEmpty(t, events, "expected-down suppression should downgrade, not drop, first-poll events")

	for _, e := range events {
		assert.False(t, e.IsFatal, "expected-down card should not fire fatal on first poll")
		assert.Equal(t, pb.RecommendedAction_NONE, e.RecommendedAction,
			"suppressed event should clear RecommendedAction")
	}
}
