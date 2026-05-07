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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
)

// newStateManagerForTest writes a boot-id file and returns a freshly
// loaded Manager. Tests share it between check instances to simulate a
// pod restart picking up the previous pod's state.
func newStateManagerForTest(t *testing.T, bootID string) (*statefile.Manager, string, string) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bootIDPath := filepath.Join(dir, "boot_id")

	require.NoError(t, os.WriteFile(bootIDPath, []byte(bootID+"\n"), 0o644))

	mgr := statefile.NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, mgr.Load())

	return mgr, statePath, bootIDPath
}

func TestIBState_Persistence_RecoveryAcrossPodRestart(t *testing.T) {
	// Simulate a pod that sees mlx5_0 port 1 go DOWN, then crashes.
	// A new pod starts with the same boot ID, reads the persisted state,
	// finds the port has come back up, and emits a recovery event.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

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

	firstPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := firstPod.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "DOWN port on first poll should emit fatal event")
	assert.True(t, events[0].IsFatal)

	// The port snapshot should now be on disk.
	persisted := mgr.PortStatesFor("InfiniBand")
	require.Contains(t, persisted, "mlx5_0_1")
	assert.Equal(t, "DOWN", persisted["mlx5_0_1"].State)

	// Now the admin fixes the port while our pod is restarting.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}

	// A new pod re-reads the state file from disk (same boot ID) and
	// should seed previousPorts from it.
	mgr2 := reloadManager(t, mgr)
	secondPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)

	events, err = secondPod.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "second pod should emit exactly one recovery event")
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assert.Contains(t, events[0].Message, "healthy")
}

func TestIBState_Persistence_BootIDChangedEmitsHealthyBaseline(t *testing.T) {
	// When bootIDChanged=true the first poll emits healthy baseline
	// events for every currently-healthy port so the platform can clear
	// stale FATAL conditions carried from the previous boot.
	mgr, _, _ := newStateManagerForTest(t, "boot-2")

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
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
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "boot-ID-changed first poll should emit a baseline event per healthy port")
	assert.True(t, events[0].IsHealthy)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)

	// Second poll must be back to normal (no duplicate baseline).
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "second poll after a baseline should be silent unless something changes")
}

func TestIBState_Persistence_BootIDChangedStillEmitsFatalForUnhealthy(t *testing.T) {
	// On a boot-ID change, unhealthy ports still produce fatal events so
	// operators learn about hardware that didn't come back clean.
	mgr, _, _ := newStateManagerForTest(t, "boot-2")

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
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
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "DOWN port after reboot should still fire fatal")
}

func TestIBState_Persistence_DeviceDisappearanceAcrossRestart(t *testing.T) {
	// First pod sees mlx5_0 and mlx5_1 up. It crashes. mlx5_1 vanishes
	// while the pod is restarting. The new pod should emit a device
	// disappearance event because mlx5_1 is in the persisted
	// KnownDevices list but missing from sysfs.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

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

	firstPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	_, err := firstPod.Run()
	require.NoError(t, err)

	// Drop mlx5_1, simulate a fresh pod on the same boot.
	delete(node.ib, "mlx5_1")

	mgr2 := reloadManager(t, mgr)
	secondPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)
	events, err := secondPod.Run()
	require.NoError(t, err)

	var disappeared *pb.HealthEvent

	for _, e := range events {
		if e.IsFatal {
			for _, ent := range e.EntitiesImpacted {
				if ent.EntityValue == "mlx5_1" {
					disappeared = e
				}
			}
		}
	}

	require.NotNil(t, disappeared, "new pod should emit a fatal device-disappearance event for mlx5_1")
	assert.Contains(t, disappeared.Message, "mlx5_1")
	assert.Contains(t, disappeared.Message, "disappeared")
}

func TestEthState_Persistence_IBAndEthShareFileWithoutClobber(t *testing.T) {
	// Both state checks share the same MonitorState. A write from one
	// must not wipe the other's entries.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := newStubNode().
		addIB("mlx5_ib", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_eth", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_ib": {"PIX"}, "mlx5_eth": {"NODE"}},
	)

	ib := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	eth := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	_, err := ib.Run()
	require.NoError(t, err)
	_, err = eth.Run()
	require.NoError(t, err)

	// After both have polled, the file should contain entries for both
	// layers.
	all := mgr.PortStatesFor()
	assert.Len(t, all, 2, "shared state file should contain both IB and Ethernet entries")
	assert.Contains(t, all, "mlx5_ib_1")
	assert.Contains(t, all, "mlx5_eth_1")

	// A second IB poll should not drop the Ethernet entry.
	_, err = ib.Run()
	require.NoError(t, err)

	all = mgr.PortStatesFor()
	assert.Contains(t, all, "mlx5_eth_1", "Ethernet entry must survive IB rewrite")
}

// reloadManager creates a fresh statefile.Manager that reads from the
// same on-disk file as the original, simulating a pod restart that
// picks up persisted state via Load() rather than sharing in-memory state.
func reloadManager(t *testing.T, original *statefile.Manager) *statefile.Manager {
	t.Helper()

	path, bootIDPath := original.Paths()
	fresh := statefile.NewManagerWithPaths(path, bootIDPath)
	require.NoError(t, fresh.Load())

	return fresh
}
