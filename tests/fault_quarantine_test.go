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
	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestDontCordonIfEventDoesntMatchCELExpression(t *testing.T) {
	feature := features.New("TestBasicCELMatching").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")
		return newCtx
	})

	feature.Assess("event doesn't match CEL expression", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithCheckName("UnknownCheck").
			WithErrorCode("999")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestManualUncordonBehavior(t *testing.T) {
	feature := features.New("TestManualUncordonBehavior").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(newCtx, t, event)

		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.AssertQuarantineState(newCtx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return newCtx
	})

	feature.Assess("manual uncordon clears quarantine state", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			require.NoError(t, err)

			node.Spec.Unschedulable = false
			return client.Resources().Update(ctx, node)
		})
		assert.NoError(t, err, "failed to uncordon node")

		t.Log("Waiting for state to be updated")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				t.Logf("failed to get node %s: %v", testCtx.NodeName, err)
				return false
			}

			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				return false
			}

			manualUncordon, exists := node.Annotations["quarantinedNodeUncordonedManually"]
			if !exists || manualUncordon != "True" {
				return false
			}

			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" {
					return false
				}
			}

			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Assess("healthy event clears all annotations", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestPreCordonedNodeHandling(t *testing.T) {
	feature := features.New("TestPreCordonedNodeHandling").
		WithLabel("suite", "fault-quarantine-special-modes")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")

		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Manually cordoning and tainting node %s", testCtx.NodeName)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = true
		node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
			Key:    "manual-taint",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		})

		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)
		t.Logf("Node %s pre-cordoned with manual taint", testCtx.NodeName)

		return newCtx
	})

	feature.Assess("FQ adds its taints to pre-cordoned node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		t.Log("Waiting for FQ to add its taint to pre-cordoned node")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				t.Logf("failed to get node: %v", err)
				return false
			}

			hasFQTaint := false
			hasManualTaint := false

			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" {
					hasFQTaint = true
				}
				if taint.Key == "manual-taint" {
					hasManualTaint = true
				}
			}

			t.Logf("Node state: hasFQTaint=%v, hasManualTaint=%v, cordoned=%v, taints=%+v",
				hasFQTaint, hasManualTaint, node.Spec.Unschedulable, node.Spec.Taints)

			if !hasFQTaint {
				t.Log("Waiting for FQ taint to be added")
				return false
			}

			return hasFQTaint && hasManualTaint && node.Spec.Unschedulable
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)
		t.Log("FQ taint successfully added to pre-cordoned node")

		return ctx
	})

	feature.Assess("FQ annotations added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.NotNil(t, node.Annotations)

		_, exists := node.Annotations["quarantineHealthEvent"]
		assert.True(t, exists)

		return ctx
	})

	feature.Assess("FQ clears its taints on healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			hasFQTaint := false
			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" {
					hasFQTaint = true
					break
				}
			}

			_, hasAnnotation := node.Annotations["quarantineHealthEvent"]

			return !hasFQTaint && !hasAnnotation
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		if err == nil {
			node.Spec.Unschedulable = false
			newTaints := []v1.Taint{}
			for _, taint := range node.Spec.Taints {
				if taint.Key != "manual-taint" {
					newTaints = append(newTaints, taint)
				}
			}
			node.Spec.Taints = newTaints
			client.Resources().Update(ctx, node)
		}

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestManagedByNVSentinelLabel(t *testing.T) {
	feature := features.New("TestManagedByNVSentinelLabel").
		WithLabel("suite", "fault-quarantine-special-modes")

	var testNodeIgnored string
	var testNodeProcessed string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		newCtx, testCtx := helpers.SetupQuarantineTest(ctx, t, c, "data/managed-by-nvsentinel-configmap.yaml")

		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeProcessed = testCtx.NodeName

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err)

		startIdx := int(float64(len(nodes)) * 0.50)
		if startIdx >= len(nodes) {
			startIdx = len(nodes) - 1
		}

		for i := startIdx; i < len(nodes); i++ {
			if nodes[i] != testNodeProcessed {
				node, err := helpers.GetNodeByName(ctx, client, nodes[i])
				if err != nil {
					continue
				}
				if !node.Spec.Unschedulable {
					testNodeIgnored = nodes[i]
					break
				}
			}
		}
		require.NotEmpty(t, testNodeIgnored, "failed to find a second uncordoned node different from processed node")

		t.Logf("Using two test nodes: ignored=%s, processed=%s", testNodeIgnored, testNodeProcessed)
		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeIgnored)

		nodeIgnored, err := helpers.GetNodeByName(ctx, client, testNodeIgnored)
		require.NoError(t, err)

		if nodeIgnored.Labels == nil {
			nodeIgnored.Labels = make(map[string]string)
		}
		nodeIgnored.Labels["k8saas.nvidia.com/ManagedByNVSentinel"] = "false"

		err = client.Resources().Update(ctx, nodeIgnored)
		require.NoError(t, err)

		return newCtx
	})

	feature.Assess("node with label=false ignored by FQ", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testNodeIgnored).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testNodeIgnored, true)

		return ctx
	})

	feature.Assess("node without label processed by FQ", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testNodeProcessed).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertQuarantineState(ctx, t, client, testNodeProcessed, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthEvent(ctx, t,
			helpers.NewHealthEvent(testNodeIgnored).
				WithHealthy(true).
				WithFatal(false).
				WithMessage("No Health Failures"))

		helpers.SendHealthEvent(ctx, t,
			helpers.NewHealthEvent(testNodeProcessed).
				WithHealthy(true).
				WithFatal(false).
				WithMessage("No Health Failures"))

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testNodeProcessed)
			if err != nil {
				return false
			}
			if node.Spec.Unschedulable {
				return false
			}
			if node.Annotations != nil {
				if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
					return false
				}
			}
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		nodeIgnored, err := helpers.GetNodeByName(ctx, client, testNodeIgnored)
		if err == nil {
			delete(nodeIgnored.Labels, "k8saas.nvidia.com/ManagedByNVSentinel")
			client.Resources().Update(ctx, nodeIgnored)
		}

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
