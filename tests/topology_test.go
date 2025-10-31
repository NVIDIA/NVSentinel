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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	DCGMVersionLabel     = "nvsentinel.dgxc.nvidia.com/dcgm.version"
	DriverInstalledLabel = "nvsentinel.dgxc.nvidia.com/driver.installed"
	KataEnabledLabel     = "nvsentinel.dgxc.nvidia.com/kata.enabled"
	DCGMDeployLabel      = "nvidia.com/gpu.deploy.dcgm"
	DriverDeployLabel    = "nvidia.com/gpu.deploy.driver"
)

func TestTopologyLabels(t *testing.T) {
	feature := features.New("TestTopologyLabelsAndHealthMonitors").
		WithLabel("suite", "topology")

	feature.Assess("Verify DCGM topology: node selector -> labels -> DCGM pods -> GPU health monitors", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		allNodes, err := helpers.GetAllNodesNames(ctx, client)
		assert.NoError(t, err, "failed to get cluster nodes")

		t.Logf("Checking DCGM topology on %d nodes", len(allNodes))
		for _, nodeName := range allNodes {
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				assert.NoError(t, err, "failed to get node %s", nodeName)

				dcgmDeploy, exists := node.Labels[DCGMDeployLabel]
				if !exists || dcgmDeploy != "true" {
					return true
				}

				dcgmVersion, exists := node.Labels[DCGMVersionLabel]
				assert.True(t, exists, "Node %s with DCGM deploy label missing DCGM version label", nodeName)
				assert.Contains(t, []string{"3.x", "4.x"}, dcgmVersion, "Node %s has unexpected DCGM version label: %s", nodeName, dcgmVersion)

				pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), nodeName)
				assert.NoError(t, err, "failed to get pods on node %s", nodeName)

				assert.True(t, hasGPUHealthMonitorPods(pods), "Node %s with DCGM deploy label missing GPU health monitor pods", nodeName)

				return true
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "DCGM topology validation failed for node %s", nodeName)
		}

		return ctx
	})

	feature.Assess("Verify driver topology: node selector -> labels -> driver pods -> syslog health monitors", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		allNodes, err := helpers.GetAllNodesNames(ctx, client)
		assert.NoError(t, err, "failed to get cluster nodes")

		t.Logf("Checking driver topology on %d nodes", len(allNodes))
		for _, nodeName := range allNodes {
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				assert.NoError(t, err, "failed to get node %s", nodeName)

				driverDeploy, exists := node.Labels[DriverDeployLabel]
				if !exists || driverDeploy != "true" {
					return true
				}

				driverInstalled, exists := node.Labels[DriverInstalledLabel]
				assert.True(t, exists, "Node %s with driver deploy label missing driver installed label", nodeName)
				assert.Equal(t, "true", driverInstalled, "Node %s has incorrect driver installed label: %s", nodeName, driverInstalled)

				pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), nodeName)
				assert.NoError(t, err, "failed to get pods on node %s", nodeName)

				assert.True(t, hasSyslogHealthMonitorPods(pods), "Node %s with driver deploy label missing syslog health monitor pods", nodeName)

				return true
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "driver topology validation failed for node %s", nodeName)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestKataTopology(t *testing.T) {
	feature := features.New("TestKataTopologyLabelsAndDaemonSets").
		WithLabel("suite", "topology")

	feature.Assess("Verify Kata topology: node runtime -> kata.enabled label -> correct syslog DaemonSet", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		allNodes, err := helpers.GetAllNodesNames(ctx, client)
		assert.NoError(t, err, "failed to get cluster nodes")

		t.Logf("Checking Kata topology on %d nodes", len(allNodes))

		// Track validated nodes by type to avoid counting duplicates across Eventually retries
		kataNodes := make(map[string]bool)
		regularNodes := make(map[string]bool)

		for _, nodeName := range allNodes {
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				assert.NoError(t, err, "failed to get node %s", nodeName)

				// Check if kata.enabled label exists (should be set by labeler)
				kataEnabled, exists := node.Labels[KataEnabledLabel]
				if !exists {
					t.Logf("Node %s missing kata.enabled label (labeler may not have processed it yet)", nodeName)
					return false
				}

				assert.Contains(t, []string{"true", "false"}, kataEnabled, "Node %s has invalid kata.enabled label: %s", nodeName, kataEnabled)

				// Get pods on this node
				pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), nodeName)
				assert.NoError(t, err, "failed to get pods on node %s", nodeName)

				// Find syslog health monitor pods
				var syslogPods []v1.Pod
				for _, pod := range pods {
					if name, exists := pod.Labels["app.kubernetes.io/name"]; exists && name == "syslog-health-monitor" {
						syslogPods = append(syslogPods, pod)
					}
				}

				if len(syslogPods) == 0 {
					t.Logf("Node %s has no syslog-health-monitor pods yet", nodeName)
					return false
				}

				// Verify the correct DaemonSet variant is scheduled based on kata.enabled
				nodeValidated := false
				expectedKataValue := kataEnabled
				for _, pod := range syslogPods {
					podKataLabel, hasPodLabel := pod.Labels["nvsentinel.dgxc.nvidia.com/kata"]
					assert.True(t, hasPodLabel, "Node %s has syslog pod without kata label", nodeName)
					assert.Equal(t, expectedKataValue, podKataLabel, "Node %s has syslog pod with wrong kata label: expected %s, got %s", nodeName, expectedKataValue, podKataLabel)

					if !nodeValidated {
						if kataEnabled == "true" {
							kataNodes[nodeName] = true
							t.Logf("✓ Kata node %s correctly has kata syslog DaemonSet pod", nodeName)
						} else {
							regularNodes[nodeName] = true
							t.Logf("✓ Regular node %s correctly has regular syslog DaemonSet pod", nodeName)
						}
						nodeValidated = true
					}
				}

				return true
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Kata topology validation failed for node %s", nodeName)
		}

		kataNodeCount := len(kataNodes)
		regularNodeCount := len(regularNodes)
		t.Logf("Kata topology validated: %d Kata nodes, %d regular nodes", kataNodeCount, regularNodeCount)

		// Verify we have both types of nodes in the test cluster
		assert.Greater(t, kataNodeCount, 0, "No Kata nodes found - check KWOK node templates")
		assert.Greater(t, regularNodeCount, 0, "No regular nodes found - check KWOK node templates")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func hasGPUHealthMonitorPods(pods []v1.Pod) bool {
	for _, pod := range pods {
		if name, exists := pod.Labels["app.kubernetes.io/name"]; exists && name == "gpu-health-monitor" {
			return true
		}
	}

	return false
}

func hasSyslogHealthMonitorPods(pods []v1.Pod) bool {
	for _, pod := range pods {
		if name, exists := pod.Labels["app.kubernetes.io/name"]; exists && name == "syslog-health-monitor" {
			return true
		}
	}

	return false
}
