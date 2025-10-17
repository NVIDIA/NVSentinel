// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
			}, helpers.WaitTimeout, helpers.WaitInterval, "DCGM topology validation failed for node %s", nodeName)
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
			}, helpers.WaitTimeout, helpers.WaitInterval, "driver topology validation failed for node %s", nodeName)
		}

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
