//go:build amd64_group
// +build amd64_group

// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type nicCounterContextKey string

const (
	keyCounterNodeName nicCounterContextKey = "nicCounterNodeName"
	keyCounterPodName  nicCounterContextKey = "nicCounterPodName"
	keyCounterState    nicCounterContextKey = "nicCounterState"

	// State checks use their own identity for link-state conditions.
	ibStateCheckName  = "InfiniBandStateCheck"
	ethStateCheckName = "EthernetStateCheck"

	// All counter events retain the DEGRADATION check identity. Fatal events
	// update Node Conditions; non-fatal events create Node Events.
	ibDegCheckName  = "InfiniBandDegradationCheck"
	ethDegCheckName = "EthernetDegradationCheck"

	ibDegUnhealthyReason  = "InfiniBandDegradationCheckIsNotHealthy"
	ethDegUnhealthyReason = "EthernetDegradationCheckIsNotHealthy"
)

// TestNICCounterIBDegradation covers all InfiniBand counter scenarios
// in a single setup/teardown cycle: fatal delta breach, latching,
// recovery via reset, per-second velocity, per-minute velocity, and
// multi-counter faults.
func TestNICCounterIBDegradation(t *testing.T) {
	feature := features.New("NIC Counter - IB Degradation").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "ib-counter")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyCounterNodeName, nodeName)
		ctx = context.WithValue(ctx, keyCounterPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyCounterState, state)

		return ctx
	})

	feature.Assess("IB healthy baseline", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Verifying InfiniBandStateCheck condition is False (healthy)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibStateCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	// --- Fatal delta counter tests (use node Conditions) ---

	feature.Assess("link_downed fatal delta breach", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Incrementing link_downed on mlx5_0")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed", "1")

		t.Log("Verifying InfiniBandDegradationCheck condition becomes True with link_downed message")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "link_downed", "", corev1.ConditionTrue)

		return ctx
	})

	feature.Assess("Breach stays latched", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Observing for 15s that InfiniBandDegradationCheck remains True (latched)")
		require.Never(t, func() bool {
			node, getErr := helpers.GetNodeByName(ctx, client, nodeName)
			if getErr != nil {
				require.NoError(t, getErr, "failed to read node while verifying latch behavior")
				return false
			}

			for _, cond := range node.Status.Conditions {
				if string(cond.Type) == ibDegCheckName && cond.Status == corev1.ConditionFalse {
					return true
				}
			}

			return false
		}, 15*time.Second, helpers.WaitInterval,
			"Latched breach should NOT self-recover")

		return ctx
	})

	feature.Assess("link_downed reset recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Resetting link_downed to 0 on mlx5_0 (simulating admin counter clear)")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed")

		t.Log("Verifying InfiniBandDegradationCheck condition returns to False (recovery)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	// --- Non-fatal velocity counter tests (use node Events) ---

	feature.Assess("symbol_error per-second velocity breach", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing InfiniBandDegradationCheck events to avoid stale matches")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ibDegCheckName, ibDegUnhealthyReason)

		t.Log("Writing symbol_error=100 on mlx5_0 (rate >> 10/sec threshold)")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/symbol_error", "100")

		t.Log("Verifying InfiniBandDegradationCheck node Event is created")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ibDegCheckName,
			Reason: ibDegUnhealthyReason,
		})

		return ctx
	})

	feature.Assess("symbol_error reset clears latch", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Resetting symbol_error to 0 on mlx5_0")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/symbol_error")

		t.Log("Deleting old events, then re-triggering to prove latch was cleared")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ibDegCheckName, ibDegUnhealthyReason)

		t.Log("Writing symbol_error=200 to trigger a fresh breach")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/symbol_error", "200")

		t.Log("Verifying new breach event appears (proves reset cleared the latch)")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ibDegCheckName,
			Reason: ibDegUnhealthyReason,
		})

		t.Log("Final reset to leave clean state")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/symbol_error")

		return ctx
	})

	feature.Assess("link_error_recovery per-minute velocity breach", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing InfiniBandDegradationCheck events")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ibDegCheckName, ibDegUnhealthyReason)

		t.Log("Writing link_error_recovery=100 on mlx5_0 (will breach > 5/min after 60s window)")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_error_recovery", "100")

		t.Log("Waiting up to 150s for the 60s velocity window to elapse and emit event")
		require.Eventually(t, func() bool {
			found, _ := helpers.CheckNodeEventExists(ctx, client, nodeName,
				ibDegCheckName, ibDegUnhealthyReason)
			return found
		}, 150*time.Second, helpers.WaitInterval,
			"InfiniBandDegradationCheck event should appear after 60s window elapses")

		return ctx
	})

	feature.Assess("link_error_recovery reset", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Resetting link_error_recovery to 0 on mlx5_0 to clear latch for next tests")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_error_recovery")

		return ctx
	})

	// --- Multi-counter fatal test (uses node Conditions) ---

	feature.Assess("Multiple fatal counters on different devices", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Incrementing link_downed on mlx5_0 and excessive_buffer_overrun_errors on mlx5_1")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed", "1")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_1", "1", "counters/excessive_buffer_overrun_errors", "1")

		t.Log("Verifying InfiniBandDegradationCheck becomes True")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "link_downed", "", corev1.ConditionTrue)

		t.Log("Resetting both counters")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_1", "1", "counters/excessive_buffer_overrun_errors")

		t.Log("Verifying InfiniBandDegradationCheck returns to False")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		state, _ := ctx.Value(keyCounterState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICCounterEthernetDegradation covers Ethernet counter scenarios:
// carrier_changes (delta, net sysfs path) and roce_slow_restart
// (velocity, IB sysfs path for RoCE port). Both are non-fatal, so
// assertions use node Events instead of node Conditions.
func TestNICCounterEthernetDegradation(t *testing.T) {
	feature := features.New("NIC Counter - Ethernet Degradation").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "eth-counter")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyCounterNodeName, nodeName)
		ctx = context.WithValue(ctx, keyCounterPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyCounterState, state)

		return ctx
	})

	feature.Assess("carrier_changes breach via net sysfs path", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing EthernetDegradationCheck events")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ethDegCheckName, ethDegUnhealthyReason)

		t.Log("Writing carrier_changes=5 on eth0 (delta=5, threshold > 2)")
		helpers.MutateNetCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "eth0", "carrier_changes", "5")

		t.Log("Verifying EthernetDegradationCheck node Event is created")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ethDegCheckName,
			Reason: ethDegUnhealthyReason,
		})

		return ctx
	})

	feature.Assess("carrier_changes reset clears latch", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Resetting carrier_changes to 0 on eth0")
		helpers.MutateNetCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "eth0", "carrier_changes", "0")

		t.Log("Deleting old events, then re-triggering to prove latch was cleared")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ethDegCheckName, ethDegUnhealthyReason)

		t.Log("Writing carrier_changes=10 to trigger a fresh breach")
		helpers.MutateNetCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "eth0", "carrier_changes", "10")

		t.Log("Verifying new breach event appears (proves reset cleared the latch)")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ethDegCheckName,
			Reason: ethDegUnhealthyReason,
		})

		t.Log("Final reset to leave clean state")
		helpers.MutateNetCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "eth0", "carrier_changes", "0")

		return ctx
	})

	feature.Assess("roce_slow_restart velocity breach on RoCE port", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing EthernetDegradationCheck events")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ethDegCheckName, ethDegUnhealthyReason)

		t.Log("Writing roce_slow_restart=100 on mlx5_8 (rate >> 10/sec threshold)")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "hw_counters/roce_slow_restart", "100")

		t.Log("Verifying EthernetDegradationCheck node Event is created")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ethDegCheckName,
			Reason: ethDegUnhealthyReason,
		})

		return ctx
	})

	feature.Assess("roce_slow_restart reset clears latch", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Resetting roce_slow_restart to 0 on mlx5_8")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "hw_counters/roce_slow_restart")

		t.Log("Deleting old events, then re-triggering to prove latch was cleared")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ethDegCheckName, ethDegUnhealthyReason)

		t.Log("Writing roce_slow_restart=200 to trigger a fresh breach")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "hw_counters/roce_slow_restart", "200")

		t.Log("Verifying new breach event appears (proves reset cleared the latch)")
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, corev1.Event{
			Type:   ethDegCheckName,
			Reason: ethDegUnhealthyReason,
		})

		t.Log("Final reset to leave clean state")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "hw_counters/roce_slow_restart")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		state, _ := ctx.Value(keyCounterState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICCounterBelowThreshold verifies that counters incremented below
// their configured threshold do NOT trigger events. Guards against the
// monitor firing on any non-zero delta regardless of config.
func TestNICCounterBelowThreshold(t *testing.T) {
	feature := features.New("NIC Counter - Below Threshold No Event").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "counter-negative")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyCounterNodeName, nodeName)
		ctx = context.WithValue(ctx, keyCounterPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyCounterState, state)

		return ctx
	})

	feature.Assess("Healthy baseline", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethStateCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Assess("carrier_changes below delta threshold does not trigger", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing EthernetDegradationCheck events")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ethDegCheckName, ethDegUnhealthyReason)

		t.Log("Writing carrier_changes=1 on eth0 (delta=1, threshold > 2, should NOT breach)")
		helpers.MutateNetCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "eth0", "carrier_changes", "1")

		t.Log("Observing for 15s that no EthernetDegradationCheck unhealthy event appears")
		helpers.EnsureNodeEventNotPresent(ctx, t, client, nodeName,
			ethDegCheckName, ethDegUnhealthyReason)

		return ctx
	})

	feature.Assess("symbol_error below velocity threshold does not trigger", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Deleting existing InfiniBandDegradationCheck events")
		_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, ibDegCheckName, ibDegUnhealthyReason)

		t.Log("Writing symbol_error=5 on mlx5_0 (rate=5/sec, threshold > 10/sec, should NOT breach)")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/symbol_error", "5")

		t.Log("Observing for 15s that no InfiniBandDegradationCheck unhealthy event appears")
		helpers.EnsureNodeEventNotPresent(ctx, t, client, nodeName,
			ibDegCheckName, ibDegUnhealthyReason)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		state, _ := ctx.Value(keyCounterState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICCounterBootIDClearsBreachState verifies that a boot_id change
// clears counter breach state and emits healthy baselines. This needs
// its own setup because it restarts the pod mid-test.
func TestNICCounterBootIDClearsBreachState(t *testing.T) {
	feature := features.New("NIC Counter - Boot ID Clears Breach").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "counter-bootid")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyCounterNodeName, nodeName)
		ctx = context.WithValue(ctx, keyCounterPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyCounterState, state)

		return ctx
	})

	feature.Assess("Trigger link_downed breach", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)

		t.Log("Verifying healthy baseline first")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibStateCheckName, "", "", corev1.ConditionFalse)

		t.Log("Incrementing link_downed on mlx5_0")
		helpers.MutateSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed", "1")

		t.Log("Waiting for InfiniBandDegradationCheck to become True")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "link_downed", "", corev1.ConditionTrue)

		return ctx
	})

	feature.Assess("Boot ID change clears breach", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyCounterNodeName).(string)
		podName := ctx.Value(keyCounterPodName).(string)

		newBootID := fmt.Sprintf("test-counter-bootid-%d", time.Now().UnixNano())
		t.Logf("Writing new boot_id %s to simulate reboot", newBootID)
		helpers.MutateBootID(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, newBootID)

		t.Log("Resetting link_downed to 0 (counters reset on reboot)")
		helpers.ResetSysfsCounter(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "counters/link_downed")

		t.Log("Restarting NIC monitor pod to trigger boot-ID comparison")
		newPod := helpers.RestartNICMonitorPod(t, ctx, client,
			helpers.NVSentinelNamespace, podName, nodeName)

		ctx = context.WithValue(ctx, keyCounterPodName, newPod.Name)

		t.Log("Verifying InfiniBandDegradationCheck returns to False (healthy baselines emitted)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibDegCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		state, _ := ctx.Value(keyCounterState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
