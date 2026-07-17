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
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
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
	// Singleton card: no peer evidence the port should be up, so the
	// first-poll unhealthy state is logged locally and suppressed.
	assert.Empty(t, events, "DOWN port on first poll without peer evidence should not emit an event")

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

func TestIBState_Persistence_BootIDChangedSuppressesUnhealthyWithoutPeerEvidence(t *testing.T) {
	// On a boot-ID change, singleton unhealthy ports still update
	// persisted state, but without peer evidence they do not emit an
	// external event. A below-mode card in a >=2 group would stay fatal
	// (see TestIBState_FirstPollDownBelowModeStaysFatal).
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
	assert.Empty(t, events, "singleton card has no peer evidence; post-reboot DOWN should be suppressed")
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
	removedDevice := node.ib["mlx5_1"]
	delete(node.ib, "mlx5_1")

	mgr2 := reloadManager(t, mgr)
	secondPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)
	events, err := secondPod.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "first miss after restart must be debounced")

	events, err = secondPod.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "second miss after restart must be debounced")

	events, err = secondPod.Run()
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

	// The disappearance latch must survive another pod restart so a healthy
	// re-enumeration clears the outstanding device-level condition — with
	// both the port-scoped and the device-scoped (entity-symmetric) healthy.
	node.ib["mlx5_1"] = removedDevice
	mgr3 := reloadManager(t, mgr2)
	thirdPod := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr3, false)

	events, err = thirdPod.Run()
	require.NoError(t, err)
	require.Len(t, events, 2)

	deviceScoped := 0

	for _, evt := range events {
		require.True(t, evt.IsHealthy)
		assert.Contains(t, evt.Message, "mlx5_1")

		if len(evt.EntitiesImpacted) == 1 {
			deviceScoped++
		}
	}

	assert.Equal(t, 1, deviceScoped, "exactly one recovery must be device-scoped (NIC entity only)")
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

func TestEthState_InclusionScopeChange_NoDisappearanceStorm(t *testing.T) {
	// Regression for the scope-shrink transition: enabling the inclusion
	// override on a node with full-scope persisted state fabricated a
	// fatal "disappeared from sysfs" REPLACE_VM for every unpinned NIC.
	// The scope fingerprint in the state file must force a fresh-boot
	// reset instead of seeding the stale device set.
	fullScope := "incl=;excl="
	pinnedScope := "incl=^mlx5_1$;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(fullScope)
	require.NoError(t, mgr.Load())

	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}},
	)

	fullPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	_, err := fullPod.Run()
	require.NoError(t, err)
	require.Len(t, mgr.KnownDevices(), 2, "precondition: both devices persisted under the full scope")

	// Pod restarts with the override enabled: the scope mismatch must
	// discard the seeded port/device state (fresh-boot semantics for the
	// state checks, reported via ScopeChanged rather than BootIDChanged
	// so counters keep their latches)...
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(pinnedScope)
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.ScopeChanged(), "scope change must discard persisted port/device state")
	require.False(t, mgr2.BootIDChanged(), "scope change must not masquerade as a reboot")

	pinnedCfg := &config.Config{NicInclusionRegexOverride: "^mlx5_1$"}
	pinnedPod := NewEthernetStateCheck("node1", reader, pinnedCfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2,
		mgr2.BootIDChanged() || mgr2.ScopeChanged())

	events, err := pinnedPod.Run()
	require.NoError(t, err)

	// ...so the unpinned mlx5_0 must NOT be reported as disappeared.
	for _, e := range events {
		assert.False(t, e.IsFatal,
			"scope shrink must not fabricate fatal events, got: %s", e.Message)
		assert.NotContains(t, e.Message, "mlx5_0",
			"unpinned device must not appear in any event, got: %s", e.Message)
	}
}

func TestEthState_PinnedFirstPollDown_EmitsFatal(t *testing.T) {
	// The inclusion override is explicit operator intent: a pinned
	// device that is DOWN at first sight must be reported, not gated on
	// peer evidence (a pinned singleton has none by construction).
	// Without the override, the identical topology is suppressed — see
	// TestEthState_FirstPollDownSingletonStorageIsSuppressed.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
	})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	cfg := &config.Config{NicInclusionRegexOverride: "^mlx5_0$"}
	check := NewEthernetStateCheck("node1", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "pinned first-poll DOWN must emit an event, not be suppressed")
	assert.True(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
	assert.Contains(t, events[0].Message, "mlx5_0")
}

