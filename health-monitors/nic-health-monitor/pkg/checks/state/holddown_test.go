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
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
)

const testHoldDown = 10 * time.Second

var (
	holdIBUp      = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}
	holdIBDown    = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "InfiniBand"}
	holdIBPolling = stubPort{state: "DOWN", physState: "Polling", linkLayer: "InfiniBand"}
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func holdDownConfig(holdDown time.Duration) *config.Config {
	return &config.Config{StateCheck: config.StateCheckConfig{HoldDown: holdDown}}
}

func singlePortIBNode() *stubNode {
	return newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: holdIBUp},
	})
}

// threeCardIBNode has three single-port compute cards in one role group,
// so one card going down is below the group's mode of one active port.
func threeCardIBNode() *stubNode {
	node := newStubNode()
	for i, pci := range []string{"0000:47:00.0", "0000:48:00.0", "0000:49:00.0"} {
		node.addIB(fmt.Sprintf("mlx5_%d", i), &stubDevice{
			pciAddress: pci, numaNode: 0,
			ports: map[int]stubPort{1: holdIBUp},
		})
	}

	return node
}

func newHeldIBCheck(
	t *testing.T, nodeName string, node *stubNode, holdDown time.Duration, mgr *statefile.Manager,
) (*InfiniBandStateCheck, *fakeClock) {
	t.Helper()

	topo := make(map[string][]string, len(node.ib))
	for name := range node.ib {
		topo[name] = []string{"PIX"}
	}

	reader := node.reader()
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, topo)
	check := NewInfiniBandStateCheck(nodeName, reader, holdDownConfig(holdDown),
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	clock := &fakeClock{now: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}
	check.now = clock.Now

	return check, clock
}

type pollRunner interface {
	Run() ([]*pb.HealthEvent, error)
}

func runPoll(t *testing.T, check pollRunner) []*pb.HealthEvent {
	t.Helper()

	events, err := check.Run()
	require.NoError(t, err)

	return events
}

// blips reads nic_health_monitor_port_state_blips_total for one port.
func blips(t *testing.T, nodeName, checkName, device, port string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	want := map[string]string{"node": nodeName, "check": checkName, "device": device, "port": port}

	for _, family := range families {
		if family.GetName() != "nic_health_monitor_port_state_blips_total" {
			continue
		}

		for _, m := range family.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}

			if maps.Equal(got, want) {
				return m.GetCounter().GetValue()
			}
		}
	}

	return 0
}

// fatalEventKinds reports whether events hold a fatal port event for device
// and a fatal card homogeneity event.
func fatalEventKinds(events []*pb.HealthEvent, device string) (port, card bool) {
	for _, e := range events {
		if !e.IsFatal {
			continue
		}

		if strings.Contains(e.Message, "active ports, expected") {
			card = true
			continue
		}

		for _, ent := range e.EntitiesImpacted {
			if ent.EntityType == checks.EntityTypeNIC && ent.EntityValue == device {
				port = true
			}
		}
	}

	return port, card
}

func TestIBState_PortBlipWithinHoldDown_PublishesNothingAndCountsBlip(t *testing.T) {
	const nodeName = "hold-ib-blip"

	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, nodeName, node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown

	for range 3 {
		clock.Advance(3 * time.Second)
		assert.Empty(t, runPoll(t, check), "a port down for less than the hold-down must not be reported")
	}

	node.ib["mlx5_0"].ports[1] = holdIBUp
	clock.Advance(3 * time.Second)

	assert.Empty(t, runPoll(t, check), "recovering a port that was never reported must not publish a healthy event")
	assert.InDelta(t, 1, blips(t, nodeName, checks.InfiniBandStateCheckName, "mlx5_0", "1"), 0)

	// A later outage starts a fresh hold rather than resuming the old one.
	clock.Advance(time.Minute)
	assert.Empty(t, runPoll(t, check))
	assert.InDelta(t, 1, blips(t, nodeName, checks.InfiniBandStateCheckName, "mlx5_0", "1"), 0,
		"a port that stays healthy is not a blip")

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "a new outage must be held for the full hold-down")
}

