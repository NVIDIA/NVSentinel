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

package statefile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempEnv sets up a temp dir with state and boot-id paths and returns a
// Manager pointing at them.
func tempEnv(t *testing.T, bootID string) (*Manager, string, string) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bootIDPath := filepath.Join(dir, "boot_id")

	require.NoError(t, os.WriteFile(bootIDPath, []byte(bootID+"\n"), 0o644))

	return NewManagerWithPaths(statePath, bootIDPath), statePath, bootIDPath
}

func TestLoad_NoFile_StartsFreshAndMarksBootIDChanged(t *testing.T) {
	m, _, _ := tempEnv(t, "boot-1")

	require.NoError(t, m.Load())
	assert.True(t, m.BootIDChanged(), "a missing state file should be treated as a fresh boot")
	assert.Empty(t, m.KnownDevices())
	assert.Empty(t, m.PortStatesFor())
}

func TestLoad_CorruptFile_StartsFresh(t *testing.T) {
	m, statePath, _ := tempEnv(t, "boot-1")
	require.NoError(t, os.WriteFile(statePath, []byte("{ not valid json"), 0o644))

	require.NoError(t, m.Load())
	assert.True(t, m.BootIDChanged(), "a corrupt state file should be treated as a fresh boot")
	assert.Empty(t, m.KnownDevices())
}