func TestIBState_PinnedFirstPollDown_EmitsFatal(t *testing.T) {
	// IB mirror of TestEthState_PinnedFirstPollDown_EmitsFatal: the
	// override bypass is wired per-file, so both link layers need the
	// regression pinned.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}},
	})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	cfg := &config.Config{NicInclusionRegexOverride: "^mlx5_0$"}
	check := NewInfiniBandStateCheck("node1", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "pinned first-poll DOWN must emit an event, not be suppressed")
	assert.True(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
}

func TestEthState_PinnedMultiCard_NoCardHomogeneityEvent(t *testing.T) {
	// Three same-role cards, one DOWN: under normal discovery this is a
	// decisive below-mode anomaly (mode=1, the DOWN card has 0 active)
	// and produces BOTH a port FATAL and a card-level homogeneity FATAL.
	// While the inclusion override is active the homogeneity check is
	// disabled, so only the port event may appear.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_2", &stubDevice{
			pciAddress: "0000:49:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}, "mlx5_2": {"PIX"}},
	)

	cfg := &config.Config{NicInclusionRegexOverride: "^mlx5_0$,^mlx5_1$,^mlx5_2$"}
	check := NewEthernetStateCheck("node1", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)

	var fatals []*pb.HealthEvent

	for _, e := range events {
		assert.NotContains(t, e.Message, "active ports",
			"card-homogeneity events must not fire while the override is active, got: %s", e.Message)

		if e.IsFatal {
			fatals = append(fatals, e)
		}
	}

	require.Len(t, fatals, 1, "exactly one fatal (the pinned DOWN port), no card event")
	assert.Contains(t, fatals[0].Message, "mlx5_0")
}

func TestEthState_PinnedVFRemoved_NoFalseDisappearance(t *testing.T) {
	// Bug B regression: a VF is only visible to discovery while pinned.
	// Removing the override must not report the VF as vanished hardware —
	// the scope-change reset discards the seeded device set first.
	pinnedScope := "incl=^mlx5_19$;excl="
	normalScope := "incl=;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(pinnedScope)
	require.NoError(t, mgr.Load())

	node := newStubNode().addIB("mlx5_19", &stubDevice{
		pciAddress: "0000:47:02.1", numaNode: 0, isVF: true,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_19": {"PIX"}},
	)

	pinnedCfg := &config.Config{NicInclusionRegexOverride: "^mlx5_19$"}
	pinnedPod := NewEthernetStateCheck("node1", reader, pinnedCfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	_, err := pinnedPod.Run()
	require.NoError(t, err)
	require.Contains(t, mgr.KnownDevices(), "mlx5_19", "precondition: pinned VF persisted")

	// Override removed: the VF is filtered out of normal discovery, but
	// the scope reset means it is no longer a "known" device either.
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(normalScope)
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.ScopeChanged())

	normalPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2,
		mgr2.BootIDChanged() || mgr2.ScopeChanged())

	events, err := normalPod.Run()
	require.NoError(t, err)

	for _, e := range events {
		assert.False(t, e.IsFatal, "unpinning a VF must not fabricate fatals, got: %s", e.Message)
		assert.NotContains(t, e.Message, "mlx5_19",
			"the unpinned VF must not appear in any event, got: %s", e.Message)
	}
}

