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

	"github.com/stretchr/testify/assert"
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

		helpers.EnsureDryRunDisabled(ctx, t, client)

		originalDeployment = helpers.SetCircuitBreakerThreshold(ctx, t, client, 20, "10m")

		originalCBState = helpers.GetCircuitBreakerState(ctx, t, c)
		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err)

		startIdx := int(float64(len(nodes)) * 0.50)
		if startIdx >= len(nodes) {
			startIdx = len(nodes) - 1
		}
		testNodes = nodes[startIdx:]
		t.Logf("Using %d test nodes for CB tests", len(testNodes))

		return newCtx
	})

	feature.Assess("manually TRIPPED CB blocks normal cordoning", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SetCircuitBreakerState(ctx, t, c, "TRIPPED")

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		tempFile, err := helpers.SendHealthEventWithTemplate(testCtx.NodeName, event)
		require.NoError(t, err)
		defer os.Remove(tempFile)

		t.Log("Verifying node NOT cordoned when CB is manually TRIPPED")
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "node should not be cordoned when CB is TRIPPED")

		return ctx
	})

	feature.Assess("force override also blocked when CB is TRIPPED", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		if len(testNodes) < 2 {
			t.Skip("Need at least 2 nodes for this test")
		}
		testNode2 := testNodes[1]

		event := helpers.NewHealthEvent(testNode2).
			WithErrorCode("79").
			WithMessage("XID error with force override").
			WithForceOverride()
		tempFile, err := helpers.SendHealthEventWithTemplate(testNode2, event)
		require.NoError(t, err)
		defer os.Remove(tempFile)

		t.Log("Verifying force override does NOT bypass CB TRIPPED state")
		node, err := helpers.GetNodeByName(ctx, client, testNode2)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "force override should not bypass CB TRIPPED state")

		return ctx
	})

	feature.Assess("CB auto-trips when threshold exceeded and blocks further cordoning", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")

		allNodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err)
		totalNodes := len(allNodes)
		threshold = int(float64(totalNodes)*0.20) + 1
		t.Logf("Total GPU nodes: %d, cordoning %d nodes to exceed 20%% threshold", totalNodes, threshold)

		if len(testNodes) < threshold+1 {
			t.Skipf("Need at least %d nodes for auto-trip test, have %d", threshold+1, len(testNodes))
		}

		t.Logf("Rapidly sending events to %d nodes to trigger CB", threshold)
		cordonedNodes := []string{}
		tempFiles := []string{}

		for i := 0; i < threshold && i < len(testNodes); i++ {
			nodeName := testNodes[i]

			event := helpers.NewHealthEvent(nodeName).
				WithErrorCode("79").
				WithMessage("XID error for CB test")
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

		t.Log("Checking if CB auto-tripped")
		cbState := helpers.GetCircuitBreakerState(ctx, t, c)
		if cbState != "TRIPPED" {
			t.Logf("CB state is %s, waiting for auto-trip...", cbState)
			require.Eventually(t, func() bool {
				state := helpers.GetCircuitBreakerState(ctx, t, c)
				t.Logf("Current CB state: %s", state)
				return state == "TRIPPED"
			}, helpers.WaitTimeout, helpers.WaitInterval)
		}
		t.Log("CB successfully auto-tripped")

		if len(testNodes) <= threshold {
			t.Skip("No additional nodes available to test blocking")
		}

		testNodeBlocked := testNodes[threshold]
		t.Logf("Attempting to cordon node %s (should be blocked by auto-tripped CB)", testNodeBlocked)
		event := helpers.NewHealthEvent(testNodeBlocked).
			WithErrorCode("79").
			WithMessage("Should be blocked")
		tempFile, err := helpers.SendHealthEventWithTemplate(testNodeBlocked, event)
		require.NoError(t, err)
		defer os.Remove(tempFile)

		node, err := helpers.GetNodeByName(ctx, client, testNodeBlocked)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "node should not be cordoned after CB auto-tripped")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Resetting CB to CLOSED before cleanup")
		helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")

		t.Log("Cleaning up all test nodes asynchronously")
		helpers.SendHealthyEventsAsync(ctx, t, client, testNodes)

		helpers.TeardownQuarantineTest(ctx, t, c)

		t.Log("Restoring original deployment (CB threshold/duration)")
		helpers.RestoreFQDeployment(ctx, t, client, originalDeployment)

		t.Log("Restoring original CB state")
		if originalCBState != "" {
			helpers.SetCircuitBreakerState(ctx, t, c, originalCBState)
		} else {
			helpers.SetCircuitBreakerState(ctx, t, c, "CLOSED")
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
