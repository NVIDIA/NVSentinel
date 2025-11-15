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
	"strings"
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

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		gpuHealthMonitorPod, err := helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "gpu-health-monitor")
		require.NoError(t, err, "failed to find GPU health monitor pod on worker node")
		require.NotNil(t, gpuHealthMonitorPod, "GPU health monitor pod should exist on worker node")

		testNodeName = gpuHealthMonitorPod.Spec.NodeName
		t.Logf("Using GPU health monitor pod: %s on node: %s", gpuHealthMonitorPod.Name, testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, helpers.NVSentinelNamespace, testNodeName, metadata)

		t.Logf("Restarting GPU health monitor pod %s to load metadata", gpuHealthMonitorPod.Name)
		err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, gpuHealthMonitorPod.Name)
		require.NoError(t, err, "failed to restart GPU health monitor pod")
		helpers.WaitForPodsDeleted(ctx, t, client, helpers.NVSentinelNamespace, []string{gpuHealthMonitorPod.Name})

		t.Logf("Waiting for GPU health monitor pod to be ready on node %s", testNodeName)
		pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), testNodeName)
		require.NoError(t, err, "failed to get pods on node %s", testNodeName)

		newGPUHealthMonitorPodName := ""
		for _, pod := range pods {
			if strings.Contains(pod.Name, "gpu-health-monitor") && pod.Name != gpuHealthMonitorPod.Name {
				newGPUHealthMonitorPodName = pod.Name
				break
			}
		}

		require.NotEmpty(t, newGPUHealthMonitorPodName, "new GPU health monitor pod name not found")

		helpers.WaitForPodsRunning(ctx, t, client, helpers.NVSentinelNamespace, []string{newGPUHealthMonitorPodName})

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keyPodName, newGPUHealthMonitorPodName)
		return ctx
	})

	feature.Assess("Inject multiple errors and verify all conditions appear", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeNameVal := ctx.Value(keyNodeName)
		require.NotNil(t, nodeNameVal, "nodeName not found in context")
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keyPodName)
		require.NotNil(t, podNameVal, "podName not found in context")
		podName := podNameVal.(string)

		restConfig := client.RESTConfig()

		// GPU 0 has UUID and PCI address according to test metadata
		expectedGPUUUID := "GPU-00000000-0000-0000-0000-000000000000"
		expectedPCIAddress := "0000:17:00.0"

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

			stdout, stderr, execErr := helpers.ExecInPod(ctx, restConfig, helpers.NVSentinelNamespace, podName, "", cmd)
			require.NoError(t, execErr, "failed to inject %s error: %s", dcgmError.name, stderr)
			require.Contains(t, stdout, "Successfully injected", "%s error injection failed", dcgmError.name)
		}

		t.Logf("Waiting for node conditions to appear with PCI addresses and GPU UUIDs")
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
				if condition == nil {
					foundConditions[dcgmError.condition] = false
					continue
				}

				if !strings.Contains(condition.Message, expectedPCIAddress) {
					t.Logf("Condition %s found but missing expected PCI address %s: %s",
						dcgmError.condition, expectedPCIAddress, condition.Message)
					foundConditions[dcgmError.condition] = false
					continue
				}

				if !strings.Contains(condition.Message, expectedGPUUUID) {
					t.Logf("Condition %s found but missing expected GPU UUID %s: %s",
						dcgmError.condition, expectedGPUUUID, condition.Message)
					foundConditions[dcgmError.condition] = false
					continue
				}

				t.Logf("Found %s condition with expected PCI address %s and GPU UUID %s: %s",
					dcgmError.condition, expectedPCIAddress, expectedGPUUUID, condition.Message)
				foundConditions[dcgmError.condition] = true
			}

			allFound := true
			for _, found := range foundConditions {
				if !found {
					allFound = false
					break
				}
			}

			return allFound
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "all injected error conditions should appear with PCI addresses and GPU UUIDs")

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

		podNameVal := ctx.Value(keyPodName)
		if podNameVal == nil {
			t.Log("Skipping teardown: podName not set (setup likely failed early)")
			return ctx
		}
		podName := podNameVal.(string)

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
			_, _, _ = helpers.ExecInPod(ctx, restConfig, helpers.NVSentinelNamespace, podName, "", cmd)
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

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

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
