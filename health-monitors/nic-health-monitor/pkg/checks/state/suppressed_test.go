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
	"os"
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
	return tiedIBNodeWith(holdIBPolling)
}

// tiedIBNodeWith is tiedIBNode with mlx5_0 and mlx5_1 in the given state.
func tiedIBNodeWith(down stubPort) *stubNode {
	node := newStubNode()
	for i, pci := range []string{"0000:03:00.0", "0002:03:00.0", "0010:03:00.0", "0012:03:00.0"} {
		port := holdIBUp
		if i < 2 {
			port = down
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
		assert.True(t, e.IsFatal, "a port still DOWN after the delay is fatal, Polling included")
		assert.Equal(t, pb.RecommendedAction_REPLACE_VM, e.RecommendedAction)
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

func TestIBState_SuppressedPort_SeverityAfterDelay(t *testing.T) {
	cases := []struct {
		name   string
		port   stubPort
		fatal  bool
		action pb.RecommendedAction
	}{
		{"DOWN/Polling", holdIBPolling, true, pb.RecommendedAction_REPLACE_VM},
		{"DOWN/Disabled", stubPort{state: checks.IBStateDown, physState: checks.IBPhysDisabled, linkLayer: ibLinkLayer},
			true, pb.RecommendedAction_REPLACE_VM},
		// INIT and ARMED wait for the Subnet Manager; an SM outage must not
		// make every booting node fatal.
		{"INIT", stubPort{state: "INIT", physState: checks.IBPhysLinkUp, linkLayer: ibLinkLayer},
			false, pb.RecommendedAction_NONE},
		{"ARMED", stubPort{state: "ARMED", physState: checks.IBPhysLinkUp, linkLayer: ibLinkLayer},
			false, pb.RecommendedAction_NONE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check, clock := newSuppressedIBCheck(t, tiedIBNodeWith(tc.port), testReportAfter,
				freshStateManager(t), suppressedStart)
			require.Empty(t, runPoll(t, check), "a tied vote gives no peer evidence; the first poll must suppress")

			clock.Advance(testReportAfter)
			events := runPoll(t, check)
			require.ElementsMatch(t, []string{"mlx5_0", "mlx5_1"}, unhealthyPorts(events))

			for _, e := range events {
				assert.Equal(t, tc.fatal, e.IsFatal)
				assert.Equal(t, tc.action, e.RecommendedAction)
			}
		})
	}
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

func TestEthState_SuppressedPort_OnlyComputeCardsReported(t *testing.T) {
	// mlx5_0 is a lone compute card. mlx5_11 is a lone storage card, like the
	// unused frontend aux port on OCI BM.GPU.H100.8 (ADR-047), which is down
	// by design. InfiniBand NICs on a GPU socket always classify as compute.
	node := singletonDownNode(ethLinkLayer).addIB("mlx5_11", &stubDevice{
		pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
		ports: map[int]stubPort{1: downPort(ethLinkLayer)},
	})
	node.nets["eth1"] = "down"
	topo := map[string][]string{"mlx5_0": {"PIX"}, "mlx5_11": {"NODE"}}
	check, clock := newLayerCheckWithTopology(t, ethLinkLayer, node, topo, testReportAfter,
		freshStateManager(t), suppressedStart, false)

	assert.Empty(t, runPoll(t, check), "singleton cards have no peer evidence; the first poll must suppress")
	assert.Contains(t, suppressionTimers(t, check), "mlx5_0_1")
	assert.NotContains(t, suppressionTimers(t, check), "mlx5_11_1", "a storage card must get no timer")

	clock.Advance(testReportAfter)
	events := runPoll(t, check)
	require.Equal(t, []string{"mlx5_0"}, unhealthyPorts(events), "only the compute card is reported")
	assert.True(t, events[0].IsFatal, "a RoCE port still DOWN after the delay is fatal")
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)

	clock.Advance(24 * time.Hour)
	assert.Empty(t, runPoll(t, check))
}

// TestState_SuppressedPort_UnreadableDevice_HoldsCardsPeersMayVouchFor covers
// polls with an unreadable device, where the homogeneity vote does not run.
func TestState_SuppressedPort_UnreadableDevice_HoldsCardsPeersMayVouchFor(t *testing.T) {
	for _, layer := range bothLayers {
		for _, bootIDChanged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bootIDChanged=%t", layer, bootIDChanged), func(t *testing.T) {
				// Both cards have one active port out of two, so a complete
				// poll vouches for them (#1379). mlx5_9 is unreadable.
				node := dualPortNode(layer, []string{"0000:03:00.0", "0002:03:00.0"}, 0).
					addIB("mlx5_9", &stubDevice{
						pciAddress: "0000:50:00.0", numaNode: 0,
						ports:           map[int]stubPort{1: upPort(layer)},
						portsUnreadable: true,
					})
				check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
					suppressedStart, bootIDChanged)

				for range checks.FirstPollDeferralLimit {
					require.Empty(t, runPoll(t, check))
				}

				require.Empty(t, unhealthyPorts(runPoll(t, check)), "the partial first poll must suppress")
				require.Len(t, suppressionTimers(t, check), 2, "no vote ran, so the uncabled ports get timers")

				clock.Advance(24 * time.Hour)
				assert.Empty(t, unhealthyPorts(runPoll(t, check)),
					"a card with an active port must wait for a complete poll")

				node.ib["mlx5_9"].portsUnreadable = false
				clock.Advance(time.Second)
				assert.Empty(t, unhealthyPorts(runPoll(t, check)))
				assert.Empty(t, suppressionTimers(t, check), "the complete poll vouches for the cards")
			})
		}
	}
}

