// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	dcgmServiceHost      = "nvidia-dcgm.gpu-operator.svc"
	dcgmServicePort      = "5555"
	gpuOperatorNamespace = "gpu-operator"
	dcgmServiceName      = "nvidia-dcgm"
	dcgmOriginalPort     = 5555
	dcgmBrokenPort       = 1555
)

// TestGPUHealthMonitorMultipleErrors verifies GPU health monitor handles multiple concurrent errors
func TestGPUHealthMonitorMultipleErrors(t *testing.T) {
	feature := features.New("GPU Health Monitor - Multiple Concurrent Errors").
		WithLabel("suite", "gpu-health-monitor").
		WithLabel("component", "multi-error")

	var testNodeName string
	var gpuHealthMonitorPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		gpuHealthMonitorPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "gpu-health-monitor")
		require.NoError(t, err, "failed to find GPU health monitor pod on worker node")
		require.NotNil(t, gpuHealthMonitorPod, "GPU health monitor pod should exist on worker node")

		testNodeName = gpuHealthMonitorPod.Spec.NodeName
		t.Logf("Using GPU health monitor pod: %s on node: %s", gpuHealthMonitorPod.Name, testNodeName)

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		return ctx
	})

	feature.Assess("Inject multiple errors and verify all conditions appear", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeNameVal := ctx.Value(keyNodeName)
		require.NotNil(t, nodeNameVal, "nodeName not found in context")
		nodeName := nodeNameVal.(string)

		restConfig := client.RESTConfig()

		errors := []struct {
			name      string
			fieldID   string
			value     string
			condition string
			reason    string
		}{
			{"Inforom", "84", "0", "GpuInforomWatch", "GpuInforomWatchIsNotHealthy"},
			{"Memory", "395", "1", "GpuMemWatch", "GpuMemWatchIsNotHealthy"},
		}

		for _, dcgmError := range errors {
			t.Logf("Injecting %s error on node %s", dcgmError.name, nodeName)
			cmd := []string{"/bin/sh", "-c",
				fmt.Sprintf("dcgmi test --host %s:%s --inject --gpuid 0 -f %s -v %s",
					dcgmServiceHost, dcgmServicePort, dcgmError.fieldID, dcgmError.value)}

			stdout, stderr, execErr := helpers.ExecInPod(ctx, restConfig, helpers.NVSentinelNamespace, gpuHealthMonitorPod.Name, "", cmd)
			require.NoError(t, execErr, "failed to inject %s error: %s", dcgmError.name, stderr)
			require.Contains(t, stdout, "Successfully injected", "%s error injection failed", dcgmError.name)
		}

		t.Logf("Waiting for node conditions to appear")
		require.Eventually(t, func() bool {
			foundConditions := make(map[string]bool)
			for _, dcgmError := range errors {
				condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
					dcgmError.condition, dcgmError.reason)
				if err != nil {
					t.Logf("Error checking condition %s: %v", dcgmError.condition, err)
					foundConditions[dcgmError.condition] = false
					continue
				}
				found := condition != nil
				foundConditions[dcgmError.condition] = found
				if found {
					t.Logf("Found %s condition: %s", dcgmError.condition, condition.Message)
				}
			}

			allFound := true
			for _, found := range foundConditions {
				if !found {
					allFound = false
					break
				}
			}

			return allFound
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "all injected error conditions should appear")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)
		restConfig := client.RESTConfig()

		clearCommands := []struct {
			name      string
			fieldID   string
			value     string
			condition string
		}{
			{"Inforom", "84", "1", "GpuInforomWatch"},
			{"Memory", "395", "0", "GpuMemWatch"},
		}

		t.Logf("Clearing injected errors on node %s", nodeName)
		for _, clearCmd := range clearCommands {
			cmd := []string{"/bin/sh", "-c",
				fmt.Sprintf("dcgmi test --host %s:%s --inject --gpuid 0 -f %s -v %s",
					dcgmServiceHost, dcgmServicePort, clearCmd.fieldID, clearCmd.value)}
			_, _, _ = helpers.ExecInPod(ctx, restConfig, helpers.NVSentinelNamespace, gpuHealthMonitorPod.Name, "", cmd)
		}

		t.Logf("Waiting for node conditions to be cleared automatically on %s", nodeName)
		for _, clearCmd := range clearCommands {
			t.Logf("  Waiting for %s condition to clear", clearCmd.condition)
			require.Eventually(t, func() bool {
				condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
					clearCmd.condition, "")
				if err != nil {
					return false
				}
				// Condition should either be removed or become healthy (Status=False)
				if condition == nil {
					t.Logf("  %s condition removed", clearCmd.condition)
					return true
				}
				if condition.Status == v1.ConditionFalse {
					t.Logf("  %s condition became healthy", clearCmd.condition)
					return true
				}
				t.Logf("  %s condition still unhealthy: %s", clearCmd.condition, condition.Message)
				return false
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "%s condition should be cleared", clearCmd.condition)
		}

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestGPUHealthMonitorDCGMConnectionError verifies GPU health monitor detects DCGM connectivity failures
func TestGPUHealthMonitorDCGMConnectionError(t *testing.T) {
	feature := features.New("GPU Health Monitor - DCGM Connection Error").
		WithLabel("suite", "gpu-health-monitor").
		WithLabel("component", "dcgm-connectivity")

	var testNodeName string
	var gpuHealthMonitorPodName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		gpuHealthMonitorPod, err := helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "gpu-health-monitor")
		require.NoError(t, err, "failed to find GPU health monitor pod on worker node")
		require.NotNil(t, gpuHealthMonitorPod, "GPU health monitor pod should exist on worker node")

		testNodeName = gpuHealthMonitorPod.Spec.NodeName
		gpuHealthMonitorPodName = gpuHealthMonitorPod.Name
		t.Logf("Using GPU health monitor pod: %s on node: %s", gpuHealthMonitorPodName, testNodeName)

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		return ctx
	})

	feature.Assess("break DCGM connection and verify condition", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		t.Log("Breaking DCGM communication by changing service port")
		err = helpers.PatchServicePort(ctx, client, gpuOperatorNamespace, dcgmServiceName, dcgmBrokenPort)
		require.NoError(t, err, "failed to patch DCGM service port")

		t.Logf("Restarting GPU health monitor pod %s to trigger reconnection", gpuHealthMonitorPodName)
		err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, gpuHealthMonitorPodName)
		require.NoError(t, err, "failed to restart GPU health monitor pod")

		t.Logf("Waiting for GpuDcgmConnectivityFailure condition on node %s", nodeName)
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"GpuDcgmConnectivityFailure", "GpuDcgmConnectivityFailureIsNotHealthy")
			if err != nil {
				t.Logf("Error checking condition: %v", err)
				return false
			}
			if condition == nil {
				t.Log("Condition not found yet")
				return false
			}

			t.Logf("Found condition - Status: %s, Reason: %s, Message: %s",
				condition.Status, condition.Reason, condition.Message)
			return condition.Status == v1.ConditionTrue
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "GpuDcgmConnectivityFailure condition should appear")

		return ctx
	})

	feature.Assess("restore DCGM connection and verify recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		t.Log("Restoring DCGM communication by restoring service port")
		err = helpers.PatchServicePort(ctx, client, gpuOperatorNamespace, dcgmServiceName, dcgmOriginalPort)
		require.NoError(t, err, "failed to restore DCGM service port")

		t.Logf("Waiting for GpuDcgmConnectivityFailure condition to become healthy on node %s", nodeName)
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"GpuDcgmConnectivityFailure", "GpuDcgmConnectivityFailureIsHealthy")
			if err != nil {
				t.Logf("Error checking condition: %v", err)
				return false
			}
			if condition == nil {
				t.Log("Condition not found")
				return false
			}

			t.Logf("Found condition - Status: %s, Reason: %s, Message: %s",
				condition.Status, condition.Reason, condition.Message)

			// Condition should have Status=False when healthy
			return condition.Status == v1.ConditionFalse
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "GpuDcgmConnectivityFailure should become healthy")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		t.Log("Ensuring DCGM service port is restored")
		err = helpers.PatchServicePort(ctx, client, gpuOperatorNamespace, dcgmServiceName, dcgmOriginalPort)
		if err != nil {
			t.Logf("Warning: failed to restore DCGM service port: %v", err)
		}

		t.Logf("Waiting for GpuDcgmConnectivityFailure condition to clear on node %s", nodeName)
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"GpuDcgmConnectivityFailure", "")
			if err != nil {
				return false
			}
			if condition == nil || condition.Status == v1.ConditionFalse {
				return true
			}
			t.Logf("Condition still present: Status=%s", condition.Status)
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "GpuDcgmConnectivityFailure should clear")

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