func TestLoad_ScopeChangeDiscardsPortStateKeepsCounters(t *testing.T) {
	// Port/device state persisted under one discovery scope must not
	// seed a monitor running under another: a shrinking scope would
	// fabricate device-disappearance FATALs and a growing scope would
	// bypass first-poll severity gating for never-seen ports. Counter
	// snapshots and latched breaches, however, MUST survive: hardware
	// counters do not reset on a scope change, and a latched breach must
	// not produce a synthetic "healthy after reboot" recovery.
	m, statePath, bootIDPath := tempEnv(t, "boot-1")
	m.SetScope("incl=;excl=")
	require.NoError(t, m.Load())

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "ACTIVE", PhysicalState: "LinkUp", LinkLayer: "Ethernet"},
	}, []string{"mlx5_0"}, "Ethernet")
	m.UpdateAnomalousCards(map[string]AnomalousCardFlag{
		"0000:47:00": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateDisappearedDevices(map[string]DisappearedDeviceFlag{
		"mlx5_0": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateDeviceMissCounts(map[string]int{"mlx5_1": 2}, "Ethernet")
	m.UpdateCounterSnapshots(map[string]CounterSnapshot{
		"mlx5_0:1:link_downed": {Value: 7},
	})
	m.UpdateBreachFlags(map[string]CounterBreachFlag{
		"mlx5_0:1:link_downed": {Breached: true, CheckName: "EthernetStateCheck", IsFatal: true},
	})
	require.NoError(t, m.Save())

	// Same scope: everything survives the reload.
	same := NewManagerWithPaths(statePath, bootIDPath)
	same.SetScope("incl=;excl=")
	require.NoError(t, same.Load())
	assert.False(t, same.BootIDChanged(), "matching scope must keep persisted state")
	assert.False(t, same.ScopeChanged())
	assert.Contains(t, same.PortStatesFor(), "mlx5_0_1")
	assert.Contains(t, same.DisappearedDevicesFor("Ethernet"), "mlx5_0")
	assert.Equal(t, 2, same.DeviceMissCountsFor("Ethernet")["mlx5_1"])

	// Different scope (inclusion override enabled): port/device state is
	// discarded, counter state survives, and the two signals are
	// reported separately.
	changed := NewManagerWithPaths(statePath, bootIDPath)
	changed.SetScope("incl=^mlx5_1$;excl=")
	require.NoError(t, changed.Load())
	assert.True(t, changed.ScopeChanged(), "scope change must be reported")
	assert.False(t, changed.BootIDChanged(), "scope change must NOT masquerade as a reboot")
	assert.Empty(t, changed.PortStatesFor())
	assert.Empty(t, changed.KnownDevices())
	assert.Contains(t, changed.CounterSnapshots(), "mlx5_0:1:link_downed",
		"counter snapshots must survive a scope change")

	flags := changed.BreachFlags()
	require.Contains(t, flags, "mlx5_0:1:link_downed",
		"latched breaches must survive a scope change")
	assert.True(t, flags["mlx5_0:1:link_downed"].Breached)

	assert.Contains(t, changed.AnomalousCardsFor("Ethernet"), "0000:47:00",
		"the card-anomaly latch must survive a scope change — the new scope "+
			"may skip the card lifecycle entirely (inclusion override)")
	assert.Contains(t, changed.DisappearedDevicesFor("Ethernet"), "mlx5_0",
		"an outstanding disappearance FATAL must survive a scope change")
	assert.Empty(t, changed.DeviceMissCountsFor("Ethernet"),
		"partial debounce state must reset with the discovery scope")
}

func TestLoad_BootIDChangePreservesAnomalousCards(t *testing.T) {
	// A reboot resets port/device and counter state, but an outstanding
	// card FATAL downstream (e.g., a quarantine annotation) does not
	// disappear on reboot — the latch must survive so the card-healthy
	// recovery can still be emitted once positive evidence arrives.
	m, statePath, bootIDPath := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "DOWN", PhysicalState: "Disabled", LinkLayer: "Ethernet"},
	}, []string{"mlx5_0"}, "Ethernet")
	m.UpdateAnomalousCards(map[string]AnomalousCardFlag{
		"0000:47:00": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateDisappearedDevices(map[string]DisappearedDeviceFlag{
		"mlx5_0": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateDeviceMissCounts(map[string]int{"mlx5_1": 2}, "Ethernet")
	require.NoError(t, m.Save())

	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-2\n"), 0o644))

	rebooted := NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, rebooted.Load())
	assert.True(t, rebooted.BootIDChanged())
	assert.Empty(t, rebooted.PortStatesFor(), "port state must reset on reboot")
	assert.Contains(t, rebooted.AnomalousCardsFor("Ethernet"), "0000:47:00",
		"card-anomaly latch must survive a reboot")
	assert.Contains(t, rebooted.DisappearedDevicesFor("Ethernet"), "mlx5_0",
		"an outstanding disappearance FATAL must survive a reboot")
	assert.Empty(t, rebooted.DeviceMissCountsFor("Ethernet"),
		"partial debounce state must reset on reboot")
}

func TestDeviceLifecycleState_MixedLayersDoNotClobber(t *testing.T) {
	m, _, _ := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdateDisappearedDevices(map[string]DisappearedDeviceFlag{
		"mlx5_0": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateDisappearedDevices(map[string]DisappearedDeviceFlag{
		"mlx5_0": {LinkLayer: "InfiniBand"},
	}, "InfiniBand")
	m.UpdateDeviceMissCounts(map[string]int{"mlx5_1": 1}, "Ethernet")
	m.UpdateDeviceMissCounts(map[string]int{"mlx5_1": 2}, "InfiniBand")

	m.UpdateDisappearedDevices(map[string]DisappearedDeviceFlag{}, "Ethernet")
	m.UpdateDeviceMissCounts(map[string]int{}, "Ethernet")

	assert.Empty(t, m.DisappearedDevicesFor("Ethernet"))
	assert.Empty(t, m.DeviceMissCountsFor("Ethernet"))
	assert.Contains(t, m.DisappearedDevicesFor("InfiniBand"), "mlx5_0")
	assert.Equal(t, 2, m.DeviceMissCountsFor("InfiniBand")["mlx5_1"])
}

func TestAnomalousCards_MixedLayerSameCard(t *testing.T) {
	// VPI/mixed cards expose IB and Ethernet functions under the same
	// PCI bus:device, so both checks can latch the same card. The
	// entries must coexist and per-layer updates must not clobber the
	// sibling's entry.
	m, statePath, bootIDPath := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdateAnomalousCards(map[string]AnomalousCardFlag{
		"0000:47:00": {LinkLayer: "Ethernet"},
	}, "Ethernet")
	m.UpdateAnomalousCards(map[string]AnomalousCardFlag{
		"0000:47:00": {LinkLayer: "InfiniBand"},
	}, "InfiniBand")

	assert.Contains(t, m.AnomalousCardsFor("Ethernet"), "0000:47:00")
	assert.Contains(t, m.AnomalousCardsFor("InfiniBand"), "0000:47:00",
		"IB latch for the same card must not overwrite the Ethernet entry")

	// Ethernet recovers: its entry clears, the IB one stays.
	m.UpdateAnomalousCards(map[string]AnomalousCardFlag{}, "Ethernet")
	assert.Empty(t, m.AnomalousCardsFor("Ethernet"))
	assert.Contains(t, m.AnomalousCardsFor("InfiniBand"), "0000:47:00",
		"clearing one layer's latch must preserve the sibling layer's entry")

	// And the surviving entry round-trips through disk.
	require.NoError(t, m.Save())

	reloaded := NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, reloaded.Load())
	assert.Contains(t, reloaded.AnomalousCardsFor("InfiniBand"), "0000:47:00")
	assert.Empty(t, reloaded.AnomalousCardsFor("Ethernet"))
}

func TestLoad_LegacyFileWithoutScopeIsDiscarded(t *testing.T) {
	// Files written before the scope field existed have Scope == "". A
	// monitor running with a real fingerprint must not seed their
	// port/device state blindly — the safe direction is a one-time
	// port-state reset on upgrade (counters still survive).
	m, statePath, bootIDPath := tempEnv(t, "boot-1")
	require.NoError(t, m.Load()) // legacy writer: no SetScope

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "ACTIVE", PhysicalState: "LinkUp", LinkLayer: "Ethernet"},
	}, []string{"mlx5_0"}, "Ethernet")
	require.NoError(t, m.Save())

	upgraded := NewManagerWithPaths(statePath, bootIDPath)
	upgraded.SetScope("incl=;excl=")
	require.NoError(t, upgraded.Load())
	assert.True(t, upgraded.ScopeChanged(), "legacy scope-less file must reset under a fingerprinted monitor")
	assert.False(t, upgraded.BootIDChanged())
	assert.Empty(t, upgraded.PortStatesFor())
}

func TestLoad_UnreadableBootID_StillStartsFresh(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithPaths(filepath.Join(dir, "state.json"), filepath.Join(dir, "missing"))

	require.NoError(t, m.Load())
	assert.True(t, m.BootIDChanged())
}

func TestLoadSave_RoundTripPreservesFields(t *testing.T) {
	m, statePath, _ := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "ACTIVE", PhysicalState: "LinkUp", LinkLayer: "InfiniBand"},
	}, []string{"mlx5_0"}, "InfiniBand")

	require.NoError(t, m.Save())

	// Read the file back raw to make sure the JSON contains every field.
	data, err := os.ReadFile(statePath)
	require.NoError(t, err)

	var raw MonitorState
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, SchemaVersion, raw.Version)
	assert.Equal(t, "boot-1", raw.BootID)
	assert.Contains(t, raw.PortStates, "mlx5_0_1")
	assert.Contains(t, raw.KnownDevices, "mlx5_0")

	// A second Load on the same file with matching boot ID must not
	// report bootIDChanged.
	m2, _, _ := newManagerForExisting(t, statePath, "boot-1")
	require.NoError(t, m2.Load())
	assert.False(t, m2.BootIDChanged(), "pod restart with same boot ID should not reset")
	assert.Equal(t, []string{"mlx5_0"}, m2.KnownDevices())
	assert.Contains(t, m2.PortStatesFor("InfiniBand"), "mlx5_0_1")
}

