//go:build arm64_group
// +build arm64_group

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
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

func TestFatalHealthEvent(t *testing.T) {
	feature := features.New("TestFatalHealthEventEndToEnd").
		WithLabel("suite", "smoke")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "allowcompletion-test"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Use a real (non-KWOK) node for smoke test to validate actual container execution
		nodeName, err := helpers.GetRealNodeName(ctx, client)
		assert.NoError(t, err, "failed to get real node")
		t.Logf("Selected real node for smoke test: %s", nodeName)

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		// Clean up any existing HealthEvents for this node
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)

		return ctx
	})

	feature.Assess("Can create fatal HealthEvent CRD", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Create a fatal HealthEvent CRD (replaces HTTP POST to health-event endpoint)
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-smoke-test").
			WithCheckName("GpuXidError").
			WithComponentClass("GPU").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithEntity("GPU", "0").
			WithMessage("XID error occurred").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent CRD: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)
		return ctx
	})

	feature.Assess("HealthEvent transitions to Quarantined phase", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Wait for QuarantineController to process the event
		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseQuarantined)
		t.Logf("HealthEvent %s reached phase: %s", eventName, event.Status.Phase)

		// Verify NodeQuarantined condition is set
		helpers.AssertNodeQuarantinedCondition(t, event)

		return ctx
	})

	feature.Assess("Node is cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after cordoning")

		// Verify node is unschedulable
		assert.True(t, node.Spec.Unschedulable, "node should be unschedulable (cordoned)")

		return ctx
	})

	feature.Assess("HealthEvent transitions to Draining then Drained", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Wait for DrainController to start draining
		helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseDraining)
		t.Logf("HealthEvent %s is in Draining phase", eventName)

		// Manually drain pods to simulate completion (for test with AllowCompletion mode)
		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		// Wait for drain to complete
		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseDrained)
		t.Logf("HealthEvent %s reached phase: %s", eventName, event.Status.Phase)

		// Verify PodsDrained condition is set
		helpers.AssertPodsDrainedCondition(t, event)

		return ctx
	})

	feature.Assess("HealthEvent transitions to Remediated", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Wait for RemediationController to process the event
		// This will create a RebootNode CR and wait for it to complete
		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseRemediated)
		t.Logf("HealthEvent %s reached phase: %s", eventName, event.Status.Phase)

		// Verify Remediated condition is set
		helpers.AssertRemediatedCondition(t, event)

		// Clean up the RebootNode CR created by remediation
		rebootNode := helpers.WaitForRebootNodeCR(ctx, t, client, nodeName)
		err = helpers.DeleteRebootNodeCR(ctx, client, rebootNode)
		assert.NoError(t, err, "failed to delete RebootNode CR")

		return ctx
	})

	feature.Assess("Log-collector job completes successfully", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Verify log-collector job completed successfully on real node
		t.Logf("Waiting for log-collector job to complete on node %s", nodeName)
		job := helpers.WaitForLogCollectorJobStatus(ctx, t, client, nodeName, "Complete")

		// Verify log files were uploaded to file server
		t.Logf("Verifying log files were uploaded to file server for node %s", nodeName)
		helpers.VerifyLogFilesUploaded(ctx, t, client, job)

		return ctx
	})

	feature.Assess("Create healthy HealthEvent to resolve", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Create a healthy event (replaces HTTP POST to health-event endpoint)
		helpers.SendHealthyEventViaCRD(ctx, t, client, nodeName)
		t.Logf("Created healthy HealthEvent for node %s", nodeName)

		return ctx
	})

	feature.Assess("Original HealthEvent transitions to Resolved", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Wait for the original event to be resolved
		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseResolved)
		t.Logf("HealthEvent %s reached phase: %s", eventName, event.Status.Phase)

		// Verify ResolvedAt timestamp is set
		helpers.AssertResolvedAtSet(t, event)

		return ctx
	})

	feature.Assess("Node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after uncordoning")

		// Verify node is schedulable again
		assert.False(t, node.Spec.Unschedulable, "node should be schedulable (uncordoned)")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		// Clean up HealthEvent CRDs
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestFatalUnsupportedHealthEvent(t *testing.T) {
	feature := features.New("TestFatalUnsupportedHealthEventEndToEnd").
		WithLabel("suite", "smoke")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "allowcompletion-test"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Use a real (non-KWOK) node for smoke test to validate actual container execution
		nodeName, err := helpers.GetRealNodeName(ctx, client)
		assert.NoError(t, err, "failed to get real node")
		t.Logf("Selected real node for smoke test: %s", nodeName)

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		// Clean up any existing resources
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)
		helpers.DeleteAllLogCollectorJobs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)

		return ctx
	})

	feature.Assess("Can create unsupported fatal HealthEvent CRD", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Create an unsupported fatal HealthEvent (XID 145 = CONTACT_SUPPORT)
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-smoke-test").
			WithCheckName("GpuXidError").
			WithComponentClass("GPU").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("145").
			WithEntity("GPU", "0").
			WithMessage("XID error occurred").
			WithRecommendedAction(nvsentinelv1alpha1.ActionContactSupport).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent CRD: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)
		return ctx
	})

	feature.Assess("HealthEvent transitions to Quarantined phase", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseQuarantined)
		t.Logf("HealthEvent %s reached phase: %s", eventName, event.Status.Phase)

		return ctx
	})

	feature.Assess("Node is cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		return ctx
	})

	feature.Assess("HealthEvent transitions through Draining to Drained", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Wait for DrainController to start draining
		helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseDraining)

		// Manually drain pods
		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		// Wait for drain to complete
		helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseDrained)

		return ctx
	})

	feature.Assess("No log-collector job created for unsupported event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// For unsupported events (CONTACT_SUPPORT), log-collector should NOT be triggered
		helpers.VerifyNoLogCollectorJobExists(ctx, t, client, nodeName)

		return ctx
	})

	feature.Assess("HealthEvent stays in Drained (remediation skipped for CONTACT_SUPPORT)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// For CONTACT_SUPPORT events, the RemediationController should not progress to Remediated
		// because no automatic remediation is possible
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseRemediated)

		return ctx
	})

	feature.Assess("Create healthy HealthEvent to resolve", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.SendHealthyEventViaCRD(ctx, t, client, nodeName)
		t.Logf("Created healthy HealthEvent for node %s", nodeName)

		return ctx
	})

	feature.Assess("Node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after uncordoning")

		assert.False(t, node.Spec.Unschedulable, "node should be schedulable (uncordoned)")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		// Clean up HealthEvent CRDs
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
