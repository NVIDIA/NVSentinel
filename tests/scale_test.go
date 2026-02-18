//go:build amd64_group
// +build amd64_group

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
	"math"
	"math/rand"
	"testing"

	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type scaleTestContextKey int
type cursorMode string

const (
	keyNodes scaleTestContextKey = iota
	keyHealthCheckNodes
	keyOriginalCBState
	keyOriginalDeployment
	keyCircuitBreakerNodes

	cursorModeCreate cursorMode = "CREATE"
	cursorModeResume cursorMode = "RESUME"
)

func TestScaleHealthEvents(t *testing.T) {
	feature := features.New("TestScaleHealthEvents").
		WithLabel("suite", "scale")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "workloads"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		originalCBState := helpers.GetCircuitBreakerState(ctx, t, c)

		var originalDeployment *appsv1.Deployment
		ctx, _, originalDeployment = helpers.SetupQuarantineTestWithOptions(ctx, t, c,
			"data/basic-matching-configmap.yaml",
			&helpers.QuarantineSetupOptions{
				CircuitBreakerPercentage: 40,
				CircuitBreakerDuration:   "10m",
				CircuitBreakerState:      "CLOSED",
			})

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		kwokNodes, err := helpers.GetAllNodesNames(ctx, client)
		assert.NoError(t, err, "failed to get KWOK nodes")

		var allNodesList v1.NodeList
		err = client.Resources().List(ctx, &allNodesList)
		assert.NoError(t, err, "failed to get all nodes")

		totalNodesInCluster := len(allNodesList.Items)

		t.Logf("Found %d KWOK nodes, %d total nodes in cluster", len(kwokNodes), totalNodesInCluster)

		cbThresholdPercentage := 40
		nodesToCordon := min(int(math.Ceil(float64(totalNodesInCluster)*float64(cbThresholdPercentage+3)/100.0)), len(kwokNodes))

		healthCheckNodes := kwokNodes[:nodesToCordon]
		t.Logf("Selected %d KWOK nodes to cordon (43%% of %d total nodes, exceeds 40%% CB threshold)",
			len(healthCheckNodes), totalNodesInCluster)

		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		ctx = context.WithValue(ctx, keyNodes, kwokNodes)
		ctx = context.WithValue(ctx, keyHealthCheckNodes, healthCheckNodes)
		ctx = context.WithValue(ctx, keyOriginalCBState, originalCBState)
		ctx = context.WithValue(ctx, keyOriginalDeployment, originalDeployment)

		return ctx
	})

	feature.Assess("Create one pod per GPU on each node and wait for ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		namespaceName := ctx.Value(keyNamespace).(string)
		nodes := ctx.Value(keyNodes).([]string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		podTemplate := helpers.NewGPUPodSpec(namespaceName, 1)
		podTemplate.ObjectMeta.Annotations = map[string]string{
			"test.massive.annotation/config-1": generateRandomString(30000),
		}
		podTemplate.Spec.Containers[0].Env = []v1.EnvVar{
			{Name: "MASSIVE_CONFIG_1", Value: generateRandomString(30000)},
		}

		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, nodes, podTemplate)

		return ctx
	})

	feature.Assess("Send unhealthy events to trigger circuit breaker", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		err := helpers.SendHealthEventsToNodes(healthCheckNodes, "data/fatal-health-event-restart-vm.json")
		assert.NoError(t, err, "failed to send unhealthy events")

		return ctx
	})

	feature.Assess("Circuit breaker trips after 40% of total nodes cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		client, err := c.NewClient()
		require.NoError(t, err)

		var allNodesList v1.NodeList
		err = client.Resources().List(ctx, &allNodesList)
		require.NoError(t, err)

		totalNodesInCluster := len(allNodesList.Items)

		cbThreshold := int(math.Ceil(float64(totalNodesInCluster) * 0.40))

		t.Logf("Waiting for ~%d nodes (40%% of %d total nodes) to be cordoned before CB trips", cbThreshold, totalNodesInCluster)
		require.Eventually(t, func() bool {
			cordonedCount := 0
			for _, nodeName := range healthCheckNodes {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err == nil && node.Spec.Unschedulable {
					cordonedCount++
				}
			}
			percentageOfTotal := float64(cordonedCount) / float64(totalNodesInCluster) * 100
			if cordonedCount >= cbThreshold {
				t.Logf("Circuit breaker should trip: %d cordoned = %.1f%% of %d total nodes",
					cordonedCount, percentageOfTotal, totalNodesInCluster)
				return true
			}
			if cordonedCount%5 == 0 && cordonedCount > 0 {
				t.Logf("Progress: %d cordoned = %.1f%% of %d total nodes",
					cordonedCount, percentageOfTotal, totalNodesInCluster)
			}
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		cbState := helpers.GetCircuitBreakerState(ctx, t, c)
		if cbState != "TRIPPED" {
			require.Eventually(t, func() bool {
				state := helpers.GetCircuitBreakerState(ctx, t, c)
				t.Logf("Circuit breaker state: %s", state)
				return state == "TRIPPED"
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)
		}
		t.Log("Circuit breaker auto-tripped after threshold exceeded")

		return ctx
	})

	feature.Assess("Remaining nodes blocked from cordoning by circuit breaker", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		client, err := c.NewClient()
		require.NoError(t, err)

		cordonedCount := 0
		blockedNodes := []string{}
		for _, nodeName := range healthCheckNodes {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err == nil && node.Spec.Unschedulable {
				cordonedCount++
			} else if err == nil {
				blockedNodes = append(blockedNodes, nodeName)
			}
		}

		t.Logf("Cordoned: %d/%d nodes, Blocked by CB: %d nodes", cordonedCount, len(healthCheckNodes), len(blockedNodes))
		require.True(t, len(blockedNodes) > 0, "Some nodes should be blocked by circuit breaker")

		ctx = context.WithValue(ctx, keyCircuitBreakerNodes, blockedNodes)

		return ctx
	})

	feature.Assess("Force override also blocked when circuit breaker TRIPPED", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		blockedNodes := ctx.Value(keyCircuitBreakerNodes).([]string)
		require.True(t, len(blockedNodes) > 0, "Need at least one blocked node for force override test")

		testNode := blockedNodes[0]

		t.Cleanup(func() {
			helpers.SendHealthyEvent(ctx, t, testNode)
		})

		event := helpers.NewHealthEvent(testNode).
			WithErrorCode("79").
			WithMessage("XID error with force override").
			WithForceOverride()
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testNode, false)

		return ctx
	})

	feature.Assess("Check Prometheus metrics for API server health", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Check for slow LIST operations (>5 seconds at 90th percentile) // TODO this is causing failures depending on the load in CI, need a better way
		// to detect this.
		// err := helpers.CheckPrometheusMetricEmpty(ctx, t,
		// 	`histogram_quantile(0.90, sum(rate(apiserver_request_duration_seconds_bucket{verb="LIST"}[5m])) by (le, instance, resource)) > 5`,
		// 	"slow LIST operations (>5s at p90)")
		// assert.NoError(t, err, "API server LIST operations should not be slow")

		return ctx
	})

	feature.Assess("Reset circuit breaker and validate blocked nodes are not cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {

		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED", string(cursorModeCreate))

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		blockedNodes := ctx.Value(keyCircuitBreakerNodes).([]string)
		require.True(t, len(blockedNodes) > 0, "Need at least one blocked node for force override test")

		require.Never(
			t,
			func() bool {
				for _, nodeName := range blockedNodes {
					node, err := helpers.GetNodeByName(ctx, client, nodeName)
					if err == nil && node.Spec.Unschedulable {
						return true
					}
				}
				return false
			},
			helpers.NeverWaitTimeout,
			helpers.WaitInterval,
			"blocked nodes should not be cordoned",
			blockedNodes,
		)

		return ctx
	})

	feature.Assess("Send healthy events and validate cordoned nodes are uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		err := helpers.SendHealthEventsToNodes(healthCheckNodes, "data/healthy-event.json")
		assert.NoError(t, err, "failed to send healthy events")

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Log which nodes we're waiting to uncordon
		t.Logf("Waiting for %d nodes to be uncordoned: %v", len(healthCheckNodes), healthCheckNodes)

		// Track last state for detailed failure logging
		var lastCordonedNodes []string

		require.Eventually(t, func() bool {
			cordonedNodes := []string{}
			uncordonedCount := 0

			for _, nodeName := range healthCheckNodes {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					cordonedNodes = append(cordonedNodes, nodeName)
					continue
				}

				if node.Spec.Unschedulable {
					cordonedNodes = append(cordonedNodes, nodeName)
				} else {
					uncordonedCount++
				}
			}

			lastCordonedNodes = cordonedNodes
			t.Logf("Uncordon progress: %d/%d uncordoned, still cordoned: %v",
				uncordonedCount, len(healthCheckNodes), cordonedNodes)

			return len(cordonedNodes) == 0
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"nodes should be uncordoned, stuck nodes: %v", lastCordonedNodes)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED", string(cursorModeResume))

		helpers.SendHealthyEventsAsync(ctx, t, client, healthCheckNodes)

		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		err = helpers.DeleteAllCRs(ctx, t, client, helpers.RebootNodeGVK)
		assert.NoError(t, err, "failed to delete RebootNode CRs")

		originalDeployment := ctx.Value(keyOriginalDeployment).(*appsv1.Deployment)
		if originalDeployment != nil {
			helpers.RestoreFQDeployment(ctx, t, client, originalDeployment)
		}

		originalCBState := ctx.Value(keyOriginalCBState).(string)
		if originalCBState != "" {
			helpers.SetCircuitBreakerState(ctx, t, c, originalCBState, string(cursorModeResume))
		} else {
			helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED", string(cursorModeResume))
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ._-"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}
