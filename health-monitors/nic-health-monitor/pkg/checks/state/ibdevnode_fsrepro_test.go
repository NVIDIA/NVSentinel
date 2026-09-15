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
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// writeFile creates path (and parents) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// buildRealNodeSysfs materialises, on a real temp filesystem, the exact
// InfiniBand character-device layout observed on a live Crusoe H100-IB
// node (cluster slurmwise, node np-a62cfc8a-1, 2026-07-28): one Ethernet
// management NIC (mlx5_0, no issm) and one InfiniBand compute NIC
// (mlx5_1, issm present). It returns the ib/net base paths for
// sysfs.NewReader — the production reader, not the mock. This is the
// filesystem mirror of ticket #11021's environment.
func buildRealNodeSysfs(t *testing.T) (ibBase, netBase string) {
	t.Helper()

	root := t.TempDir()
	ibBase = filepath.Join(root, "infiniband")
	madBase := filepath.Join(root, "infiniband_mad")
	verbsBase := filepath.Join(root, "infiniband_verbs")
	netBase = filepath.Join(root, "net")

	// mlx5_0 — Ethernet management NIC (has umad/uverbs, no issm).
	writeFile(t, filepath.Join(ibBase, "mlx5_0", "device", "vendor"), "0x15b3")
	writeFile(t, filepath.Join(ibBase, "mlx5_0", "ports", "1", "state"), "4: ACTIVE")
	writeFile(t, filepath.Join(ibBase, "mlx5_0", "ports", "1", "phys_state"), "5: LinkUp")
	writeFile(t, filepath.Join(ibBase, "mlx5_0", "ports", "1", "link_layer"), "Ethernet")

	// mlx5_1 — InfiniBand compute NIC (issm expected).
	writeFile(t, filepath.Join(ibBase, "mlx5_1", "device", "vendor"), "0x15b3")
	writeFile(t, filepath.Join(ibBase, "mlx5_1", "ports", "1", "state"), "4: ACTIVE")
	writeFile(t, filepath.Join(ibBase, "mlx5_1", "ports", "1", "phys_state"), "5: LinkUp")
	writeFile(t, filepath.Join(ibBase, "mlx5_1", "ports", "1", "link_layer"), "InfiniBand")

	// infiniband_mad: umad0(mlx5_0/1), umad1(mlx5_1/1), issm1(mlx5_1/1) + abi_version.
	writeFile(t, filepath.Join(madBase, "abi_version"), "5")
	writeFile(t, filepath.Join(madBase, "umad0", "ibdev"), "mlx5_0")
	writeFile(t, filepath.Join(madBase, "umad0", "port"), "1")
	writeFile(t, filepath.Join(madBase, "umad1", "ibdev"), "mlx5_1")
	writeFile(t, filepath.Join(madBase, "umad1", "port"), "1")
	writeFile(t, filepath.Join(madBase, "issm1", "ibdev"), "mlx5_1")
	writeFile(t, filepath.Join(madBase, "issm1", "port"), "1")

	// infiniband_verbs: uverbs0(mlx5_0), uverbs1(mlx5_1) + abi_version.
	writeFile(t, filepath.Join(verbsBase, "abi_version"), "1")
	writeFile(t, filepath.Join(verbsBase, "uverbs0", "ibdev"), "mlx5_0")
	writeFile(t, filepath.Join(verbsBase, "uverbs1", "ibdev"), "mlx5_1")

	require.NoError(t, os.MkdirAll(netBase, 0o755))

	return ibBase, netBase
}

// TestIBCharDev_RealNodeLayout_FilesystemRepro drives the check through the
// production sysfs.Reader against a real on-disk mirror of a live H100-IB
// node: healthy first (mlx5_0 Ethernet correctly needs no issm, mlx5_1 IB
// has issm), then reproduces ticket #11021 by removing mlx5_1's issm node.
func TestIBCharDev_RealNodeLayout_FilesystemRepro(t *testing.T) {
	t.Parallel()

	ibBase, netBase := buildRealNodeSysfs(t)
	reader := sysfs.NewReader(ibBase, netBase)

	// Inclusion override keeps the classifier out of it (no gpu_metadata
	// needed): scope is decided by our own IB-port gate, so mlx5_0
	// (Ethernet) is still correctly out of scope.
	// issm detection is opt-in (default never); this repro targets the issm
	// mechanics, so enable it explicitly.
	cfg := &config.Config{
		NicInclusionRegexOverride: "mlx5_.*",
		CharDeviceCheck:           config.CharDeviceCheckConfig{Issm: config.IssmModeAlways},
	}
	classifier := topology.NewOverrideClassifier(reader)

	check := NewInfiniBandCharDeviceCheck("np-a62cfc8a-1", reader, cfg, classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	// Healthy node: every expected char device present → no events. This
	// also proves the Ethernet mlx5_0 does not trigger a false issm miss.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "healthy H100-IB node must emit no events")

	// Reproduce #11021: mlx5_1's issm node is gone (device still present,
	// port still ACTIVE/LinkUp) — the exact failure the passive port check
	// misses. The fatal fires once the miss has been confirmed for
	// charDevMissThreshold consecutive polls.
	require.NoError(t, os.RemoveAll(filepath.Join(ibBase+"_mad", "issm1")))

	runQuietPolls(t, check, charDevMissThreshold-1)

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "missing issm on an IB device must produce one fatal event")

	evt := events[0]
	assert.True(t, evt.IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Equal(t, []string{"issm"}, evt.ErrorCode)
	assert.Contains(t, evt.Message, "issm")
	assert.Contains(t, evt.Message, "mlx5_1")
	assertPortEntities(t, evt, "mlx5_1", 1)

	// Restore the node and confirm a positively-observed recovery.
	writeFile(t, filepath.Join(ibBase+"_mad", "issm1", "ibdev"), "mlx5_1")
	writeFile(t, filepath.Join(ibBase+"_mad", "issm1", "port"), "1")

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy, "restored issm must emit a recovery")
}
