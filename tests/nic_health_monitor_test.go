//go:build arm64_group
// +build arm64_group

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
	"strings"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type nicContextKey string

const (
	keyNICNodeName nicContextKey = "nicNodeName"
	keyNICPodName  nicContextKey = "nicPodName"
	keyNICState    nicContextKey = "nicState"

	ethCheckName = "EthernetStateCheck"
	ibCheckName  = "InfiniBandStateCheck"
)

// TestNICHealthMonitorRoCEStateDetection tests RoCE (Ethernet link-layer)
// port DOWN detection, recovery, and device disappearance.
func TestNICHealthMonitorRoCEStateDetection(t *testing.T) {
	feature := features.New("NIC Health Monitor - RoCE State Detection").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "roce-state")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	// Test 1: Healthy baseline — fresh start with all ports ACTIVE/LinkUp
	feature.Assess("Healthy baseline on fresh start", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying EthernetStateCheck condition is False (healthy) after first poll")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	// Test 2: RoCE port DOWN detection — mlx5_8 (Ethernet management NIC) goes DOWN
	feature.Assess("RoCE port DOWN detection", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Mutating mlx5_8 port 1 to DOWN/Disabled")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "1: DOWN", "3: Disabled")

		t.Log("Verifying EthernetStateCheck condition becomes True (unhealthy)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "RoCE port mlx5_8 port 1", "", corev1.ConditionTrue)

		return ctx
	})

	// Test 3: RoCE port recovery — mlx5_8 restored to ACTIVE/LinkUp
	feature.Assess("RoCE port recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Restoring mlx5_8 port 1 to ACTIVE/LinkUp")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "4: ACTIVE", "5: LinkUp")

		t.Log("Verifying EthernetStateCheck condition returns to False (healthy)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	// Test 4: RoCE device disappearance — mlx5_8 directory removed
	feature.Assess("RoCE device disappearance", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Removing mlx5_8 from fake sysfs")
		helpers.DeleteSysfsDevice(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8")

		t.Log("Verifying EthernetStateCheck condition shows device disappearance")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "disappeared", "", corev1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorIBStateDetection tests InfiniBand port DOWN
// detection and recovery using the mlx5_0/mlx5_1 IB compute NICs.
func TestNICHealthMonitorIBStateDetection(t *testing.T) {
	feature := features.New("NIC Health Monitor - IB State Detection").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "ib-state")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	// Test 5: IB port DOWN detection — mlx5_0 goes DOWN
	feature.Assess("IB port DOWN detection", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying InfiniBandStateCheck baseline is healthy")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		t.Log("Mutating mlx5_0 port 1 to DOWN/Disabled")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "1: DOWN", "3: Disabled")

		t.Log("Verifying InfiniBandStateCheck condition becomes True (unhealthy)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "Port mlx5_0 port 1", "", corev1.ConditionTrue)

		return ctx
	})

	// Test 6: IB port recovery — mlx5_0 restored
	feature.Assess("IB port recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Restoring mlx5_0 port 1 to ACTIVE/LinkUp")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "4: ACTIVE", "5: LinkUp")

		t.Log("Verifying InfiniBandStateCheck condition returns to False (healthy)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorPersistence verifies that the NIC monitor's
// persisted state file survives a pod restart and that a recovery event
// is emitted when a DOWN port is fixed during the restart window.
func TestNICHealthMonitorPersistence(t *testing.T) {
	feature := features.New("NIC Health Monitor - Persistence Across Restart").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "persistence")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("Persist state across pod restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)
		podName := ctx.Value(keyNICPodName).(string)

		t.Log("Verifying healthy baseline before triggering fault")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		t.Log("Mutating mlx5_8 port 1 to DOWN — creating a fault")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "1: DOWN", "3: Disabled")

		t.Log("Waiting for EthernetStateCheck to detect the fault")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "RoCE port mlx5_8 port 1", "", corev1.ConditionTrue)

		t.Log("Restoring mlx5_8 BEFORE restarting pod — recovery during restart window")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "4: ACTIVE", "5: LinkUp")

		t.Log("Restarting NIC monitor pod — state file should persist on hostPath")
		newPod := helpers.RestartNICMonitorPod(t, ctx, client,
			helpers.NVSentinelNamespace, podName, nodeName)

		ctx = context.WithValue(ctx, keyNICPodName, newPod.Name)

		t.Log("Verifying recovery event — new pod reads persisted DOWN, sees ACTIVE, emits healthy")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorBootIDChange verifies that a changed boot_id
// (simulated host reboot) causes the NIC monitor to discard old state
// and re-emit healthy baselines for all ports.
func TestNICHealthMonitorBootIDChange(t *testing.T) {
	feature := features.New("NIC Health Monitor - Boot ID Change").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "boot-id")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("Boot ID change triggers healthy baseline re-emission", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)
		podName := ctx.Value(keyNICPodName).(string)

		t.Log("Verifying healthy EthernetStateCheck baseline on initial boot ID")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		t.Log("Mutating mlx5_8 to DOWN so condition flips to unhealthy")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "1: DOWN", "3: Disabled")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "RoCE port mlx5_8 port 1", "", corev1.ConditionTrue)

		t.Log("Restoring mlx5_8 to ACTIVE")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "4: ACTIVE", "5: LinkUp")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		t.Log("Writing new boot_id to simulate host reboot")
		helpers.MutateBootID(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "test-boot-id-REBOOT-0000-0000-0000-111111111111")

		t.Log("Restarting NIC monitor pod to trigger boot-ID comparison")
		newPod := helpers.RestartNICMonitorPod(t, ctx, client,
			helpers.NVSentinelNamespace, podName, nodeName)

		ctx = context.WithValue(ctx, keyNICPodName, newPod.Name)

		t.Log("Verifying healthy baseline is re-emitted after boot-ID change")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorVFSkip verifies that SR-IOV Virtual Function