func TestIBState_PortDownPastHoldDown_PublishesFatalOnce(t *testing.T) {
	const nodeName = "hold-ib-due"

	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, nodeName, node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown

	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.Advance(testHoldDown - time.Second)
	assert.Empty(t, runPoll(t, check), "one poll short of the hold-down must still be held")

	clock.Advance(time.Second)
	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.False(t, events[0].IsHealthy)

	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "the fatal event must be published once")

	node.ib["mlx5_0"].ports[1] = holdIBUp
	clock.Advance(time.Second)
	events = runPoll(t, check)
	require.Len(t, events, 1, "recovery must not be held")
	assert.True(t, events[0].IsHealthy)
	assert.InDelta(t, 0, blips(t, nodeName, checks.InfiniBandStateCheckName, "mlx5_0", "1"), 0)
}

func TestIBState_DiscardedPollsDuringHoldDown_NeitherRestartNorConsumeIt(t *testing.T) {
	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, "hold-ib-discard", node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown

	clock.Advance(time.Second)
	events, err := check.Prepare()
	require.NoError(t, err)
	assert.Empty(t, events)
	check.Discard()

	clock.Advance(testHoldDown)
	events, err = check.Prepare()
	require.NoError(t, err)
	require.Len(t, events, 1, "a discarded poll must not restart the hold-down")
	check.Discard()

	clock.Advance(time.Second)
	events, err = check.Prepare()
	require.NoError(t, err)
	require.Len(t, events, 1, "a discarded publication must not consume the due event")
	assert.True(t, events[0].IsFatal)
	check.Commit()

	events, err = check.Prepare()
	require.NoError(t, err)
	assert.Empty(t, events)
	check.Commit()
}

func TestIBState_PortDownBelowCardMode_HoldsCardEventWithPortEvent(t *testing.T) {
	// Control: without a hold-down the same transition reports both events.
	controlNode := threeCardIBNode()
	control, _ := newHeldIBCheck(t, "hold-ib-card-control", controlNode, 0, freshStateManager(t))
	assert.Empty(t, runPoll(t, control))

	controlNode.ib["mlx5_2"].ports[1] = holdIBDown
	portFatal, cardFatal := fatalEventKinds(runPoll(t, control), "mlx5_2")
	require.True(t, portFatal, "control: port event expected without a hold-down")
	require.True(t, cardFatal, "control: card event expected without a hold-down")

	node := threeCardIBNode()
	check, clock := newHeldIBCheck(t, "hold-ib-card", node, testHoldDown, freshStateManager(t))
	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_2"].ports[1] = holdIBDown

	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "neither the port nor its card may be reported during the hold-down")

	clock.Advance(testHoldDown)
	portFatal, cardFatal = fatalEventKinds(runPoll(t, check), "mlx5_2")
	assert.True(t, portFatal, "port event once the hold-down has elapsed")
	assert.True(t, cardFatal, "card event once the hold-down has elapsed")
}

func TestIBState_FirstSeenUnhealthyPort_IsNotHeld(t *testing.T) {
	node := threeCardIBNode()
	node.ib["mlx5_2"].ports[1] = holdIBDown
	check, _ := newHeldIBCheck(t, "hold-ib-first-seen", node, testHoldDown, freshStateManager(t))

	portFatal, cardFatal := fatalEventKinds(runPoll(t, check), "mlx5_2")
	assert.True(t, portFatal, "the hold-down only gates a healthy-to-unhealthy edge")
	assert.True(t, cardFatal)
}

func TestIBState_EscalationOfReportedUnhealthyPort_IsNotHeld(t *testing.T) {
	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, "hold-ib-escalation", node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBPolling

	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.Advance(testHoldDown)
	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.False(t, events[0].IsFatal, "DOWN/Polling is reported non-fatal")

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	events = runPoll(t, check)
	require.Len(t, events, 1, "escalating a port already reported unhealthy must not be held")
	assert.True(t, events[0].IsFatal)
}

