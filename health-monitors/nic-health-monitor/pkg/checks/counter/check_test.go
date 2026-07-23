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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// counterStubDevice describes one discoverable device for check-level
// counter tests (discovery + classification + counter reads).
type counterStubDevice struct {
	numaNode        int
	linkLayer       string
	value           uint64
	portsUnreadable bool

	// counterUnreadable fails counter reads while the device itself
	// still parses (exercises reconciliation of unobservable breaches).
	counterUnreadable bool

	// counters overrides value per counter path (e.g.
	// "counters/link_downed") so tests can drive two counters on one
	// port independently.
	counters map[string]uint64
}

type counterStubNode struct {
	devices map[string]*counterStubDevice
}

func newCounterStubNode() *counterStubNode {
	return &counterStubNode{devices: make(map[string]*counterStubDevice)}
}

func (n *counterStubNode) add(name string, d *counterStubDevice) *counterStubNode {
	n.devices[name] = d
	return n
}

func (n *counterStubNode) reader() *sysfs.MockReader {
	return &sysfs.MockReader{
		IBBase: "/sys/class/infiniband",
		ListDirsFunc: func(path string) ([]string, error) {
			switch {
			case path == "/sys/class/infiniband":
				names := make([]string, 0, len(n.devices))
				for k := range n.devices {
					names = append(names, k)
				}

				sort.Strings(names)

				return names, nil
			case strings.HasSuffix(path, "/ports"):
				parts := strings.Split(path, "/")
				dev := parts[len(parts)-2]

				d := n.devices[dev]
				if d == nil {
					return nil, nil
				}

				if d.portsUnreadable {
					return nil, fmt.Errorf("ports directory unreadable for %s", dev)
				}

				return []string{"1"}, nil
			}

			return nil, nil
		},
		ReadIBPortLinkLayerFunc: func(device string, _ int) (string, error) {
			d := n.devices[device]
			if d == nil {
				return "", nil
			}

			return d.linkLayer, nil
		},
		ReadIBDeviceFieldFunc: func(_, field string) (string, error) {
			if field == "device/vendor" {
				return "0x15b3", nil
			}

			return "", nil
		},
		ReadIBDeviceNUMAFunc: func(device string) (int, error) {
			d := n.devices[device]
			if d == nil {
				return -1, nil
			}

			return d.numaNode, nil
		},
		ReadIBPortCounterFunc: func(device string, _ int, counterPath string) (uint64, error) {
			d := n.devices[device]
			if d == nil {
				return 0, fmt.Errorf("unknown device %s", device)
			}

			if d.counterUnreadable {
				return 0, fmt.Errorf("counter unreadable for %s", device)
			}

			if v, ok := d.counters[counterPath]; ok {
				return v, nil
			}

			return d.value, nil
		},
	}
}

// counterClassifier builds a real classifier from a synthetic metadata
// file: one GPU on NUMA 0, plus the given per-device topology levels.
// Devices on NUMA != 0 with all-SYS levels classify as Management.
func counterClassifier(t *testing.T, reader sysfs.Reader, topo map[string][]string) *topology.Classifier {
	t.Helper()

	meta := &model.GPUMetadata{
		GPUs:        []model.GPUInfo{{PCIAddress: "0000:0f:00.0", NUMANode: 0}},
		NICTopology: topo,
	}

	path := filepath.Join(t.TempDir(), "gpu_metadata.json")
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	c, err := topology.LoadFromMetadata(path, reader)
	require.NoError(t, err)

	return c
}

func counterStateManager(t *testing.T) *statefile.Manager {
	t.Helper()

	mgr, _, _ := counterStateManagerWithPaths(t)

	return mgr
}

func counterStateManagerWithPaths(t *testing.T) (*statefile.Manager, string, string) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bootPath := filepath.Join(dir, "boot_id")
	require.NoError(t, os.WriteFile(bootPath, []byte("boot-1\n"), 0o644))

	mgr := statefile.NewManagerWithPaths(statePath, bootPath)
	require.NoError(t, mgr.Load())

	return mgr, statePath, bootPath
}

