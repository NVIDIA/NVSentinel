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

package state

import (
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

// efaNode is a two-adapter p4d-style fixture on a real temp filesystem:
// two EFA devices on the GPU NUMA node, both with an ENA netdev sibling,
// driven through the production sysfs reader and a metadata-backed
// classifier that lists them as PIX-attached compute NICs.
type efaNode struct {
	tree       *sysfstest.Tree
	reader     sysfs.Reader
	classifier *topology.Classifier
}

func newEFANode(t *testing.T, withNetdev bool) *efaNode {
	t.Helper()

	tree := sysfstest.New(t)

	for i, name := range []string{"rdmap0s6", "rdmap0s7"} {
		opts := sysfstest.EFAOpts{
			Driver:     "efa",
			PCIAddress: []string{"0000:00:06.0", "0000:00:07.0"}[i],
		}
		if withNetdev {
			opts.NetDev = []string{"ens6", "ens7"}[i]
			opts.Operstate = "up"
			opts.Carrier = "1"
		}

		tree.AddEFA(t, name, opts)
	}

	reader := sysfs.NewReader(tree.IBBase, tree.NetBase)
	classifier := buildClassifier(t, reader,
		[]string{"0000:10:1c.0"},
		map[string][]string{"rdmap0s6": {"PIX"}, "rdmap0s7": {"PIX"}},
		writeProcNetRoute(t, "eth0"),
	)

	return &efaNode{tree: tree, reader: reader, classifier: classifier}
}

func (n *efaNode) newCheck(t *testing.T, bootIDChanged bool) *EFAStateCheck {
	t.Helper()

	return NewEFAStateCheck("node1", n.reader, &config.Config{}, n.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), bootIDChanged)
}

func runPoll(t *testing.T, check checks.Check) []*pb.HealthEvent {
	t.Helper()

	events, err := check.Run()
	require.NoError(t, err)

	return events
}

func TestEFAState_HealthyFirstPollIsSilent(t *testing.T) {
	node := newEFANode(t, true)
	check := node.newCheck(t, false)

	assert.Equal(t, checks.EFAStateCheckName, check.Name())
	assert.Empty(t, runPoll(t, check), "first healthy observation must not emit")
	assert.Empty(t, runPoll(t, check), "steady healthy state must not emit")

	require.Equal(t, topology.RoleCompute, node.classifier.RoleOf("rdmap0s6"))
}

func TestEFAState_NetdevOperstateDownIsFatalThenRecovers(t *testing.T) {
	node := newEFANode(t, true)
	check := node.newCheck(t, false)
	require.Empty(t, runPoll(t, check))

	// The efa driver keeps reporting ACTIVE/LinkUp in sysfs; only the
	// netdev shows the link loss.
	node.tree.SetNetDev(t, "ens6", "down", "0")

	events := runPoll(t, check)
	require.Len(t, events, 1)

	evt := events[0]
	assert.Equal(t, checks.EFAStateCheckName, evt.CheckName)
	assert.True(t, evt.IsFatal)
	assert.False(t, evt.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, evt.RecommendedAction)
	assert.Equal(t, "EFA port rdmap0s6 port 1: state DOWN, phys_state LinkDown, operstate down (ens6)",
		evt.Message)
	require.Len(t, evt.EntitiesImpacted, 2)
	assert.Equal(t, checks.EntityTypeNIC, evt.EntitiesImpacted[0].EntityType)
	assert.Equal(t, "rdmap0s6", evt.EntitiesImpacted[0].EntityValue)
	assert.Equal(t, checks.EntityTypePort, evt.EntitiesImpacted[1].EntityType)
	assert.Equal(t, "1", evt.EntitiesImpacted[1].EntityValue)

	assert.Empty(t, runPoll(t, check), "a port that stays DOWN must not re-emit")

	node.tree.SetNetDev(t, "ens6", "up", "1")

	events = runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, "EFA port rdmap0s6 port 1: healthy (ACTIVE, LinkUp)", events[0].Message)
}