func TestIBState_RestartDuringHoldDown_RestartsHoldAndStillReports(t *testing.T) {
	mgr, _, _ := newStateManagerForTest(t, "boot-1")

	node := singlePortIBNode()
	firstPod, clock := newHeldIBCheck(t, "hold-ib-restart", node, testHoldDown, mgr)

	assert.Empty(t, runPoll(t, firstPod))

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, firstPod))

	// The held port is persisted as last reported, so the next pod sees
	// a fresh healthy-to-unhealthy edge rather than a port it never reported.
	assert.Equal(t, "ACTIVE", mgr.PortStatesFor("InfiniBand")["mlx5_0_1"].State)

	secondPod, secondClock := newHeldIBCheck(t, "hold-ib-restart", node, testHoldDown, reloadManager(t, mgr))
	secondClock.now = clock.now.Add(testHoldDown)

	assert.Empty(t, runPoll(t, secondPod), "a restart restarts the hold-down")

	secondClock.Advance(testHoldDown)
	events := runPoll(t, secondPod)
	require.Len(t, events, 1, "the outage must still be reported after the restarted hold-down")
	assert.True(t, events[0].IsFatal)
}

func TestEthState_PortBlipWithinHoldDown_PublishesNothingThenReportsSustainedOutage(t *testing.T) {
	const nodeName = "hold-eth"

	node := newStubNode().addIB("mlx5_11", &stubDevice{
		pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	node.nets["eth1"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_11": {"NODE"}},
	)

	check := NewEthernetStateCheck(nodeName, reader, holdDownConfig(testHoldDown),
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)
	clock := &fakeClock{now: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}
	check.now = clock.Now

	assert.Empty(t, runPoll(t, check))

	setDown := func(down bool) {
		if down {
			node.ib["mlx5_11"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
			node.nets["eth1"] = "down"

			return
		}

		node.ib["mlx5_11"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}
		node.nets["eth1"] = "up"
	}

	setDown(true)
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "a RoCE port down for less than the hold-down must not be reported")

	setDown(false)
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))
	assert.InDelta(t, 1, blips(t, nodeName, checks.EthernetStateCheckName, "mlx5_11", "1"), 0)

	setDown(true)
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.Advance(testHoldDown)
	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
}

func TestIBState_UnreadableDeviceDuringHoldDown_KeepsHold(t *testing.T) {
	const nodeName = "hold-ib-unreadable"

	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, nodeName, node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	// A failed read retains the last committed snapshot, which is not a recovery.
	node.ib["mlx5_0"].attrReadsFail = true
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))
	assert.InDelta(t, 0, blips(t, nodeName, checks.InfiniBandStateCheckName, "mlx5_0", "1"), 0)

	node.ib["mlx5_0"].attrReadsFail = false
	clock.Advance(testHoldDown - time.Second)
	events := runPoll(t, check)
	require.Len(t, events, 1, "an unreadable poll must not restart the hold-down")
	assert.True(t, events[0].IsFatal)
}

func TestIBState_DeviceMissedDuringHoldDown_KeepsHold(t *testing.T) {
	node := singlePortIBNode().addIB("mlx5_1", &stubDevice{
		pciAddress: "0000:48:00.0", numaNode: 0,
		ports: map[int]stubPort{1: holdIBUp},
	})
	check, clock := newHeldIBCheck(t, "hold-ib-missed", node, testHoldDown, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_1"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	// One missed enumeration is below the disappearance threshold.
	missing := node.ib["mlx5_1"]
	delete(node.ib, "mlx5_1")
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_1"] = missing
	clock.Advance(testHoldDown - time.Second)

	portFatal, _ := fatalEventKinds(runPoll(t, check), "mlx5_1")
	assert.True(t, portFatal, "a missed enumeration must not restart the hold-down")
}