func TestState_SuppressedPort_UnreadableSiblingFunction_Holds(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			// mlx5_9 is the other function of mlx5_0's card, so the card's
			// active count is unknown.
			node := singletonDownNode(layer).addIB("mlx5_9", &stubDevice{
				pciAddress: "0000:03:00.1", numaNode: 0,
				ports:           map[int]stubPort{1: upPort(layer)},
				portsUnreadable: true,
			})
			check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
				suppressedStart, false)

			for range checks.FirstPollDeferralLimit {
				require.Empty(t, runPoll(t, check))
			}

			require.Empty(t, unhealthyPorts(runPoll(t, check)))
			require.Contains(t, suppressionTimers(t, check), "mlx5_0_1")

			clock.Advance(24 * time.Hour)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)),
				"a card with an unreadable function must wait for a complete poll")
			assert.Contains(t, suppressionTimers(t, check), "mlx5_0_1", "the wait must not be dropped")
		})
	}
}

// downPort is the port newSuppressedLayerCheck starts with: DOWN/Polling for
// IB, DOWN/Disabled for RoCE.
func downPort(layer string) stubPort {
	physical := checks.IBPhysPolling
	if layer == ethLinkLayer {
		physical = checks.IBPhysDisabled
	}

	return stubPort{state: checks.IBStateDown, physState: physical, linkLayer: layer}
}

func upPort(layer string) stubPort {
	return stubPort{state: checks.IBStateActive, physState: checks.IBPhysLinkUp, linkLayer: layer}
}

// singletonDownNode has one down port on a singleton card, which the first
// poll suppresses for lack of peer evidence.
func singletonDownNode(layer string) *stubNode {
	return newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:03:00.0", numaNode: 0,
		ports: map[int]stubPort{1: downPort(layer)},
	})
}

func newSuppressedLayerCheck(
	t *testing.T, layer string, reportAfter time.Duration, mgr *statefile.Manager, start time.Time,
) (checks.TransactionalCheck, *stubNode, *fakeClock) {
	t.Helper()

	node := singletonDownNode(layer)
	check, clock := newLayerCheckOn(t, layer, node, reportAfter, mgr, start, false)

	return check, node, clock
}

// newLayerCheckOn builds the layer's state check on node, with every device
// on its own compute card.
func newLayerCheckOn(
	t *testing.T, layer string, node *stubNode, reportAfter time.Duration, mgr *statefile.Manager,
	start time.Time, bootIDChanged bool,
) (checks.TransactionalCheck, *fakeClock) {
	t.Helper()

	topo := make(map[string][]string, len(node.ib))
	for name := range node.ib {
		topo[name] = []string{"PIX"}
	}

	return newLayerCheckWithTopology(t, layer, node, topo, reportAfter, mgr, start, bootIDChanged)
}

