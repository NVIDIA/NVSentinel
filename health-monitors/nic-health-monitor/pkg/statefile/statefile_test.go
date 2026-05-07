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
