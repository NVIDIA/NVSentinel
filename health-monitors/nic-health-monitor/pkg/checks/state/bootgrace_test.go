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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
)

const testBootGrace = 10 * time.Minute

// newGraceIBCheck builds an InfiniBand state check whose host booted 30 s
// before the test clock's start, with a boot grace of testBootGrace.
func newGraceIBCheck(
	t *testing.T, nodeName string, node *stubNode, holdDown time.Duration,
) (*InfiniBandStateCheck, *fakeClock, time.Time) {
	t.Helper()

	check, clock := newHeldIBCheck(t, nodeName, node, holdDown, freshStateManager(t))
	end := BootGraceEnd(clock.Now(), 30*time.Second, testBootGrace)
	check.cfg.StateCheck.BootGrace = testBootGrace
	check.cfg.StateCheck.BootGraceEnd = end

	return check, clock, end
}

func TestReadUptime(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+"-"+content)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))

		return p
	}

	got, err := ReadUptime(write("123.45 9876.54\n"))
	require.NoError(t, err)
	assert.Equal(t, 123450*time.Millisecond, got)

	for _, bad := range []string{"", "abc 1.0", "-5.0 1.0"} {
		_, err := ReadUptime(write(bad))
		assert.Error(t, err, "content %q", bad)
	}

	_, err = ReadUptime(filepath.Join(dir, "missing"))
	assert.Error(t, err)
}

func TestBootGraceEnd(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	assert.True(t, BootGraceEnd(now, time.Minute, 0).IsZero(), "no grace configured")
	assert.True(t, BootGraceEnd(now, 10*time.Minute, 10*time.Minute).IsZero(), "grace already over")
	assert.True(t, BootGraceEnd(now, time.Hour, 10*time.Minute).IsZero(), "grace long over")
	assert.Equal(t, now.Add(8*time.Minute), BootGraceEnd(now, 2*time.Minute, 10*time.Minute))
}

func TestIBState_BootGrace_BringUpBlipsAreNotReported(t *testing.T) {
	const nodeName = "grace-ib-blip"

	node := threeCardIBNode()
	check, clock, _ := newGraceIBCheck(t, nodeName, node, 0)

	assert.Empty(t, runPoll(t, check))

	// Bring-up: each card drops for a few seconds, one after another, with
	// no hold-down configured.
	for _, dev := range []string{"mlx5_0", "mlx5_1", "mlx5_2"} {
		node.ib[dev].ports[1] = holdIBDown

		for range 3 {
			clock.Advance(time.Second)
			assert.Empty(t, runPoll(t, check), "%s: neither port nor card may be reported during the boot grace", dev)
		}

		node.ib[dev].ports[1] = holdIBUp
		clock.Advance(time.Second)
		assert.Empty(t, runPoll(t, check), "%s: a recovery that was never reported publishes nothing", dev)
		assert.InDelta(t, 1, blips(t, nodeName, checks.InfiniBandStateCheckName, dev, "1"), 0)
	}
}

func TestIBState_BootGrace_PortStillDownWhenGraceEnds_IsReportedOnce(t *testing.T) {
	const nodeName = "grace-ib-due"

	node := threeCardIBNode()
	check, clock, end := newGraceIBCheck(t, nodeName, node, 0)

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_2"].ports[1] = holdIBDown
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.now = end.Add(-time.Second)
	assert.Empty(t, runPoll(t, check), "one second before the grace ends the port is still held")

	clock.now = end
	portFatal, cardFatal := fatalEventKinds(runPoll(t, check), "mlx5_2")
	assert.True(t, portFatal, "a port still down when the grace ends is reported")
	assert.True(t, cardFatal, "and so is its card")

	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "the fatal events are published once")
	assert.InDelta(t, 0, blips(t, nodeName, checks.InfiniBandStateCheckName, "mlx5_2", "1"), 0)

	node.ib["mlx5_2"].ports[1] = holdIBUp
	clock.Advance(time.Second)

	events := runPoll(t, check)
	require.NotEmpty(t, events, "recovery is not held")

	for _, e := range events {
		assert.True(t, e.IsHealthy)
	}
}