func TestEthState_OverrideRemoved_DesignedDownSingletonSuppressed(t *testing.T) {
	// Scope-expansion regression: removing the override used to seed the
	// narrow pinned state, making firstPoll false, so the designed-DOWN
	// Aux frontend port (mlx5_11) bypassed the peer-evidence gate and
	// emitted a false FATAL REPLACE_VM. The scope reset must restore
	// true first-poll semantics so the singleton is suppressed again.
	pinnedScope := "incl=^mlx5_2$;excl="
	normalScope := "incl=;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(pinnedScope)
	require.NoError(t, mgr.Load())

	node := newStubNode().
		addIB("mlx5_2", &stubDevice{ // Prime frontend, default-route NIC
			pciAddress: "0000:9a:00.0", numaNode: 0, netDev: "eth0",
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_11", &stubDevice{ // lone Aux frontend, Disabled by design
			pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
		})
	node.nets["eth0"] = "up"
	node.nets["eth1"] = "down"

	reader := node.reader()
	routePath := writeProcNetRoute(t, "eth0")
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_2": {"NODE"}, "mlx5_11": {"NODE"}},
		routePath,
	)
	require.Equal(t, topology.RoleStorage, classifier.RoleOf("mlx5_11"),
		"precondition: mlx5_11 must be monitored (storage), not excluded")

	pinnedCfg := &config.Config{NicInclusionRegexOverride: "^mlx5_2$"}
	pinnedPod := NewEthernetStateCheck("node1", reader, pinnedCfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	_, err := pinnedPod.Run()
	require.NoError(t, err)

	// Override removed: scope reset → genuine first poll → the
	// peer-evidence gate applies to the never-seen DOWN singleton.
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(normalScope)
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.ScopeChanged())

	normalPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2,
		mgr2.BootIDChanged() || mgr2.ScopeChanged())

	events, err := normalPod.Run()
	require.NoError(t, err)

	for _, e := range events {
		assert.False(t, e.IsFatal,
			"designed-DOWN singleton must be suppressed after override removal, got: %s", e.Message)
		assert.NotContains(t, e.Message, "mlx5_11",
			"mlx5_11 must not appear in any event, got: %s", e.Message)
	}
}

// threeCardNode builds three same-role single-port Ethernet cards with
// mlx5_0's port in the given state — the minimal decisive homogeneity
// group (mode=1) where taking mlx5_0 down is a positive below-mode
// anomaly.
func threeCardNode(mlx50State, mlx50Phys string) *stubNode {
	return newStubNode().
		addIB("mlx5_0", &stubDevice{
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: mlx50State, physState: mlx50Phys, linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_1", &stubDevice{
			pciAddress: "0000:48:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_2", &stubDevice{
			pciAddress: "0000:49:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		})
}

func threeCardClassifier(t *testing.T, reader sysfs.Reader) *topology.Classifier {
	t.Helper()

	return buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_1": {"PIX"}, "mlx5_2": {"PIX"}},
	)
}

// cardEvents filters events down to those carrying the given card
// entity (the card-homogeneity FATAL/healthy pair).
func cardEvents(events []*pb.HealthEvent, card string) []*pb.HealthEvent {
	var out []*pb.HealthEvent

	for _, e := range events {
		for _, ent := range e.EntitiesImpacted {
			if ent.EntityValue == card {
				out = append(out, e)
			}
		}
	}

	return out
}

func TestEthState_CardAnomaly_LatchLifecycle(t *testing.T) {
	// The card-homogeneity event now has a full lifecycle: FATAL once
	// when the card drops below its role group's decisive mode, silence
	// while latched, and a card-healthy recovery when it returns to
	// mode — previously the FATAL had no recovery counterpart and any
	// quarantine held by the card entity was wedged forever.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := threeCardNode("ACTIVE", "LinkUp")
	reader := node.reader()
	classifier := threeCardClassifier(t, reader)

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	// Poll 1: everything healthy, nothing to say.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Poll 2: mlx5_0 drops. Expect the port FATAL and the card FATAL.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
	events, err = check.Run()
	require.NoError(t, err)

	cardEvts := cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1, "card anomaly onset must emit exactly one card event")
	assert.True(t, cardEvts[0].IsFatal)
	assert.Contains(t, cardEvts[0].Message, "active ports")

	// Poll 3: unchanged. Latch keeps the card silent; port is
	// edge-triggered and silent too.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "latched anomaly must not repeat")

	// Poll 4: mlx5_0 recovers. Expect the port recovery AND the card
	// healthy event that clears the card entity downstream.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}
	events, err = check.Run()
	require.NoError(t, err)

	cardEvts = cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1, "card recovery must emit exactly one card-healthy event")
	assert.True(t, cardEvts[0].IsHealthy)
	assert.False(t, cardEvts[0].IsFatal)
	assert.Contains(t, cardEvts[0].Message, "healthy")

	// Poll 5: steady healthy — silence again.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestEthState_CardRecoveryAcrossRestart(t *testing.T) {
	// The July 7 hardware wedge: the card FATAL fires, the pod restarts,
	// the port recovers — the card-healthy event must still be emitted,
	// which requires the anomaly latch to survive in the state file.
	scope := "incl=;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(scope)
	require.NoError(t, mgr.Load())

	node := threeCardNode("DOWN", "Disabled")
	reader := node.reader()
	classifier := threeCardClassifier(t, reader)

	firstPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := firstPod.Run()
	require.NoError(t, err)
	require.Len(t, cardEvents(events, "0000:47:00"), 1, "precondition: card FATAL latched")

	// Port fixed while the pod restarts (same scope, same boot).
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}

	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(scope)
	require.NoError(t, mgr2.Load())
	require.False(t, mgr2.ScopeChanged())
	require.False(t, mgr2.BootIDChanged())

	secondPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)

	events, err = secondPod.Run()
	require.NoError(t, err)

	cardEvts := cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1,
		"restart must not orphan the card entity: the seeded latch emits the card-healthy recovery")
	assert.True(t, cardEvts[0].IsHealthy)
}

