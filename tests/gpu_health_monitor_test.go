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
	dcgmServiceHost = "nvidia-dcgm.gpu-operator.svc"
	dcgmServicePort = "5555"
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
		}, helpers.WaitTimeout, helpers.WaitInterval, "all injected error conditions should appear")

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

		t.Logf("Removing node conditions from %s", nodeName)
		for _, clearCmd := range clearCommands {
			err := helpers.RemoveNodeCondition(ctx, client, nodeName, clearCmd.condition)
			if err != nil {
				t.Logf("Warning: failed to remove %s condition: %v", clearCmd.condition, err)
			}
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
