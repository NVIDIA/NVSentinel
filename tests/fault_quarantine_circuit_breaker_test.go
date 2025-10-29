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
	"os"
	"testing"
	"tests/helpers"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestCircuitBreaker(t *testing.T) {
	feature := features.New("TestCircuitBreaker").
		WithLabel("suite", "fault-quarantine-circuit-breaker")

	var testCtx *helpers.QuarantineTestContext
	var originalCBState string
	var originalDeployment *appsv1.Deployment
	var testNodes []string
	var threshold int

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		originalCBState = helpers.GetCircuitBreakerState(ctx, t, c)

		var newCtx context.Context
		newCtx, testCtx, originalDeployment = helpers.SetupQuarantineTestWithOptions(ctx, t, c,
			"data/basic-matching-configmap.yaml",
			&helpers.QuarantineSetupOptions{
				CircuitBreakerPercentage: 20,
				CircuitBreakerDuration:   "10m",
				CircuitBreakerState:      "CLOSED",
			})

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err)

		startIdx := int(float64(len(nodes)) * 0.50)
		if startIdx >= len(nodes) {
			startIdx = len(nodes) - 1
		}
		testNodes = nodes[startIdx:]
		t.Logf("Using %d test nodes for circuit breaker tests", len(testNodes))

		return newCtx
	})

	feature.Assess("manually TRIPPED circuit breaker blocks normal cordoning", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SetCircuitBreakerState(ctx, t, c, "TRIPPED")

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		tempFile, err := helpers.SendHealthEventWithTemplate(testCtx.NodeName, event)
		require.NoError(t, err)
		defer os.Remove(tempFile)

		t.Log("Verifying node NOT cordoned when circuit breaker is manually TRIPPED")

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testCtx.NodeName, false)

		return ctx
	})

	feature.Assess("force override also blocked when circuit breaker is TRIPPED", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNode2 := testNodes[1]

		event := helpers.NewHealthEvent(testNode2).
			WithErrorCode("79").
			WithMessage("XID error with force override").
			WithForceOverride()
		tempFile, err := helpers.SendHealthEventWithTemplate(testNode2, event)
		require.NoError(t, err)
		defer os.Remove(tempFile)

		t.Log("Verifying force override does NOT bypass circuit breaker TRIPPED state")
		helpers.AssertNodeNeverQuarantined(ctx, t, client, testNode2, false)

		return ctx
	})

	feature.Assess("circuit breaker auto-trips when threshold exceeded and blocks further cordoning", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")

		allNodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err)
		totalNodes := len(allNodes)
		threshold = int(float64(totalNodes)*0.20) + 2
		t.Logf("Total GPU nodes: %d, cordoning %d nodes to exceed 20%% threshold", totalNodes, threshold)

		t.Logf("Rapidly sending events to %d nodes to trigger circuit breaker", threshold)
		cordonedNodes := []string{}
		tempFiles := []string{}

		for i := 0; i < threshold && i < len(testNodes); i++ {
			nodeName := testNodes[i]

			event := helpers.NewHealthEvent(nodeName).
				WithErrorCode("79").
				WithMessage("XID error for circuit breaker test")
			tempFile, err := helpers.SendHealthEventWithTemplate(nodeName, event)
			require.NoError(t, err)
			tempFiles = append(tempFiles, tempFile)

			cordonedNodes = append(cordonedNodes, nodeName)
		}

		for _, f := range tempFiles {
			os.Remove(f)
		}

		t.Logf("Waiting for all %d nodes to be cordoned", len(cordonedNodes))
		for i, nodeName := range cordonedNodes {
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err == nil && node.Spec.Unschedulable {
					if i%5 == 0 {
						t.Logf("Node %d/%d cordoned: %s", i+1, len(cordonedNodes), nodeName)
					}
					return true
				}
				return false
			}, helpers.WaitTimeout, helpers.WaitInterval)
		}
		t.Logf("All %d nodes successfully cordoned", len(cordonedNodes))

		t.Log("Checking if circuit breaker auto-tripped")
		cbState := helpers.GetCircuitBreakerState(ctx, t, c)
		if cbState != "TRIPPED" {
			t.Logf("Circuit breaker state is %s, waiting for auto-trip...", cbState)
			require.Eventually(t, func() bool {
				state := helpers.GetCircuitBreakerState(ctx, t, c)
				t.Logf("Current circuit breaker state: %s", state)
				return state == "TRIPPED"
			}, helpers.WaitTimeout, helpers.WaitInterval)
		}
		t.Log("Circuit breaker successfully auto-tripped")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Resetting circuit breaker to CLOSED before cleanup")
		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")

		t.Log("Cleaning up all test nodes asynchronously")
		helpers.SendHealthyEventsAsync(ctx, t, client, testNodes)

		helpers.TeardownQuarantineTest(ctx, t, c)

		t.Log("Restoring original deployment (circuit breaker threshold/duration)")
		helpers.RestoreFQDeployment(ctx, t, client, originalDeployment)

		t.Log("Restoring original circuit breaker state")
		if originalCBState != "" {
			helpers.SetCircuitBreakerState(ctx, t, c, originalCBState)
		} else {
			helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