func counterCfg(override string, counters ...config.CounterConfig) *config.Config {
	return &config.Config{
		NicInclusionRegexOverride: override,
		CounterDetection: config.CounterDetectionConfig{
			Enabled:  true,
			Counters: counters,
		},
	}
}

func TestCounterCheck_TwoCountersOnePort_PartialRecovery(t *testing.T) {
	// Every counter event carries the counter name as ErrorCode, so
	// downstream clears conditions per counter. Two counters breached on
	// the same port must produce two distinctly-coded faults, and
	// resetting only one must recover only that one — the sibling's
	// condition and local latch stay intact.
	linkErrRecovery := config.CounterConfig{
		Name:          "link_error_recovery",
		Path:          "counters/link_error_recovery",
		Enabled:       true,
		IsFatal:       false,
		ThresholdType: "delta",
		Threshold:     0,
		Description:   "micro-flapping",
	}

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{
			numaNode: 0, linkLayer: testIBLayer,
			counters: map[string]uint64{
				"counters/link_downed":         0,
				"counters/link_error_recovery": 0,
			},
		})
	reader := node.reader()
	classifier := counterClassifier(t, reader, map[string][]string{"mlx5_0": {"PIX"}})
	mgr := counterStateManager(t)

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0), linkErrRecovery), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	_, err := check.Run()
	require.NoError(t, err)

	// Both counters breach.
	node.devices["mlx5_0"].counters["counters/link_downed"] = 1
	node.devices["mlx5_0"].counters["counters/link_error_recovery"] = 1

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 2)

	codes := map[string]bool{}
	for _, e := range events {
		require.Len(t, e.ErrorCode, 1, "every counter event must carry exactly one code")
		codes[e.ErrorCode[0]] = e.IsFatal
	}

	require.Contains(t, codes, "link_downed")
	require.Contains(t, codes, "link_error_recovery")
	assert.True(t, codes["link_downed"])
	assert.False(t, codes["link_error_recovery"])

	// Only link_downed is reset.
	node.devices["mlx5_0"].counters["counters/link_downed"] = 0

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "only the reset counter may recover")
	assert.True(t, events[0].IsHealthy)
	require.Equal(t, []string{"link_downed"}, events[0].ErrorCode,
		"the recovery must carry the same code as its breach")

	flags := mgr.BreachFlags()
	assert.NotContains(t, flags, "mlx5_0:1:link_downed")
	require.Contains(t, flags, "mlx5_0:1:link_error_recovery")
	assert.True(t, flags["mlx5_0:1:link_error_recovery"].Breached,
		"the sibling counter's latch must be untouched")
}

// computeAndManagementNode is the canonical two-device fixture: mlx5_0
// is a compute NIC (GPU NUMA, PIX) and mlx5_mgmt is a management NIC
// (non-GPU NUMA, all-SYS) — the L40S-style Mellanox management function
// that is visible in /sys/class/infiniband.
func computeAndManagementNode(linkLayer string) *counterStubNode {
	return newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: linkLayer}).
		add("mlx5_mgmt", &counterStubDevice{numaNode: 1, linkLayer: linkLayer})
}

func computeAndManagementTopo() map[string][]string {
	return map[string][]string{"mlx5_0": {"PIX"}, "mlx5_mgmt": {"SYS"}}
}

func TestCounterCheck_ManagementDeviceExcluded(t *testing.T) {
	// A fatal counter increment on a management-classified NIC must not
	// produce any degradation event: counter checks share the state
	// checks' device scope. Before this scoping, a single link blip on
	// an L40S-style management function emitted REPLACE_VM.
	node := computeAndManagementNode(testIBLayer)
	reader := node.reader()
	classifier := counterClassifier(t, reader, computeAndManagementTopo())
	require.True(t, classifier.IsManagementNIC("mlx5_mgmt"), "fixture must classify mlx5_mgmt as management")

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, counterStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "first poll seeds snapshots")

	node.devices["mlx5_0"].value++
	node.devices["mlx5_mgmt"].value++

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "only the compute NIC's breach may be reported")
	assert.True(t, events[0].IsFatal)

	for _, e := range events {
		for _, ent := range e.EntitiesImpacted {
			assert.NotEqual(t, "mlx5_mgmt", ent.EntityValue,
				"management NIC must never appear in degradation events")
		}
	}
}