func TestIBState_CardAnomaly_LatchLifecycle(t *testing.T) {
	// IB mirror of TestEthState_CardAnomaly_LatchLifecycle: the card
	// lifecycle is wired per-file, so both link layers pin the
	// onset → silence → recovery contract.
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

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
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		})
	reader := node.reader()
	classifier := threeCardClassifier(t, reader)

	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}
	events, err = check.Run()
	require.NoError(t, err)

	cardEvts := cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1, "IB card anomaly onset must emit exactly one card event")
	assert.True(t, cardEvts[0].IsFatal)

	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "latched IB anomaly must not repeat")

	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}
	events, err = check.Run()
	require.NoError(t, err)

	cardEvts = cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1, "IB card recovery must emit exactly one card-healthy event")
	assert.True(t, cardEvts[0].IsHealthy)
}

func TestEthState_OverrideRoundTrip_CardRecoveryNotOrphaned(t *testing.T) {
	// A card FATAL is outstanding when the operator enables the
	// inclusion override. Override mode skips the card lifecycle
	// entirely, so the latch must survive the scope change (and the
	// override pod's own persistence) for the recovery to be emitted
	// once the override is removed and the card is healthy again.
	normalScope := "incl=;excl="
	pinnedScope := "incl=^mlx5_1$;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(normalScope)
	require.NoError(t, mgr.Load())

	node := threeCardNode("DOWN", "Disabled")
	reader := node.reader()
	classifier := threeCardClassifier(t, reader)

	// Phase 1 (normal scope): the card FATAL fires and latches.
	normalPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	events, err := normalPod.Run()
	require.NoError(t, err)
	require.Len(t, cardEvents(events, "0000:47:00"), 1, "precondition: card FATAL latched")

	// Phase 2 (override enabled): scope reset, card lifecycle skipped.
	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(pinnedScope)
	require.NoError(t, mgr2.Load())
	require.True(t, mgr2.ScopeChanged())
	require.Contains(t, mgr2.AnomalousCardsFor(ethLinkLayer), "0000:47:00",
		"latch must survive the scope change into override mode")

	pinnedCfg := &config.Config{NicInclusionRegexOverride: "^mlx5_1$"}
	pinnedPod := NewEthernetStateCheck("node1", reader, pinnedCfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2,
		mgr2.BootIDChanged() || mgr2.ScopeChanged())

	events, err = pinnedPod.Run()
	require.NoError(t, err)
	assert.Empty(t, cardEvents(events, "0000:47:00"),
		"override mode must not emit card events")
	require.Contains(t, mgr2.AnomalousCardsFor(ethLinkLayer), "0000:47:00",
		"the override pod's persistence must carry the latch, not clobber it")

	// The card heals while the override is still active.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}

	// Phase 3 (override removed): the seeded latch emits the recovery.
	mgr3 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr3.SetScope(normalScope)
	require.NoError(t, mgr3.Load())
	require.True(t, mgr3.ScopeChanged())

	finalPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr3,
		mgr3.BootIDChanged() || mgr3.ScopeChanged())

	events, err = finalPod.Run()
	require.NoError(t, err)

	cardEvts := cardEvents(events, "0000:47:00")
	require.Len(t, cardEvts, 1,
		"the card entity must recover after the override round trip")
	assert.True(t, cardEvts[0].IsHealthy)
}