// newLayerCheckWithTopology is newLayerCheckOn with the given NIC topology.
func newLayerCheckWithTopology(
	t *testing.T, layer string, node *stubNode, topo map[string][]string, reportAfter time.Duration,
	mgr *statefile.Manager, start time.Time, bootIDChanged bool,
) (checks.TransactionalCheck, *fakeClock) {
	t.Helper()

	reader := node.reader()
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, topo)
	cfg := &config.Config{StateCheck: config.StateCheckConfig{ReportSuppressedAfter: reportAfter}}
	clock := &fakeClock{now: start}

	if layer == ibLinkLayer {
		check := NewInfiniBandStateCheck("suppressed-restart", reader, cfg,
			classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, bootIDChanged)
		check.now = clock.Now

		return check, clock
	}

	check := NewEthernetStateCheck("suppressed-restart", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, bootIDChanged)
	check.now = clock.Now

	return check, clock
}

// suppressionTimers returns a check's staged-or-committed suppression times.
func suppressionTimers(t *testing.T, check checks.TransactionalCheck) map[string]time.Time {
	t.Helper()

	switch c := check.(type) {
	case *InfiniBandStateCheck:
		return c.suppressedSince
	case *EthernetStateCheck:
		return c.suppressedSince
	}

	t.Fatalf("unexpected check type %T", check)

	return nil
}

// rebootManager simulates a host reboot: it writes a new boot ID next to the
// original manager's state file and loads a fresh manager from it.
func rebootManager(t *testing.T, original *statefile.Manager, bootID string) *statefile.Manager {
	t.Helper()

	path, bootIDPath := original.Paths()
	require.NoError(t, os.WriteFile(bootIDPath, []byte(bootID+"\n"), 0o644))

	fresh := statefile.NewManagerWithPaths(path, bootIDPath)
	require.NoError(t, fresh.Load())
	require.True(t, fresh.BootIDChanged())

	return fresh
}

// healthyFor returns the healthy events that name device.
func healthyFor(events []*pb.HealthEvent, device string) []*pb.HealthEvent {
	var out []*pb.HealthEvent

	for _, e := range events {
		if !e.IsHealthy {
			continue
		}

		for _, ent := range e.EntitiesImpacted {
			if ent.EntityType == checks.EntityTypeNIC && ent.EntityValue == device {
				out = append(out, e)

				break
			}
		}
	}

	return out
}

var bothLayers = []string{ibLinkLayer, ethLinkLayer}

func TestState_SuppressedPort_FirstPollAfterReboot_ReportedAfterDelay(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			node := singletonDownNode(layer)
			check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
				suppressedStart, true)

			assert.Empty(t, unhealthyPorts(runPoll(t, check)),
				"the baseline poll after a reboot must suppress a port with no peer evidence")
			assert.Contains(t, suppressionTimers(t, check), "mlx5_0_1")

			clock.Advance(testReportAfter - time.Second)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)))

			clock.Advance(time.Second)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, check)))

			clock.Advance(time.Minute)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)), "a reported port must be published once")
		})
	}
}

func TestState_SuppressedPort_Reboot_RestartsWait(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			mgr := freshStateManager(t)
			node := singletonDownNode(layer)
			check, _ := newLayerCheckOn(t, layer, node, testReportAfter, mgr, suppressedStart, false)
			require.Empty(t, unhealthyPorts(runPoll(t, check)))

			rebooted := rebootManager(t, mgr, "after-reboot")
			assert.Empty(t, rebooted.PortStatesFor(layer), "a reboot must drop the persisted suppression time")

			restarted, clock := newLayerCheckOn(t, layer, node, testReportAfter, rebooted,
				suppressedStart.Add(6*time.Minute), true)
			require.Empty(t, unhealthyPorts(runPoll(t, restarted)))

			clock.Advance(4 * time.Minute)
			assert.Empty(t, unhealthyPorts(runPoll(t, restarted)),
				"after a reboot the wait must restart from the new first poll")

			clock.Advance(6 * time.Minute)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, restarted)))
		})
	}
}