func TestCounterCheck_OverridePinnedManagementDeviceStillMonitored(t *testing.T) {
	// The explicit inclusion override bypasses every device filter,
	// including the management classification — explicit operator
	// intent replaces topology evidence.
	node := computeAndManagementNode(testIBLayer)
	reader := node.reader()
	classifier := counterClassifier(t, reader, computeAndManagementTopo())

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("^mlx5_mgmt$", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, counterStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	node.devices["mlx5_mgmt"].value++

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "the pinned management NIC must be monitored")
	assert.Equal(t, "mlx5_mgmt", events[0].EntitiesImpacted[0].EntityValue)
}

func TestCounterCheck_BootBaselineEmitsCheckScopedClearFirst(t *testing.T) {
	// The first poll after a host reboot emits the check-scoped clear
	// (no entities: downstream wipes every stale condition for this
	// check, including ones whose devices were renamed or removed) ahead
	// of the per-counter baselines, and strictly earlier by timestamp —
	// the platform-connector re-sorts batches by GeneratedTimestamp with
	// a non-stable sort.
	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer})
	reader := node.reader()
	classifier := counterClassifier(t, reader, map[string][]string{"mlx5_0": {"PIX"}})

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, counterStateManager(t), true)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 2, "check-scoped clear + one baseline per readable counter")

	clearEvt := events[0]
	assert.True(t, clearEvt.IsHealthy)
	assert.Empty(t, clearEvt.EntitiesImpacted)
	assert.Empty(t, clearEvt.ErrorCode,
		"the check-scoped clear must stay code-less: empty code means clear everything")

	baseline := events[1]
	assert.True(t, baseline.IsHealthy)
	assert.NotEmpty(t, baseline.EntitiesImpacted)
	assert.Equal(t, []string{"link_downed"}, baseline.ErrorCode,
		"per-counter baselines must carry the counter code")
	assert.True(t,
		clearEvt.GeneratedTimestamp.AsTime().Before(baseline.GeneratedTimestamp.AsTime()),
		"the clear must sort strictly before same-batch events under timestamp ordering")

	// Subsequent polls are normal.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestCounterCheck_SweepClearsLatchOfDisabledCounter(t *testing.T) {
	// A breach latched by a counter that is later removed from the
	// configuration can never recover through evaluation. The sweep
	// emits the recovery (correct entities, original check name) and
	// deletes the persisted flag — previously this condition was
	// orphaned downstream until the next reboot.
	mgr := counterStateManager(t)
	mgr.UpdateBreachFlags(map[string]statefile.CounterBreachFlag{
		"mlx5_0:1:link_downed": {
			Breached:  true,
			CheckName: checks.InfiniBandDegradationCheckName,
			IsFatal:   true,
		},
	})

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer})
	reader := node.reader()
	classifier := counterClassifier(t, reader, map[string][]string{"mlx5_0": {"PIX"}})

	// link_downed is no longer in the enabled counter set.
	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg(""), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "the stale latch must produce exactly one recovery")

	evt := events[0]
	assert.True(t, evt.IsHealthy)
	assert.False(t, evt.IsFatal)
	assert.Equal(t, checks.InfiniBandDegradationCheckName, evt.CheckName)
	assert.Equal(t, []string{"link_downed"}, evt.ErrorCode,
		"the sweep recovery must carry the counter code so it clears only its own condition")
	require.Len(t, evt.EntitiesImpacted, 2)
	assert.Equal(t, "mlx5_0", evt.EntitiesImpacted[0].EntityValue)
	assert.Equal(t, "1", evt.EntitiesImpacted[1].EntityValue)

	assert.NotContains(t, mgr.BreachFlags(), "mlx5_0:1:link_downed",
		"the persisted flag must be deleted once the recovery commits")
}

