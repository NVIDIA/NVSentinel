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

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

// TestRemediationControllerBasicFlow tests the RemediationController's basic flow.
func TestRemediationControllerBasicFlow(t *testing.T) {
	feature := features.New("TestRemediationControllerBasicFlow").
		WithLabel("suite", "remediation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		// Clean up existing resources
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("RemediationController creates RebootNode CR and transitions to Remediated", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a fatal event that requires remediation
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithMessage("XID error occurred").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)

		// Wait for event to progress through phases
		t.Log("Waiting for Quarantined phase...")
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// DrainController may set Draining/Drained or skip if no pods
		t.Log("Waiting for Drained phase...")
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDrained)

		// Wait for RemediationController to process
		t.Log("Waiting for Remediated phase...")
		finalEvent := helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseRemediated)

		// Verify Remediated condition is set
		helpers.AssertRemediatedCondition(t, finalEvent)

		// Verify RebootNode CR was created
		rebootNode := helpers.WaitForRebootNodeCR(ctx, t, client, nodeName)
		t.Logf("RebootNode CR created and completed: %s", rebootNode.GetName())

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
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestMultipleRemediationsOnSameNode tests that multiple remediation CRs can be created for the same node.
func TestMultipleRemediationsOnSameNode(t *testing.T) {
	feature := features.New("TestMultipleRemediationsOnSameNode").
		WithLabel("suite", "remediation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("Second remediation succeeds after first completes", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// --- First remediation cycle ---
		t.Log("=== First remediation cycle ===")

		event1 := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created1 := helpers.CreateHealthEventCRD(ctx, t, client, event1)
		t.Logf("Created first HealthEvent: %s", created1.Name)

		// Wait for remediation to complete
		helpers.WaitForHealthEventPhase(ctx, t, client, created1.Name, nvsentinelv1alpha1.PhaseRemediated)

		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, nodeName)
		t.Logf("First RebootNode CR completed: %s", cr1.GetName())

		// Send healthy event to resolve
		helpers.SendHealthyEventViaCRD(ctx, t, client, nodeName)

		// Wait for first event to be resolved
		helpers.WaitForHealthEventPhase(ctx, t, client, created1.Name, nvsentinelv1alpha1.PhaseResolved)

		// Uncordon node for next cycle
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		if node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			err = client.Resources().Update(ctx, node)
			require.NoError(t, err)
		}

		// --- Second remediation cycle ---
		t.Log("=== Second remediation cycle ===")

		event2 := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuMemoryError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("31").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created2 := helpers.CreateHealthEventCRD(ctx, t, client, event2)
		t.Logf("Created second HealthEvent: %s", created2.Name)

		// Wait for second remediation
		helpers.WaitForHealthEventPhase(ctx, t, client, created2.Name, nvsentinelv1alpha1.PhaseRemediated)

		// Verify we now have 2 completed RebootNode CRs
		crList, err := helpers.GetRebootNodeCRsForNode(ctx, client, nodeName)
		require.NoError(t, err)
		assert.Len(t, crList, 2, "should have 2 completed RebootNode CRs")

		t.Logf("Successfully created %d RebootNode CRs for node %s", len(crList), nodeName)

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
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestContactSupportDoesNotTriggerRemediation tests that CONTACT_SUPPORT events skip remediation.
func TestContactSupportDoesNotTriggerRemediation(t *testing.T) {
	feature := features.New("TestContactSupportDoesNotTriggerRemediation").
		WithLabel("suite", "remediation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("CONTACT_SUPPORT event does not create RebootNode CR", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create an event with CONTACT_SUPPORT action (no automatic remediation)
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("145"). // Unsupported XID
			WithRecommendedAction(nvsentinelv1alpha1.ActionContactSupport).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent with CONTACT_SUPPORT: %s", created.Name)

		// Wait for quarantine and drain
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDrained)

		// Verify NO RebootNode CR is created (CONTACT_SUPPORT = manual intervention required)
		helpers.WaitForNoRebootNodeCR(ctx, t, client, nodeName)
		t.Log("Verified no RebootNode CR created for CONTACT_SUPPORT event")

		// Event should NOT reach Remediated phase
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseRemediated)

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
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestFullPhaseSequenceToResolved tests the complete lifecycle from New to Resolved.
func TestFullPhaseSequenceToResolved(t *testing.T) {
	feature := features.New("TestFullPhaseSequenceToResolved").
		WithLabel("suite", "remediation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		return ctx
	})

	feature.Assess("HealthEvent progresses through full lifecycle", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create fatal event
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent: %s", created.Name)

		// Define full expected phase sequence
		sequence := helpers.ExpectedPhaseSequence{
			nvsentinelv1alpha1.PhaseQuarantined,
			nvsentinelv1alpha1.PhaseDrained,
			nvsentinelv1alpha1.PhaseRemediated,
		}

		// Wait for sequence up to Remediated
		helpers.WaitForHealthEventPhaseSequence(ctx, t, client, created.Name, sequence)
		t.Log("Reached Remediated phase")

		// Send healthy event to trigger resolution
		helpers.SendHealthyEventViaCRD(ctx, t, client, nodeName)

		// Wait for Resolved phase
		finalEvent := helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseResolved)

		// Verify ResolvedAt timestamp is set
		helpers.AssertResolvedAtSet(t, finalEvent)

		t.Log("Successfully verified full phase sequence: New → Quarantined → Drained → Remediated → Resolved")

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
		helpers.DeleteAllRebootNodeCRs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