func TestState_SuppressedPort_PendingBaselineRestart_KeepsWait(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			// mlx5_9 stays unreadable for the whole boot, so the reboot
			// baseline stays owed and every pod restart is a pending-baseline
			// restart.
			newNode := func() *stubNode {
				return singletonDownNode(layer).addIB("mlx5_9", &stubDevice{
					pciAddress: "0000:50:00.0", numaNode: 0,
					ports:           map[int]stubPort{1: upPort(layer)},
					portsUnreadable: true,
				})
			}

			mgr := freshStateManager(t)
			check, _ := newLayerCheckOn(t, layer, newNode(), testReportAfter, mgr, suppressedStart, true)

			for range checks.FirstPollDeferralLimit {
				require.Empty(t, runPoll(t, check))
			}

			require.Empty(t, unhealthyPorts(runPoll(t, check)), "the partial first poll must suppress")
			require.True(t, mgr.PendingBaseline(check.Name()), "the baseline must stay owed")
			require.Equal(t, suppressedStart, mgr.PortStatesFor(layer)["mlx5_0_1"].SuppressedSince)

			restartedMgr := reloadManager(t, mgr)
			restarted, clock := newLayerCheckOn(t, layer, newNode(), testReportAfter, restartedMgr,
				suppressedStart.Add(6*time.Minute), false)

			for range checks.FirstPollDeferralLimit {
				require.Empty(t, runPoll(t, restarted))
			}

			assert.Empty(t, unhealthyPorts(runPoll(t, restarted)),
				"the restarted pod must not report before the original delay")

			clock.Advance(4 * time.Minute)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, restarted)),
				"a pending baseline must not restart the wait")
		})
	}
}

func TestState_SuppressedPort_EnabledAfterRestart_ReportsPortsSuppressedWhileOff(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			mgr := freshStateManager(t)
			check, _, _ := newSuppressedLayerCheck(t, layer, 0, mgr, suppressedStart)
			require.Empty(t, runPoll(t, check))
			require.Equal(t, suppressedStart, mgr.PortStatesFor(layer)["mlx5_0_1"].SuppressedSince,
				"the suppression time must be saved while the option is off")

			restarted, _, clock := newSuppressedLayerCheck(t, layer, testReportAfter, reloadManager(t, mgr),
				suppressedStart.Add(6*time.Minute))
			assert.Empty(t, unhealthyPorts(runPoll(t, restarted)))

			clock.Advance(4 * time.Minute)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, restarted)),
				"enabling the option must report a port suppressed while it was off")
		})
	}
}

func TestState_SuppressedPort_DiscardedReportIsRetried(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, _, clock := newSuppressedLayerCheck(t, layer, testReportAfter, freshStateManager(t),
				suppressedStart)
			require.Empty(t, runPoll(t, check))

			clock.Advance(testReportAfter)
			events, err := check.Prepare()
			require.NoError(t, err)
			require.Equal(t, []string{"mlx5_0"}, unhealthyPorts(events))
			check.Discard()

			clock.Advance(time.Second)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, check)),
				"a discarded report must be published on the next poll")
			assert.Empty(t, runPoll(t, check))
		})
	}
}

func TestState_SuppressedPort_ComesUpFirst_Forgotten(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newSuppressedLayerCheck(t, layer, testReportAfter, freshStateManager(t),
				suppressedStart)
			require.Empty(t, runPoll(t, check))
			require.Contains(t, suppressionTimers(t, check), "mlx5_0_1")

			node.ib["mlx5_0"].ports[1] = upPort(layer)
			clock.Advance(10 * time.Second)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)))
			assert.Empty(t, suppressionTimers(t, check), "a port read healthy must be forgotten")

			clock.Advance(time.Hour)
			assert.Empty(t, runPoll(t, check))
		})
	}
}