func TestLoad_DifferentBootIDDiscardsState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bootIDPath := filepath.Join(dir, "boot_id")

	// Prime the file with state under boot-1.
	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-1\n"), 0o644))
	m1 := NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, m1.Load())
	m1.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "ACTIVE", PhysicalState: "LinkUp", LinkLayer: "InfiniBand"},
	}, []string{"mlx5_0"}, "InfiniBand")
	require.NoError(t, m1.Save())

	// Now "reboot": flip the boot ID and load again.
	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-2\n"), 0o644))
	m2 := NewManagerWithPaths(statePath, bootIDPath)
	require.NoError(t, m2.Load())
	assert.True(t, m2.BootIDChanged(), "changed boot ID should reset state")
	assert.Empty(t, m2.KnownDevices(), "state should be cleared after boot ID change")
	assert.Empty(t, m2.PortStatesFor())
}

func TestPortStatesFor_FiltersByLinkLayer(t *testing.T) {
	m, _, _ := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, LinkLayer: "InfiniBand"},
	}, []string{"mlx5_0"}, "InfiniBand")

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_1_1": {Device: "mlx5_1", Port: 1, LinkLayer: "Ethernet"},
	}, []string{"mlx5_1"}, "Ethernet")

	ib := m.PortStatesFor("InfiniBand")
	assert.Len(t, ib, 1)
	assert.Contains(t, ib, "mlx5_0_1")

	eth := m.PortStatesFor("Ethernet")
	assert.Len(t, eth, 1)
	assert.Contains(t, eth, "mlx5_1_1")

	all := m.PortStatesFor()
	assert.Len(t, all, 2)
}

