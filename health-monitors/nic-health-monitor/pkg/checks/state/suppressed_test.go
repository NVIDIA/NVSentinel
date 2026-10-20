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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
)

const testReportAfter = 10 * time.Minute

// tiedIBNode has four single-port compute cards with mlx5_0 and mlx5_1 down,
// so the card-homogeneity vote is a 2-2 tie and the first poll has no peer
// evidence for either down port.
func tiedIBNode() *stubNode {
	node := newStubNode()
	for i, pci := range []string{"0000:03:00.0", "0002:03:00.0", "0010:03:00.0", "0012:03:00.0"} {
		port := holdIBUp
		if i < 2 {
			port = holdIBPolling
		}

		node.addIB(fmt.Sprintf("mlx5_%d", i), &stubDevice{
			pciAddress: pci, numaNode: 0,
			ports: map[int]stubPort{1: port},
		})
	}

	return node
}

func newSuppressedIBCheck(
	t *testing.T, node *stubNode, reportAfter time.Duration, mgr *statefile.Manager, start time.Time,
) (*InfiniBandStateCheck, *fakeClock) {
	t.Helper()

	topo := make(map[string][]string, len(node.ib))
	for name := range node.ib {
		topo[name] = []string{"PIX"}
	}

	reader := node.reader()
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, topo)
	cfg := &config.Config{StateCheck: config.StateCheckConfig{ReportSuppressedAfter: reportAfter}}
	check := NewInfiniBandStateCheck("suppressed-ib", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	clock := &fakeClock{now: start}
	check.now = clock.Now

	return check, clock
}

var suppressedStart = time.Date(2026, 10, 5, 15, 37, 0, 0, time.UTC)

// unhealthyPorts returns the devices with an unhealthy port event.
func unhealthyPorts(events []*pb.HealthEvent) []string {
	var devices []string

	for _, e := range events {
		if e.IsHealthy {
			continue
		}

		for _, ent := range e.EntitiesImpacted {
			if ent.EntityType == checks.EntityTypeNIC {
				devices = append(devices, ent.EntityValue)
			}
		}
	}

	return devices
}

func TestIBState_SuppressedPort_OffByDefault_NeverReported(t *testing.T) {
	check, clock := newSuppressedIBCheck(t, tiedIBNode(), 0, freshStateManager(t), suppressedStart)

	assert.Empty(t, runPoll(t, check), "a tied vote gives no peer evidence; the first poll must suppress")

	for range 4 {
		clock.Advance(6 * time.Hour)
		assert.Empty(t, runPoll(t, check), "with the option off, a suppressed port is never reported")
	}
}

func TestIBState_SuppressedPort_ReportedOnceAfterDelay(t *testing.T) {
	node := tiedIBNode()
	check, clock := newSuppressedIBCheck(t, node, testReportAfter, freshStateManager(t), suppressedStart)

	assert.Empty(t, runPoll(t, check))

	clock.Advance(testReportAfter - time.Second)
	assert.Empty(t, runPoll(t, check), "one second short of the delay must stay suppressed")

	clock.Advance(time.Second)
	events := runPoll(t, check)
	assert.ElementsMatch(t, []string{"mlx5_0", "mlx5_1"}, unhealthyPorts(events))

	for _, e := range events {
		assert.False(t, e.IsFatal, "DOWN/Polling keeps its usual non-fatal severity")
		assert.Equal(t, checks.InfiniBandStateCheckName, e.CheckName)
	}

	clock.Advance(time.Minute)
	assert.Empty(t, runPoll(t, check), "a reported port must be published once")

	node.ib["mlx5_0"].ports[1] = holdIBUp
	clock.Advance(time.Second)
	events = runPoll(t, check)
	require.Len(t, events, 2)
	assert.True(t, events[0].IsHealthy, "recovery of a reported port publishes its healthy event")
	// With the tie broken 3-1, the existing card check now has peer evidence
	// against mlx5_1's card.
	assert.True(t, events[1].IsFatal)
	assert.Contains(t, events[1].Message, "Card 0002:03:00 (compute) has 0 active ports, expected 1")
}

func TestIBState_SuppressedPort_ComesUpFirst_NeverReportedUnhealthy(t *testing.T) {
	node := tiedIBNode()
	check, clock := newSuppressedIBCheck(t, node, testReportAfter, freshStateManager(t), suppressedStart)

	assert.Empty(t, runPoll(t, check))

	// Ports still training at boot come up within seconds.
	node.ib["mlx5_0"].ports[1] = holdIBUp
	node.ib["mlx5_1"].ports[1] = holdIBUp
	clock.Advance(10 * time.Second)
	assert.Empty(t, unhealthyPorts(runPoll(t, check)))

	// A later outage is a runtime transition, not a first-poll suppression.
	clock.Advance(time.Hour)
	assert.Empty(t, runPoll(t, check))
	assert.Empty(t, check.suppressedSince, "a port read healthy must be forgotten")
}

func TestIBState_SuppressedPort_WaitSurvivesPodRestart(t *testing.T) {
	node := tiedIBNode()
	mgr := freshStateManager(t)
	check, _ := newSuppressedIBCheck(t, node, testReportAfter, mgr, suppressedStart)

	assert.Empty(t, runPoll(t, check))

	restarted, clock := newSuppressedIBCheck(t, node, testReportAfter, reloadManager(t, mgr),
		suppressedStart.Add(6*time.Minute))
	assert.Empty(t, runPoll(t, restarted), "the restarted pod must not report before the original delay")

	clock.Advance(4 * time.Minute)
	assert.ElementsMatch(t, []string{"mlx5_0", "mlx5_1"}, unhealthyPorts(runPoll(t, restarted)),
		"the wait must be measured from the original suppression, not the restart")
}

func TestIBState_SuppressedPort_DiscardedReportIsRetried(t *testing.T) {
	check, clock := newSuppressedIBCheck(t, tiedIBNode(), testReportAfter, freshStateManager(t), suppressedStart)

	assert.Empty(t, runPoll(t, check))

	clock.Advance(testReportAfter)
	events, err := check.Prepare()
	require.NoError(t, err)
	assert.Len(t, unhealthyPorts(events), 2)
	check.Discard()

	clock.Advance(time.Second)
	assert.Len(t, unhealthyPorts(runPoll(t, check)), 2, "a discarded report must be published on the next poll")
	assert.Empty(t, runPoll(t, check))
}

func TestEthState_SuppressedPort_ReportedFatalAfterDelay(t *testing.T) {
	node := newStubNode().addIB("mlx5_11", &stubDevice{
		pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
		ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
	})
	node.nets["eth1"] = "down"

	reader := node.reader()
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, map[string][]string{"mlx5_11": {"NODE"}})
	cfg := &config.Config{StateCheck: config.StateCheckConfig{ReportSuppressedAfter: testReportAfter}}
	check := NewEthernetStateCheck("suppressed-eth", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)
	clock := &fakeClock{now: suppressedStart}
	check.now = clock.Now

	assert.Empty(t, runPoll(t, check), "a singleton card has no peer evidence; the first poll must suppress")

	clock.Advance(testReportAfter)
	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "a reported RoCE DOWN port keeps its usual fatal severity")

	clock.Advance(time.Minute)
	assert.Empty(t, runPoll(t, check))
}