func TestEFAState_LostCarrierWithUnknownOperstateIsFatal(t *testing.T) {
	node := newEFANode(t, true)
	check := node.newCheck(t, false)
	require.Empty(t, runPoll(t, check))

	node.tree.SetNetDev(t, "ens7", "unknown", "0")

	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Contains(t, events[0].Message, "EFA port rdmap0s7 port 1: state DOWN, phys_state LinkDown")
	assert.Equal(t, "rdmap0s7", events[0].EntitiesImpacted[0].EntityValue)
}

func TestEFAState_UnknownOperstateWithCarrierIsHealthy(t *testing.T) {
	node := newEFANode(t, true)
	node.tree.SetNetDev(t, "ens6", "unknown", "1")
	node.tree.SetNetDev(t, "ens7", "unknown", "")

	check := node.newCheck(t, false)
	assert.Empty(t, runPoll(t, check))
	assert.Empty(t, runPoll(t, check))
}

func TestEFAState_NoNetdevStaysHealthyAndDetectsDisappearance(t *testing.T) {
	// EFA-only adapters carry no netdev: the check falls back to the
	// driver's (constant) port state and to sysfs presence.
	node := newEFANode(t, false)
	check := node.newCheck(t, false)
	require.Empty(t, runPoll(t, check))

	node.tree.RemoveDevice(t, "rdmap0s7")

	var fatal *pb.HealthEvent

	for i := 0; i < deviceMissThreshold+1 && fatal == nil; i++ {
		for _, evt := range runPoll(t, check) {
			if evt.IsFatal {
				fatal = evt
			}
		}
	}

	require.NotNil(t, fatal, "device removal must surface after the miss debounce")
	assert.Equal(t, "EFA device rdmap0s7 disappeared from sysfs", fatal.Message)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, fatal.RecommendedAction)
	require.Len(t, fatal.EntitiesImpacted, 1)
	assert.Equal(t, "rdmap0s7", fatal.EntitiesImpacted[0].EntityValue)
}

func TestEFAState_BaselineRunEmitsClearAndHealthyPorts(t *testing.T) {
	node := newEFANode(t, true)
	check := node.newCheck(t, true)

	events := runPoll(t, check)
	require.GreaterOrEqual(t, len(events), 3)

	clearEvt := events[0]
	assert.True(t, clearEvt.IsHealthy)
	assert.Empty(t, clearEvt.EntitiesImpacted, "check-scoped clear carries no entities")
	assert.Equal(t, checks.EFAStateCheckName, clearEvt.CheckName)

	healthy := 0

	for _, evt := range events[1:] {
		if evt.IsHealthy && len(evt.EntitiesImpacted) == 2 {
			healthy++
		}
	}

	assert.Equal(t, 2, healthy, "one healthy baseline per EFA port")
	assert.Empty(t, runPoll(t, check), "baseline is emitted once")
}

func TestEFAState_RoCEAndIBChecksIgnoreEFADevices(t *testing.T) {
	node := newEFANode(t, true)
	node.tree.SetNetDev(t, "ens6", "down", "0")

	eth := NewEthernetStateCheck("node1", node.reader, &config.Config{}, node.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)
	ib := NewInfiniBandStateCheck("node1", node.reader, &config.Config{}, node.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	for range 3 {
		assert.Empty(t, runPoll(t, eth), "RoCE check must not own EFA ports")
		assert.Empty(t, runPoll(t, ib), "IB check must not own EFA ports")
	}
}

func TestEFAState_PersistsUnderEFALinkLayer(t *testing.T) {
	node := newEFANode(t, true)
	mgr := freshStateManager(t)

	check := NewEFAStateCheck("node1", node.reader, &config.Config{}, node.classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	require.Empty(t, runPoll(t, check))

	assert.Len(t, mgr.PortStatesFor(efaLinkLayer), 2)
	assert.Empty(t, mgr.PortStatesFor(ethLinkLayer), "EFA entries must not leak into the RoCE namespace")
}

func TestNetOperStateIsDown(t *testing.T) {
	for _, s := range []string{"up", "unknown", "UP", "", " up\n"} {
		assert.False(t, netOperStateIsDown(s), s)
	}

	for _, s := range []string{"down", "lowerlayerdown", "notpresent", "dormant", "testing"} {
		assert.True(t, netOperStateIsDown(s), s)
	}
}