// devices (mlx5_9, mlx5_10) are excluded from monitoring and never
// appear in node condition messages.
func TestNICHealthMonitorVFSkip(t *testing.T) {
	feature := features.New("NIC Health Monitor - VF Skip").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "vf-exclusion")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("VF devices are excluded from monitoring", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying healthy baseline (VFs should not appear)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		t.Log("Mutating VF mlx5_9 to DOWN — should NOT trigger a condition change")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_9", "1", "1: DOWN", "3: Disabled")

		t.Log("Verifying InfiniBandStateCheck remains healthy (VF DOWN ignored)")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		t.Log("Observing for 10s that no condition message mentions mlx5_9 or mlx5_10")
		require.Never(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			for _, condition := range node.Status.Conditions {
				msg := condition.Message
				if strings.Contains(msg, "mlx5_9") || strings.Contains(msg, "mlx5_10") {
					t.Logf("FAIL: VF device found in condition message: %s", msg)
					return true
				}
			}

			return false
		}, 10*time.Second, helpers.WaitInterval,
			"VF devices should never appear in node conditions")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorIBDeviceDisappearance tests the IB device
// disappearance path: an IB device directory is removed entirely,
// simulating hardware ejection or driver unload.
func TestNICHealthMonitorIBDeviceDisappearance(t *testing.T) {
	feature := features.New("NIC Health Monitor - IB Device Disappearance").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "ib-disappearance")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("IB device disappearance detection", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying IB healthy baseline")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		t.Log("Removing mlx5_1 (InfiniBand device) from fake sysfs")
		helpers.DeleteSysfsDevice(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_1")

		t.Log("Verifying InfiniBandStateCheck detects device disappearance")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "disappeared", "", corev1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorMultiPortFault verifies that simultaneous faults
// on both link layers (IB and Ethernet) are detected independently
// and both conditions reflect the correct state.
func TestNICHealthMonitorMultiPortFault(t *testing.T) {
	feature := features.New("NIC Health Monitor - Multi Port Fault").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "multi-fault")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("Simultaneous IB and RoCE faults", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying both check baselines are healthy")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		t.Log("Bringing down mlx5_8 (Ethernet) and mlx5_0 (IB) simultaneously")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "1: DOWN", "3: Disabled")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "1: DOWN", "3: Disabled")

		t.Log("Verifying both conditions are unhealthy")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "RoCE port mlx5_8 port 1", "", corev1.ConditionTrue)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "Port mlx5_0 port 1", "", corev1.ConditionTrue)

		t.Log("Restoring both ports")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "4: ACTIVE", "5: LinkUp")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_0", "1", "4: ACTIVE", "5: LinkUp")

		t.Log("Verifying both conditions return to healthy")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorMultipleDownRecoveryCycles verifies the NIC