func TestState_SuppressedPort_WaitSurvivesPodRestart(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			mgr := freshStateManager(t)
			check, _, _ := newSuppressedLayerCheck(t, layer, testReportAfter, mgr, suppressedStart)
			require.Empty(t, runPoll(t, check))

			restarted, _, clock := newSuppressedLayerCheck(t, layer, testReportAfter, reloadManager(t, mgr),
				suppressedStart.Add(6*time.Minute))
			assert.Empty(t, unhealthyPorts(runPoll(t, restarted)),
				"the restarted pod must not report before the original delay")

			clock.Advance(4 * time.Minute)
			assert.Equal(t, []string{"mlx5_0"}, unhealthyPorts(runPoll(t, restarted)),
				"the wait must be measured from the original suppression, not the restart")
		})
	}
}

func TestState_SuppressedPort_FirstSeenHealthyAfterDelay_NotReportedUnhealthy(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newSuppressedLayerCheck(t, layer, testReportAfter, freshStateManager(t),
				suppressedStart)
			require.Empty(t, runPoll(t, check))

			// The next poll comes after the deadline, and the port is up by then.
			node.ib["mlx5_0"].ports[1] = upPort(layer)
			clock.Advance(testReportAfter + time.Minute)
			events := runPoll(t, check)
			assert.Empty(t, unhealthyPorts(events), "a port that came up must not be reported unhealthy")
			assert.Len(t, healthyFor(events, "mlx5_0"), 1, "the port coming up publishes its healthy event")
			assert.Empty(t, suppressionTimers(t, check))
		})
	}
}

// dualPortNode has one dual-port compute card per PCI address. Port 1 of the
// first trainingCards cards is DOWN; port 2 of every card is uncabled.
func dualPortNode(layer string, pcis []string, trainingCards int) *stubNode {
	node := newStubNode()

	for i, pci := range pcis {
		first := upPort(layer)
		if i < trainingCards {
			first = downPort(layer)
		}

		node.addIB(fmt.Sprintf("mlx5_%d", i), &stubDevice{
			pciAddress: pci, numaNode: 0,
			ports: map[int]stubPort{1: first, 2: downPort(layer)},
		})
	}

	return node
}

func TestState_SuppressedPort_UncabledPortOnVouchedCard_NeverReported(t *testing.T) {
	for _, layer := range bothLayers {
		for _, bootIDChanged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bootIDChanged=%t", layer, bootIDChanged), func(t *testing.T) {
				// Every card has one active port out of two, so the vote is
				// decisive and no card is below the mode (#1379).
				node := dualPortNode(layer, []string{"0000:03:00.0", "0002:03:00.0"}, 0)
				check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
					suppressedStart, bootIDChanged)

				assert.Empty(t, unhealthyPorts(runPoll(t, check)))
				assert.Empty(t, suppressionTimers(t, check), "a card its peers vouch for must get no timer")

				clock.Advance(24 * time.Hour)
				assert.Empty(t, unhealthyPorts(runPoll(t, check)),
					"an uncabled port on a healthy card must never be reported")
			})
		}
	}
}

func TestState_SuppressedPort_TieThenCardsHealthy_DropsTimers(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			// Port 1 of two cards is still training at the first poll, so the
			// vote is a 2-2 tie and every down port is suppressed.
			pcis := []string{"0000:03:00.0", "0002:03:00.0", "0010:03:00.0", "0012:03:00.0"}
			node := dualPortNode(layer, pcis, 2)
			check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
				suppressedStart, false)

			require.Empty(t, unhealthyPorts(runPoll(t, check)))
			require.Len(t, suppressionTimers(t, check), 6)

			node.ib["mlx5_0"].ports[1] = upPort(layer)
			node.ib["mlx5_1"].ports[1] = upPort(layer)
			clock.Advance(30 * time.Second)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)))
			assert.Empty(t, suppressionTimers(t, check),
				"a later poll that finds the cards healthy must drop their timers")

			clock.Advance(testReportAfter)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)))
		})
	}
}

func TestState_SuppressedPort_CardVouchedWhenDue_NotReported(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			pcis := []string{"0000:03:00.0", "0002:03:00.0", "0010:03:00.0", "0012:03:00.0"}
			node := dualPortNode(layer, pcis, 2)
			check, clock := newLayerCheckOn(t, layer, node, testReportAfter, freshStateManager(t),
				suppressedStart, false)
			require.Empty(t, unhealthyPorts(runPoll(t, check)))

			// The training ports come up on the same poll the timers fall due.
			node.ib["mlx5_0"].ports[1] = upPort(layer)
			node.ib["mlx5_1"].ports[1] = upPort(layer)
			clock.Advance(testReportAfter)
			assert.Empty(t, unhealthyPorts(runPoll(t, check)),
				"a due timer must not report a port on a card its peers now vouch for")
			assert.Empty(t, suppressionTimers(t, check))
		})
	}
}