func TestEthState_BaselineRun_EmitsCardHealthyBaselines(t *testing.T) {
	// On baseline runs (reboot or scope change) healthy evaluated cards
	// emit card-healthy baselines alongside the port baselines, so card
	// entities recorded by a previous boot/scope clear even though the
	// latch was wiped with the rest of the state.
	mgr, _, _ := newStateManagerForTest(t, "boot-2")

	node := threeCardNode("ACTIVE", "LinkUp")
	reader := node.reader()
	classifier := threeCardClassifier(t, reader)

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	events, err := check.Run()
	require.NoError(t, err)

	require.Len(t, events, 6, "3 port baselines + 3 card-healthy baselines")

	for _, card := range []string{"0000:47:00", "0000:48:00", "0000:49:00"} {
		evts := cardEvents(events, card)
		require.Len(t, evts, 1, "card %s must get a healthy baseline", card)
		assert.True(t, evts[0].IsHealthy)
	}
}

func TestEthState_SameScopeRestart_RetainsMemory(t *testing.T) {
	// A pod restart with an unchanged scope fingerprint must keep the
	// seeded memory: a port that was DOWN before the restart and is
	// ACTIVE after it produces its recovery event.
	scope := "incl=;excl="

	mgr, statePath, bootIDPath := newStateManagerForTest(t, "boot-1")
	mgr.SetScope(scope)
	require.NoError(t, mgr.Load())

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
	})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}},
	)

	firstPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	_, err := firstPod.Run()
	require.NoError(t, err)

	// Port fixed while the pod restarts.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}

	mgr2 := statefile.NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope(scope)
	require.NoError(t, mgr2.Load())
	require.False(t, mgr2.ScopeChanged(), "unchanged scope must retain state")
	require.False(t, mgr2.BootIDChanged())

	secondPod := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)

	events, err := secondPod.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "retained memory must produce the DOWN→ACTIVE recovery event")
	assert.True(t, events[0].IsHealthy)
}

// TestPersistState_RetriesAfterFailedSave verifies the dirty-flag retry:
// a failed Save leaves the in-memory manager updated, so the change
// detection alone would never re-save on an unchanged poll and the
// on-disk state would stay stale until an unrelated change or restart.
func TestPersistState_RetriesAfterFailedSave(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the state directory belongs makes Save's
	// MkdirAll fail deterministically until it is removed.
	blocker := filepath.Join(dir, "state-dir")
	require.NoError(t, os.WriteFile(blocker, []byte("blocker"), 0o644))

	statePath := filepath.Join(blocker, "state.json")
	bootIDPath := filepath.Join(dir, "boot_id")
	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-1\n"), 0o644))

	mgr := statefile.NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, mgr.Load())

	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	})
	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewInfiniBandStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	// First poll: state changed, Save fails (the blocker file makes the
	// state directory uncreatable).
	_, err := check.Run()
	require.NoError(t, err, "persistence failures must not fail the poll")

	// Unblock before asserting: with the blocker gone, the stat result is
	// an unambiguous ENOENT for the state file itself rather than an
	// ENOTDIR artifact of the blocker on the parent path.
	require.NoError(t, os.Remove(blocker))

	_, statErr := os.Stat(statePath)
	require.True(t, os.IsNotExist(statErr), "the failed save must not have created the state file")

	// A steady-state poll has no changes, so only the dirty flag can
	// trigger the retry.
	_, err = check.Run()
	require.NoError(t, err)

	_, statErr = os.Stat(statePath)
	assert.NoError(t, statErr, "an unchanged poll after a failed save must retry persistence")
}