func TestCounterCheck_SweepClearsLatchOfNowIneligibleDevice(t *testing.T) {
	// The rollout hazard of scoping counter checks to the classifier: a
	// breach latched on a device that is now management-classified would
	// never be evaluated again. The sweep clears it.
	mgr := counterStateManager(t)
	mgr.UpdateBreachFlags(map[string]statefile.CounterBreachFlag{
		"mlx5_mgmt:1:link_downed": {
			Breached:  true,
			CheckName: checks.InfiniBandDegradationCheckName,
			IsFatal:   true,
		},
	})

	node := computeAndManagementNode(testIBLayer)
	reader := node.reader()
	classifier := counterClassifier(t, reader, computeAndManagementTopo())

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "the ineligible device's latch must produce a recovery")
	assert.True(t, events[0].IsHealthy)
	assert.Equal(t, "mlx5_mgmt", events[0].EntitiesImpacted[0].EntityValue)

	assert.NotContains(t, mgr.BreachFlags(), "mlx5_mgmt:1:link_downed")
}

func TestCounterCheck_SweepSpares_AbsentDevice_NetKeys_ForeignChecks(t *testing.T) {
	// Three latches the IB check's sweep must NOT touch: a device merely
	// absent from discovery (absence can be a transient firmware reset —
	// the reboot baseline clear handles it if it never returns), an
	// interface-level net key (entities not reconstructible; non-fatal),
	// and a latch owned by the sibling check. The sibling check then
	// sweeps its own stale latch, proving ownership scoping.
	mgr := counterStateManager(t)
	mgr.UpdateBreachFlags(map[string]statefile.CounterBreachFlag{
		"mlx5_gone:1:link_downed": {
			Breached:  true,
			CheckName: checks.InfiniBandDegradationCheckName,
			IsFatal:   true,
		},
		"net:eth0:carrier_changes": {
			Breached:  true,
			CheckName: checks.EthernetDegradationCheckName,
		},
		"mlx5_mgmt:1:link_downed": {
			Breached:  true,
			CheckName: checks.EthernetDegradationCheckName,
			IsFatal:   true,
		},
	})

	node := computeAndManagementNode(testIBLayer)
	reader := node.reader()
	classifier := counterClassifier(t, reader, computeAndManagementTopo())
	cfg := counterCfg("", deltaCounter(0))

	ibCheck := NewInfiniBandDegradationCheck(testNode, reader, cfg, classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err := ibCheck.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "the IB check must not sweep absent-device, net, or sibling-owned latches")

	flags := mgr.BreachFlags()
	assert.Contains(t, flags, "mlx5_gone:1:link_downed")
	assert.Contains(t, flags, "net:eth0:carrier_changes")
	assert.Contains(t, flags, "mlx5_mgmt:1:link_downed")

	// The Ethernet check owns the mlx5_mgmt latch and sweeps it (the
	// device is discovered but ineligible). The net key stays: entities
	// are not reconstructible from interface-level keys.
	ethCheck := NewEthernetDegradationCheck(testNode, reader, cfg, classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	events, err = ethCheck.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, checks.EthernetDegradationCheckName, events[0].CheckName)
	assert.Equal(t, "mlx5_mgmt", events[0].EntitiesImpacted[0].EntityValue)

	flags = mgr.BreachFlags()
	assert.NotContains(t, flags, "mlx5_mgmt:1:link_downed")
	assert.Contains(t, flags, "net:eth0:carrier_changes")
	assert.Contains(t, flags, "mlx5_gone:1:link_downed")
}