func TestEthState_SuppressedPort_ManagementSiblingAndVouchedCards_NeverReported(t *testing.T) {
	// mlx5_5 shares its card with the management NIC, so the card is exempt
	// from peer comparison. mlx5_7 is the uncabled function of a card at its
	// group's mode.
	node := l40sFrontendNode()
	reader := node.reader()
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, l40sTopology(),
		writeProcNetRoute(t, "eth0"))
	cfg := &config.Config{StateCheck: config.StateCheckConfig{ReportSuppressedAfter: testReportAfter}}
	check := NewEthernetStateCheck("node1", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), true)
	clock := &fakeClock{now: suppressedStart}
	check.now = clock.Now

	assert.Empty(t, unhealthyPorts(runPoll(t, check)))
	assert.Empty(t, check.suppressedSince, "exempt and vouched cards must get no timer")

	clock.Advance(24 * time.Hour)
	assert.Empty(t, unhealthyPorts(runPoll(t, check)))
}

func TestState_SuppressedPort_DisabledAfterRestart_StaysSilent(t *testing.T) {
	for _, layer := range []string{ibLinkLayer, ethLinkLayer} {
		t.Run(layer, func(t *testing.T) {
			mgr := freshStateManager(t)
			check, _, _ := newSuppressedLayerCheck(t, layer, testReportAfter, mgr, suppressedStart)
			require.Empty(t, runPoll(t, check))

			restarted, _, clock := newSuppressedLayerCheck(t, layer, 0, reloadManager(t, mgr),
				suppressedStart.Add(time.Minute))
			assert.Empty(t, runPoll(t, restarted), "disabling the option must silence persisted timers")

			clock.Advance(24 * time.Hour)
			assert.Empty(t, runPoll(t, restarted), "disabled reporting must stay off after the old deadline")
		})
	}
}

func TestState_SuppressedPort_EscalatedBeforeDelay_NotReportedAgain(t *testing.T) {
	for _, layer := range []string{ibLinkLayer, ethLinkLayer} {
		for _, discard := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/discard=%t", layer, discard), func(t *testing.T) {
				mgr := freshStateManager(t)
				check, node, clock := newSuppressedLayerCheck(t, layer, testReportAfter, mgr, suppressedStart)
				require.Empty(t, runPoll(t, check))

				if layer == ethLinkLayer {
					// DOWN was suppressed, then the port retrained without becoming healthy.
					node.ib["mlx5_0"].ports[1] = stubPort{
						state: "INIT", physState: checks.IBPhysLinkUp, linkLayer: layer,
					}
					clock.Advance(time.Minute)
					require.Empty(t, runPoll(t, check))
				}

				node.ib["mlx5_0"].ports[1] = stubPort{
					state: checks.IBStateDown, physState: checks.IBPhysDisabled, linkLayer: layer,
				}
				clock.Advance(time.Minute)
				events, err := check.Prepare()
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.True(t, events[0].IsFatal)

				if discard {
					check.Discard()
					retry := runPoll(t, check)
					require.Len(t, retry, 1, "a discarded escalation must still be retried")
					assert.True(t, retry[0].IsFatal)
				} else {
					check.Commit()
				}

				// Restore the fatal snapshot when rebuilding the sysfs fixture.
				restarted, restartedNode, restartedClock := newSuppressedLayerCheck(t, layer, testReportAfter,
					reloadManager(t, mgr), clock.Now().Add(time.Minute))
				restartedNode.ib["mlx5_0"].ports[1] = node.ib["mlx5_0"].ports[1]
				restartedClock.Advance(testReportAfter)
				assert.Empty(t, runPoll(t, restarted), "the suppression timer must stay cleared after restart")

				clock.Advance(testReportAfter)
				assert.Empty(t, runPoll(t, check), "escalation already reported this port")
			})
		}
	}
}
