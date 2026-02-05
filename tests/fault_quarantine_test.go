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
	"testing"

	"tests/helpers"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestNonFatalEventDoesNotTriggerQuarantine tests that non-fatal events don't trigger quarantine.
func TestNonFatalEventDoesNotTriggerQuarantine(t *testing.T) {
	feature := features.New("TestNonFatalEventDoesNotTriggerQuarantine").
		WithLabel("suite", "quarantine-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		// Clean up any existing HealthEvents
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("Non-fatal event stays in New phase, node not cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a non-fatal event (isFatal=false)
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuWarning").
			WithComponentClass("GPU").
			WithFatal(false). // Non-fatal
			WithHealthy(false).
			WithErrorCodes("999").
			WithMessage("Non-fatal warning").
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created non-fatal HealthEvent: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)

		// Assert that the event never reaches Quarantined phase
		// QuarantineController should skip non-fatal events
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify node was NOT cordoned
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "node should NOT be cordoned for non-fatal event")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestHealthyEventDoesNotTriggerQuarantine tests that healthy events don't trigger quarantine.
func TestHealthyEventDoesNotTriggerQuarantine(t *testing.T) {
	feature := features.New("TestHealthyEventDoesNotTriggerQuarantine").
		WithLabel("suite", "quarantine-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("Healthy event stays in New phase, node not cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a healthy event (isHealthy=true)
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithComponentClass("GPU").
			WithFatal(true).
			WithHealthy(true). // Healthy
			WithMessage("No errors detected").
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created healthy HealthEvent: %s", created.Name)

		// Assert that the event never reaches Quarantined phase
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify node was NOT cordoned
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "node should NOT be cordoned for healthy event")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestPreCordonedNodeHandling tests that QuarantineController handles pre-cordoned nodes correctly.
func TestPreCordonedNodeHandling(t *testing.T) {
	feature := features.New("TestPreCordonedNodeHandling").
		WithLabel("suite", "quarantine-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		// Pre-cordon the node manually with a taint
		t.Logf("Manually cordoning and tainting node %s", nodeName)
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = true
		node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
			Key:    "manual-taint",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		})

		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)
		t.Logf("Node %s pre-cordoned with manual taint", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("QuarantineController processes event on pre-cordoned node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a fatal event
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithMessage("XID error occurred").
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)

		// Wait for QuarantineController to process (should still transition to Quarantined)
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify the manual taint is preserved
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)

		hasManualTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "manual-taint" {
				hasManualTaint = true
				break
			}
		}
		assert.True(t, hasManualTaint, "manual taint should be preserved")
		assert.True(t, node.Spec.Unschedulable, "node should remain cordoned")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)

		// Clean up the node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
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

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestQuarantineSkipOverride tests that quarantine can be skipped via override.
func TestQuarantineSkipOverride(t *testing.T) {
	feature := features.New("TestQuarantineSkipOverride").
		WithLabel("suite", "quarantine-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("Event with skip quarantine override doesn't cordon node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a fatal event with skip quarantine override
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithMessage("XID error occurred").
			WithSkipQuarantine(true). // Skip quarantine
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent with skip quarantine: %s", created.Name)

		// Assert that the event never reaches Quarantined phase
		// QuarantineController should respect the skip override
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify node was NOT cordoned
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable, "node should NOT be cordoned when quarantine is skipped")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestMultipleEventsOnSameNode tests handling of multiple events on the same node.
func TestMultipleEventsOnSameNode(t *testing.T) {
	feature := features.New("TestMultipleEventsOnSameNode").
		WithLabel("suite", "quarantine-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("Second event on already-quarantined node processes correctly", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create first fatal event
		event1 := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithMessage("First XID error").
			Build()

		created1 := helpers.CreateHealthEventCRD(ctx, t, client, event1)
		t.Logf("Created first HealthEvent: %s", created1.Name)

		// Wait for first event to quarantine the node
		helpers.WaitForHealthEventPhase(ctx, t, client, created1.Name, nvsentinelv1alpha1.PhaseQuarantined)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		// Create second fatal event on the same node
		event2 := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuMemoryError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("31").
			WithMessage("Second memory error").
			Build()

		created2 := helpers.CreateHealthEventCRD(ctx, t, client, event2)
		t.Logf("Created second HealthEvent: %s", created2.Name)

		// Second event should also transition to Quarantined (node already cordoned)
		helpers.WaitForHealthEventPhase(ctx, t, client, created2.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify node is still cordoned
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		assert.True(t, node.Spec.Unschedulable, "node should remain cordoned")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)

		// Uncordon node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