func TestCounterCheck_DelayedBootBaseline_MonitorsThenReconciles(t *testing.T) {
	// With a baseline owed after a reboot and one unreadable device, the
	// check must NOT emit the check-scoped clear on the partial polls —
	// that would wipe the unreadable device's previous-boot conditions
	// with nothing able to re-assert them. Readable devices are
	// monitored normally in the meantime, and a breach latched during
	// that window is re-asserted right after the clear when the
	// reconciliation finally runs.
	mgr := counterStateManager(t)

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer}).
		add("mlx5_9", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer, portsUnreadable: true})
	reader := node.reader()
	classifier := counterClassifier(t, reader,
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_9": {"PIX"}})

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	require.True(t, mgr.PendingBaseline(checks.InfiniBandDegradationCheckName),
		"construction must register the owed baseline")

	for i := 1; i <= checks.FirstPollDeferralLimit; i++ {
		events, err := check.Run()
		require.NoError(t, err)
		assert.Empty(t, events, "poll %d must be deferred", i)
	}

	// Partial monitoring starts: silent seeding, no clear, baseline
	// still owed.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "the partial poll must not emit the clear or baselines")
	assert.Contains(t, mgr.CounterSnapshots(), "mlx5_0:1:link_downed",
		"the readable device must be seeded during the window")
	assert.True(t, mgr.PendingBaseline(checks.InfiniBandDegradationCheckName))

	// A breach during the window is reported in real time.
	node.devices["mlx5_0"].value++

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "the window breach must be reported immediately")
	require.True(t, events[0].IsFatal)

	// The unreadable device recovers: the reconciliation clears
	// everything and re-asserts the window-latched breach after the
	// clear, in the same batch.
	node.devices["mlx5_9"].portsUnreadable = false

	events, err = check.Run()
	require.NoError(t, err)

	require.NotEmpty(t, events)
	clearEvt := events[0]
	assert.True(t, clearEvt.IsHealthy)
	assert.Empty(t, clearEvt.EntitiesImpacted, "the clear must be first in the batch")

	var reasserted, baseline *pb.HealthEvent

	for _, e := range events[1:] {
		switch {
		case e.IsFatal && e.EntitiesImpacted[0].EntityValue == "mlx5_0":
			reasserted = e
		case e.IsHealthy && len(e.EntitiesImpacted) > 0 && e.EntitiesImpacted[0].EntityValue == "mlx5_9":
			baseline = e
		}
	}

	require.NotNil(t, reasserted, "the window-latched breach must be re-asserted after the clear")
	assert.Contains(t, reasserted.Message, "re-asserting")
	require.NotNil(t, baseline, "the recovered device must get its healthy baseline")

	for _, e := range events[1:] {
		assert.True(t, clearEvt.GeneratedTimestamp.AsTime().Before(e.GeneratedTimestamp.AsTime()),
			"the clear must sort strictly before every re-asserted event")
	}

	assert.False(t, mgr.PendingBaseline(checks.InfiniBandDegradationCheckName),
		"the reconciliation must retire the pending flag")

	flags := mgr.BreachFlags()
	require.Contains(t, flags, "mlx5_0:1:link_downed")
	assert.True(t, flags["mlx5_0:1:link_downed"].Breached,
		"the replayed breach stays latched until a counter reset")

	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "the poll after the reconciliation must be silent")
}

// delayedBaselineFixture drives a check to the reconciliation-pending
// state: deferred polls, one partial poll, then a window breach on
// mlx5_0. Returns the node so the caller can stage the reconciliation.
func delayedBaselineFixture(t *testing.T, mgr *statefile.Manager) (*counterStubNode, *InfiniBandDegradationCheck) {
	t.Helper()

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer}).
		add("mlx5_9", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer, portsUnreadable: true})
	reader := node.reader()
	classifier := counterClassifier(t, reader,
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_9": {"PIX"}})

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	for i := 1; i <= checks.FirstPollDeferralLimit+1; i++ {
		_, err := check.Run()
		require.NoError(t, err)
	}

	node.devices["mlx5_0"].value++

	events, err := check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "precondition: window breach latched")
	require.True(t, events[0].IsFatal)

	return node, check
}