func TestIBState_BootGraceOver_FallsBackToHoldDown(t *testing.T) {
	t.Run("no hold-down reports at once", func(t *testing.T) {
		node := singlePortIBNode()
		check, clock, end := newGraceIBCheck(t, "grace-ib-after-0", node, 0)

		assert.Empty(t, runPoll(t, check))

		clock.now = end.Add(time.Minute)
		assert.Empty(t, runPoll(t, check))

		node.ib["mlx5_0"].ports[1] = holdIBDown
		clock.Advance(time.Second)

		events := runPoll(t, check)
		require.Len(t, events, 1, "after the grace a drop is reported on the first unhealthy poll")
		assert.True(t, events[0].IsFatal)
	})

	t.Run("hold-down applies", func(t *testing.T) {
		node := singlePortIBNode()
		check, clock, end := newGraceIBCheck(t, "grace-ib-after-hold", node, testHoldDown)

		assert.Empty(t, runPoll(t, check))

		clock.now = end.Add(time.Minute)
		node.ib["mlx5_0"].ports[1] = holdIBDown
		assert.Empty(t, runPoll(t, check))

		clock.Advance(testHoldDown - time.Second)
		assert.Empty(t, runPoll(t, check), "short of the hold-down")

		clock.Advance(time.Second)
		events := runPoll(t, check)
		require.Len(t, events, 1)
		assert.True(t, events[0].IsFatal)
	})
}

func TestIBState_BootGrace_DropJustBeforeGraceEnds_GetsFullHoldDown(t *testing.T) {
	node := singlePortIBNode()
	check, clock, end := newGraceIBCheck(t, "grace-ib-straddle", node, testHoldDown)

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.now = end.Add(-2 * time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.now = end.Add(time.Second)
	assert.Empty(t, runPoll(t, check), "the hold-down still counts from when the port went down")

	clock.now = end.Add(testHoldDown - 2*time.Second)
	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

func TestIBState_BootGrace_MissingDeviceThatReturns_IsNotReported(t *testing.T) {
	node := threeCardIBNode()
	check, clock, _ := newGraceIBCheck(t, "grace-ib-missing", node, 0)

	assert.Empty(t, runPoll(t, check))

	missing := node.ib["mlx5_1"]
	delete(node.ib, "mlx5_1")

	// Far past the three-miss debounce, as on B200 bring-up.
	for i := range 120 {
		clock.Advance(time.Second)
		assert.Empty(t, runPoll(t, check), "a missing device is held during the boot grace (poll %d)", i)
	}

	node.ib["mlx5_1"] = missing
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check), "a device that re-enumerates within the grace publishes nothing")
	assert.Empty(t, check.deviceMissCounts)
}

func TestIBState_BootGrace_DeviceStillMissingWhenGraceEnds_IsReported(t *testing.T) {
	node := threeCardIBNode()
	check, clock, end := newGraceIBCheck(t, "grace-ib-missing-due", node, 0)

	assert.Empty(t, runPoll(t, check))

	delete(node.ib, "mlx5_1")

	for range 5 {
		clock.Advance(time.Second)
		assert.Empty(t, runPoll(t, check))
	}

	clock.now = end.Add(-time.Second)
	assert.Empty(t, runPoll(t, check))

	clock.now = end
	events := runPoll(t, check)

	var disappeared bool

	for _, e := range events {
		if e.IsFatal && strings.Contains(e.Message, "disappeared") {
			disappeared = true
		}
	}

	assert.True(t, disappeared, "a device still missing when the grace ends is reported at the next poll: %v", events)
}

func TestIBState_BootGrace_OffByDefault(t *testing.T) {
	node := singlePortIBNode()
	check, clock := newHeldIBCheck(t, "grace-ib-off", node, 0, freshStateManager(t))

	assert.Empty(t, runPoll(t, check))

	node.ib["mlx5_0"].ports[1] = holdIBDown
	clock.Advance(time.Second)

	events := runPoll(t, check)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

func TestEthState_BootGrace_HoldsRoCEPortUntilGraceEnds(t *testing.T) {
	const nodeName = "grace-eth"

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

	clock := &fakeClock{now: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}
	end := BootGraceEnd(clock.Now(), 30*time.Second, testBootGrace)
	cfg := &config.Config{StateCheck: config.StateCheckConfig{BootGrace: testBootGrace, BootGraceEnd: end}}

	check := NewEthernetStateCheck(nodeName, reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)
	check.now = clock.Now

	setDown := func(down bool) {
		if down {
			node.ib["mlx5_11"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
			node.nets["eth1"] = "up" // as on OCI B300 bring-up: operstate up

			return
		}

		node.ib["mlx5_11"].ports[1] = stubPort{state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}
	}

	assert.Empty(t, runPoll(t, check))

	setDown(true)
	clock.Advance(2 * time.Second)
	assert.Empty(t, runPoll(t, check))

	setDown(false)
	clock.Advance(time.Second)
	assert.Empty(t, runPoll(t, check))
	assert.InDelta(t, 1, blips(t, nodeName, checks.EthernetStateCheckName, "mlx5_11", "1"), 0)

	setDown(true)
	clock.Advance(time.Minute)
	assert.Empty(t, runPoll(t, check))

	clock.now = end
	events := runPoll(t, check)
	require.Len(t, events, 1, "a RoCE port still down when the grace ends is reported")
	assert.True(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
}
