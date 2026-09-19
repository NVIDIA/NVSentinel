// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package counter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs/sysfstest"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// efaCounterTOML enables one counter of each class the EFA check reads:
// a per-port hw_counter, a device-level hw_counter (fatal), a netdev
// statistic (shared with every layer) and an mlx5-only counter that
// must be ignored on EFA ports.
const efaCounterTOML = `
[counterDetection]
enabled = true

[[counterDetection.counters]]
name = "efa_rx_drops"
enabled = true
thresholdType = "delta"
threshold = 0

[[counterDetection.counters]]
name = "efa_no_completion_cmds"
enabled = true
thresholdType = "delta"
threshold = 0

[[counterDetection.counters]]
name = "efa_cmds_err"
enabled = true
thresholdType = "delta"
threshold = 5

[[counterDetection.counters]]
name = "rx_errors"
enabled = true
thresholdType = "delta"
threshold = 0

[[counterDetection.counters]]
name = "link_downed"
enabled = true
thresholdType = "delta"
threshold = 0
`

func loadCounterTOML(t *testing.T, toml string) *config.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)

	return cfg
}

type efaCounterNode struct {
	tree       *sysfstest.Tree
	reader     sysfs.Reader
	classifier *topology.Classifier
	cfg        *config.Config
}

func newEFACounterNode(t *testing.T) *efaCounterNode {
	t.Helper()

	tree := sysfstest.New(t)
	tree.AddEFA(t, "rdmap0s6", sysfstest.EFAOpts{
		Driver: "efa", PCIAddress: "0000:00:06.0",
		NetDev: "ens6", Operstate: "up", Carrier: "1",
	})
	sysfstest.WriteFile(t, filepath.Join(tree.NetBase, "ens6", "statistics", "rx_errors"), "0\n")

	reader := sysfs.NewReader(tree.IBBase, tree.NetBase)
	classifier := counterClassifier(t, reader, map[string][]string{"rdmap0s6": {"PIX"}})

	return &efaCounterNode{
		tree:       tree,
		reader:     reader,
		classifier: classifier,
		cfg:        loadCounterTOML(t, efaCounterTOML),
	}
}

func (n *efaCounterNode) newCheck(t *testing.T) *EFADegradationCheck {
	t.Helper()

	return NewEFADegradationCheck(testNode, n.reader, n.cfg, n.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, counterStateManager(t), false)
}

func codesOf(events []*pb.HealthEvent) []string {
	var codes []string

	for _, e := range events {
		codes = append(codes, e.ErrorCode...)
	}

	return codes
}

func TestEFADegradation_PortHWCounterBreachIsNonFatal(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	assert.Equal(t, checks.EFADegradationCheckName, check.Name())
	assert.Equal(t, checks.CounterCheck, checks.CategoryOf(check.Name()))

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "first poll seeds snapshots only")

	node.tree.SetPortHWCounter(t, "rdmap0s6", "rx_drops", 7)

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.Equal(t, []string{"efa_rx_drops"}, evt.ErrorCode)
	assert.False(t, evt.IsFatal)
	assert.False(t, evt.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_NONE, evt.RecommendedAction)
	assert.Equal(t, checks.EFADegradationCheckName, evt.CheckName)
	assert.Contains(t, evt.Message, "EFA RX packets dropped")
	require.Len(t, evt.EntitiesImpacted, 2)
	assert.Equal(t, "rdmap0s6", evt.EntitiesImpacted[0].EntityValue)
	assert.Equal(t, "1", evt.EntitiesImpacted[1].EntityValue)

	// Latched: further growth while breached stays silent.
	node.tree.SetPortHWCounter(t, "rdmap0s6", "rx_drops", 9)

	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestEFADegradation_DeviceLevelAdminTimeoutIsFatal(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	_, err := check.Run()
	require.NoError(t, err)

	// no_completion_cmds lives in the device-level hw_counters
	// directory, not under the port.
	node.tree.SetDeviceHWCounter(t, "rdmap0s6", "no_completion_cmds", 1)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	assert.Equal(t, []string{"efa_no_completion_cmds"}, evt.ErrorCode)
	assert.True(t, evt.IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Contains(t, evt.Message, "device firmware unresponsive")
	assert.Equal(t, "rdmap0s6", evt.EntitiesImpacted[0].EntityValue)
}

func TestEFADegradation_DeltaThresholdRespected(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	_, err := check.Run()
	require.NoError(t, err)

	// efa_cmds_err has threshold 5: a delta of 5 is not a breach, 6 is.
	node.tree.SetDeviceHWCounter(t, "rdmap0s6", "cmds_err", 5)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "delta equal to the threshold must not breach")

	node.tree.SetDeviceHWCounter(t, "rdmap0s6", "cmds_err", 11)

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, []string{"efa_cmds_err"}, events[0].ErrorCode)
	assert.False(t, events[0].IsFatal)
}

func TestEFADegradation_NetdevStatisticsApplyToEFA(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	_, err := check.Run()
	require.NoError(t, err)

	sysfstest.WriteFile(t, filepath.Join(node.tree.NetBase, "ens6", "statistics", "rx_errors"), "3\n")

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, []string{"rx_errors"}, events[0].ErrorCode)
	assert.Equal(t, "rdmap0s6", events[0].EntitiesImpacted[0].EntityValue)
}

func TestEFADegradation_Mlx5CountersSkippedOnEFAPorts(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	_, err := check.Run()
	require.NoError(t, err)

	// link_downed is enabled in the shared list but scoped to mlx5 link
	// layers; the EFA check must never read (or key) it, so no snapshot
	// is created for it even though the sysfs file is absent.
	for key := range check.evaluator.Snapshots() {
		assert.NotContains(t, key, "link_downed")
	}

	assert.Contains(t, check.evaluator.Snapshots(), "rdmap0s6:1:efa_rx_drops")
	assert.Contains(t, check.evaluator.Snapshots(), "rdmap0s6:1:efa_no_completion_cmds")
}

func TestEFADegradation_RecoveryOnCounterReset(t *testing.T) {
	node := newEFACounterNode(t)
	check := node.newCheck(t)

	_, err := check.Run()
	require.NoError(t, err)

	node.tree.SetPortHWCounter(t, "rdmap0s6", "rx_drops", 4)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.False(t, events[0].IsHealthy)

	// Driver reload resets the counter: the latched breach recovers.
	node.tree.SetPortHWCounter(t, "rdmap0s6", "rx_drops", 0)

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy)
	assert.Equal(t, []string{"efa_rx_drops"}, codesOf(events))
}

func TestEFADegradation_EthernetCheckIgnoresEFADevices(t *testing.T) {
	node := newEFACounterNode(t)

	eth := NewEthernetDegradationCheck(testNode, node.reader, node.cfg, node.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, counterStateManager(t), false)

	_, err := eth.Run()
	require.NoError(t, err)

	node.tree.SetPortHWCounter(t, "rdmap0s6", "rx_drops", 50)

	events, err := eth.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "RoCE degradation check must not evaluate EFA ports")
	assert.Empty(t, eth.evaluator.Snapshots())
}