// monitor correctly tracks multiple DOWN→ACTIVE→DOWN cycles on the
// same port without getting stuck or missing transitions.
func TestNICHealthMonitorMultipleDownRecoveryCycles(t *testing.T) {
	feature := features.New("NIC Health Monitor - Multiple Cycles").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "cycles")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("Three DOWN-recovery cycles on same port", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying healthy baseline")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		for cycle := 1; cycle <= 3; cycle++ {
			t.Logf("--- Cycle %d: bringing mlx5_0 DOWN ---", cycle)
			helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
				nodeName, "mlx5_0", "1", "1: DOWN", "3: Disabled")

			helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
				ibCheckName, "Port mlx5_0 port 1", "", corev1.ConditionTrue)

			t.Logf("--- Cycle %d: restoring mlx5_0 ---", cycle)
			helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
				nodeName, "mlx5_0", "1", "4: ACTIVE", "5: LinkUp")

			helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)
		}

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorPhysStateDisabled verifies that a port with
// state=ACTIVE but phys_state=Disabled is still detected as unhealthy
// (the physical layer is what matters for real traffic).
func TestNICHealthMonitorPhysStateDisabled(t *testing.T) {
	feature := features.New("NIC Health Monitor - PhysState Disabled").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "phys-state")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("PhysState Disabled triggers unhealthy condition", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying healthy baseline")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		t.Log("Setting mlx5_1 to ACTIVE with phys_state=Disabled")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_1", "1", "4: ACTIVE", "3: Disabled")

		t.Log("Verifying IB condition shows unhealthy for mlx5_1")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "Port mlx5_1 port 1", "", corev1.ConditionTrue)

		t.Log("Restoring mlx5_1")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_1", "1", "4: ACTIVE", "5: LinkUp")

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ibCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestNICHealthMonitorConditionMessage verifies the exact shape of node
// condition messages for both healthy and unhealthy transitions.
func TestNICHealthMonitorConditionMessage(t *testing.T) {
	feature := features.New("NIC Health Monitor - Condition Message Format").
		WithLabel("suite", "nic-health-monitor").
		WithLabel("component", "message-format")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, nicPod, state := helpers.SetUpNICHealthMonitor(ctx, t, client)

		ctx = context.WithValue(ctx, keyNICNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNICPodName, nicPod.Name)
		ctx = context.WithValue(ctx, keyNICState, state)

		return ctx
	})

	feature.Assess("DOWN message format includes state and phys_state", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNICNodeName).(string)

		t.Log("Verifying healthy baseline")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		t.Log("Bringing mlx5_8 DOWN")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "1: DOWN", "3: Disabled")

		t.Log("Verifying message format contains state and phys_state info")
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				ethCheckName, ethCheckName+"IsNotHealthy")
			if err != nil || condition == nil {
				return false
			}

			msg := condition.Message
			hasDevice := strings.Contains(msg, "mlx5_8")
			hasState := strings.Contains(msg, "DOWN")
			hasPhysState := strings.Contains(msg, "Disabled")

			if hasDevice && hasState && hasPhysState {
				t.Logf("Message format verified: %s", msg)
				return true
			}

			t.Logf("Message missing expected fields: device=%v state=%v phys=%v msg=%s",
				hasDevice, hasState, hasPhysState, msg)
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Condition message should contain device, state, and phys_state")

		t.Log("Restoring mlx5_8")
		helpers.MutateSysfsState(t, ctx, client, helpers.NVSentinelNamespace,
			nodeName, "mlx5_8", "1", "4: ACTIVE", "5: LinkUp")

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			ethCheckName, "", "", corev1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		state, _ := ctx.Value(keyNICState).(*helpers.NICTestState)
		helpers.TearDownNICHealthMonitor(ctx, t, client, state)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