func TestCounterCheck_ResetOnReconciliationConsumesWindowBreach(t *testing.T) {
	// A counter administratively reset between the last partial poll and
	// the reconciliation poll must NOT have its window breach replayed:
	// the reset is definitive recovery evidence, and the replay would
	// publish a false condition right after the clear while destroying
	// the evidence of the reset.
	mgr := counterStateManager(t)
	node, check := delayedBaselineFixture(t, mgr)

	// Admin clears the counter; the unreadable device recovers on the
	// same poll, so the reconciliation is the first to observe the reset.
	node.devices["mlx5_0"].value = 0
	node.devices["mlx5_9"].portsUnreadable = false

	events, err := check.Run()
	require.NoError(t, err)

	require.NotEmpty(t, events)
	assert.Empty(t, events[0].EntitiesImpacted, "the clear leads the batch")

	for _, e := range events {
		assert.False(t, e.IsFatal, "no breach may be replayed after an observed reset: %s", e.Message)
	}

	assert.NotContains(t, mgr.BreachFlags(), "mlx5_0:1:link_downed",
		"the reset must consume the window latch")

	// The counter must evaluate normally afterwards: a fresh increment
	// re-breaches from the post-reset baseline.
	node.devices["mlx5_0"].value++

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

func TestCounterCheck_Reconciliation_ReplaysAbsentDeviceBreach(t *testing.T) {
	// A window breach whose device is absent from the reconciliation
	// poll's enumeration cannot be replayed through evaluation. The
	// persisted identity re-asserts it so the clear cannot leave a
	// desynchronized latch (downstream healthy, local breached — which
	// would suppress all future breaches on the key).
	mgr := counterStateManager(t)
	node, check := delayedBaselineFixture(t, mgr)

	delete(node.devices, "mlx5_0")
	node.devices["mlx5_9"].portsUnreadable = false

	events, err := check.Run()
	require.NoError(t, err)

	require.NotEmpty(t, events)
	assert.Empty(t, events[0].EntitiesImpacted, "the clear leads the batch")

	var reasserted *pb.HealthEvent

	for _, e := range events[1:] {
		if e.IsFatal && e.EntitiesImpacted[0].EntityValue == "mlx5_0" {
			reasserted = e
		}
	}

	require.NotNil(t, reasserted, "the absent device's window breach must be re-asserted")
	assert.Contains(t, reasserted.Message, "not currently readable")
	assert.Equal(t, []string{"link_downed"}, reasserted.ErrorCode,
		"the replay must carry the original counter code")

	flags := mgr.BreachFlags()
	require.Contains(t, flags, "mlx5_0:1:link_downed")
	assert.True(t, flags["mlx5_0:1:link_downed"].Breached, "the latch must be retained")
}

func TestCounterCheck_Reconciliation_ReplaysUnreadableCounterBreach(t *testing.T) {
	// Same as the absent-device case, but the device parses fine and
	// only the counter file itself fails to read.
	mgr := counterStateManager(t)
	node, check := delayedBaselineFixture(t, mgr)

	node.devices["mlx5_0"].counterUnreadable = true
	node.devices["mlx5_9"].portsUnreadable = false

	events, err := check.Run()
	require.NoError(t, err)

	var reasserted *pb.HealthEvent

	for _, e := range events {
		if e.IsFatal && len(e.EntitiesImpacted) > 0 && e.EntitiesImpacted[0].EntityValue == "mlx5_0" {
			reasserted = e
		}
	}

	require.NotNil(t, reasserted, "the unreadable counter's window breach must be re-asserted")
	assert.True(t, mgr.BreachFlags()["mlx5_0:1:link_downed"].Breached)
}

func TestReplayUnobservedBreaches_IdentityAndLegacyHandling(t *testing.T) {
	// Interface-level keys are only replayable through the identity
	// recorded at breach time; a legacy flag without identity is
	// consumed so the local latch can never desynchronize from the
	// downstream clear.
	ev := NewEvaluator(testNode, &sysfs.MockReader{}, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		nil, map[string]statefile.CounterBreachFlag{
			"net:eth0:carrier_changes": {
				Breached: true, CheckName: checks.EthernetDegradationCheckName,
				Device: "mlx5_0", Port: "1",
			},
			"net:eth1:rx_errors": {
				Breached: true, CheckName: checks.EthernetDegradationCheckName,
			},
		}, false)

	events := ev.ReplayUnobservedBreaches(checks.EthernetDegradationCheckName)

	require.Len(t, events, 1, "only the identity-bearing flag is replayable")
	assert.Equal(t, "mlx5_0", events[0].EntitiesImpacted[0].EntityValue)
	assert.Equal(t, "1", events[0].EntitiesImpacted[1].EntityValue)

	flags := ev.BreachFlags()
	require.Contains(t, flags, "net:eth1:rx_errors")
	assert.False(t, flags["net:eth1:rx_errors"].Breached, "the legacy flag must be consumed")

	// The identity-bearing latch is retained: replaying again finds it.
	assert.Len(t, ev.ReplayUnobservedBreaches(checks.EthernetDegradationCheckName), 1)
}

func TestCounterCheck_PendingBootBaselineSurvivesRestart(t *testing.T) {
	// Window commits persist the new boot ID, so a pod restart inside
	// the window compares equal boot IDs; the persisted pending flag is
	// what keeps the owed clear alive.
	mgr, statePath, bootPath := counterStateManagerWithPaths(t)

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer}).
		add("mlx5_9", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer, portsUnreadable: true})
	reader := node.reader()
	classifier := counterClassifier(t, reader,
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_9": {"PIX"}})

	firstPod := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, true)

	for i := 1; i <= checks.FirstPollDeferralLimit+1; i++ {
		_, err := firstPod.Run()
		require.NoError(t, err)
	}

	require.Contains(t, mgr.CounterSnapshots(), "mlx5_0:1:link_downed", "window commit must have happened")

	mgr2 := statefile.NewManagerWithPaths(statePath, bootPath)
	require.NoError(t, mgr2.Load())
	require.False(t, mgr2.BootIDChanged())
	require.True(t, mgr2.PendingBaseline(checks.InfiniBandDegradationCheckName),
		"the owed baseline must survive the restart via the persisted flag")

	node.devices["mlx5_9"].portsUnreadable = false

	secondPod := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr2, false)

	events, err := secondPod.Run()
	require.NoError(t, err)
	require.NotEmpty(t, events, "the restarted pod must perform the owed reconciliation")
	assert.True(t, events[0].IsHealthy)
	assert.Empty(t, events[0].EntitiesImpacted, "the clear must be first in the batch")
	assert.False(t, mgr2.PendingBaseline(checks.InfiniBandDegradationCheckName))
}