func TestUpdatePortStates_SiblingLayerPreserved(t *testing.T) {
	// A write from the IB check should not drop the Ethernet check's
	// entries, and vice versa. This is what prevents the two checks
	// from clobbering each other's state when they write on different
	// poll ticks.
	m, _, _ := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, LinkLayer: "InfiniBand"},
	}, []string{"mlx5_0"}, "InfiniBand")

	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_2_1": {Device: "mlx5_2", Port: 1, LinkLayer: "Ethernet"},
	}, []string{"mlx5_2"}, "Ethernet")

	// IB rewrites its own entry; Ethernet entry must survive.
	m.UpdatePortStates(map[string]PortStateSnapshot{
		"mlx5_0_1": {Device: "mlx5_0", Port: 1, State: "DOWN", LinkLayer: "InfiniBand"},
	}, []string{"mlx5_0"}, "InfiniBand")

	got := m.PortStatesFor()
	require.Len(t, got, 2)
	assert.Equal(t, "DOWN", got["mlx5_0_1"].State)
	assert.Equal(t, "Ethernet", got["mlx5_2_1"].LinkLayer)

	assert.ElementsMatch(t, []string{"mlx5_0", "mlx5_2"}, m.KnownDevices())
}

func TestSave_WithoutLoadFails(t *testing.T) {
	m, _, _ := tempEnv(t, "boot-1")
	err := m.Save()
	require.Error(t, err, "Save must not silently succeed before Load establishes initial state")
}

func TestSave_IsAtomic_NoTmpLeftBehind(t *testing.T) {
	m, statePath, _ := tempEnv(t, "boot-1")
	require.NoError(t, m.Load())
	require.NoError(t, m.Save())

	_, err := os.Stat(statePath + ".tmp")
	assert.True(t, os.IsNotExist(err), "tmp file should be renamed away after a successful Save")
}

// newManagerForExisting constructs a fresh Manager pointing at an
// existing file (simulating a new pod picking up persisted state). It
// writes a new boot-id file with the requested value.
func newManagerForExisting(t *testing.T, statePath, bootID string) (*Manager, string, string) {
	t.Helper()

	dir := filepath.Dir(statePath)
	bootIDPath := filepath.Join(dir, "boot_id_v2")
	require.NoError(t, os.WriteFile(bootIDPath, []byte(bootID+"\n"), 0o644))

	return NewManagerWithPaths(statePath, bootIDPath), statePath, bootIDPath
}

func TestPendingBaselines_PersistAcrossReloadAndScopeChange(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bootIDPath := filepath.Join(dir, "boot_id")
	require.NoError(t, os.WriteFile(bootIDPath, []byte("boot-1\n"), 0o644))

	mgr := NewManagerWithPaths(statePath, bootIDPath)
	mgr.SetScope("incl=;excl=")
	require.NoError(t, mgr.Load())

	assert.False(t, mgr.PendingBaseline("InfiniBandStateCheck"))

	mgr.SetPendingBaseline("InfiniBandStateCheck")
	require.NoError(t, mgr.Save())

	// Same boot, same scope: the owed baseline survives a pod restart.
	mgr2 := NewManagerWithPaths(statePath, bootIDPath)
	mgr2.SetScope("incl=;excl=")
	require.NoError(t, mgr2.Load())
	assert.True(t, mgr2.PendingBaseline("InfiniBandStateCheck"),
		"pending baseline must survive a same-boot reload")

	// Same boot, different scope: a pending boot reconciliation must not
	// be forgotten because the operator changed the discovery scope.
	mgr3 := NewManagerWithPaths(statePath, bootIDPath)
	mgr3.SetScope("incl=^mlx5_1$;excl=")
	require.NoError(t, mgr3.Load())
	require.True(t, mgr3.ScopeChanged())
	assert.True(t, mgr3.PendingBaseline("InfiniBandStateCheck"),
		"pending baseline must survive a scope change")

	mgr3.ClearPendingBaseline("InfiniBandStateCheck")
	require.NoError(t, mgr3.Save())

	mgr4 := NewManagerWithPaths(statePath, bootIDPath)
	mgr4.SetScope("incl=^mlx5_1$;excl=")
	require.NoError(t, mgr4.Load())
	assert.False(t, mgr4.PendingBaseline("InfiniBandStateCheck"),
		"a cleared flag must stay cleared after reload")
}