func TestCounterCheck_FirstPollDeferralIsBounded(t *testing.T) {
	// One enumerable-but-unreadable device must not disable counter
	// monitoring of every readable NIC forever: after the deferral limit
	// the poll proceeds with the readable subset.
	mgr := counterStateManager(t)

	node := newCounterStubNode().
		add("mlx5_0", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer}).
		add("mlx5_9", &counterStubDevice{numaNode: 0, linkLayer: testIBLayer, portsUnreadable: true})
	reader := node.reader()
	classifier := counterClassifier(t, reader,
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_9": {"PIX"}})

	check := NewInfiniBandDegradationCheck(testNode, reader,
		counterCfg("", deltaCounter(0)), classifier,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)

	for i := 1; i <= checks.FirstPollDeferralLimit; i++ {
		events, err := check.Run()
		require.NoError(t, err)
		assert.Empty(t, events, "poll %d must be deferred", i)
		assert.Empty(t, mgr.CounterSnapshots(), "nothing may commit while deferred")
	}

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "the seeding poll emits no events")
	assert.Contains(t, mgr.CounterSnapshots(), "mlx5_0:1:link_downed",
		"the readable device must be monitored after the deferral limit")

	node.devices["mlx5_0"].value++

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "breaches on the readable device must now be detected")
	assert.True(t, events[0].IsFatal)
}
