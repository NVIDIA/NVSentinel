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
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

func TestFatalHealthEvent(t *testing.T) {
	feature := features.New("TestFatalHealthEventEndToEnd").
		WithLabel("suite", "smoke")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "allowcompletion-test"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/basic-matching-configmap.yaml")
		ctx = helpers.ApplyNodeDrainerConfig(ctx, t, c, "data/nd-all-modes.yaml")

		// Use a real (non-KWOK) node for smoke test to validate actual container execution
		nodeName, err := helpers.GetRealNodeName(ctx, client)
		assert.NoError(t, err, "failed to get real node")
		t.Logf("Selected real node for smoke test: %s", nodeName)

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)

		return ctx
	})

	nodeLabelSequenceObserved := make(chan bool)
	feature.Assess("Can start node label watcher", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)
		t.Logf("Starting label sequence watcher for node %s", nodeName)
		desiredNVSentinelStateNodeLabels := []string{
			string(statemanager.QuarantinedLabelValue),
			string(statemanager.DrainingLabelValue),
			string(statemanager.DrainSucceededLabelValue),
			string(statemanager.RemediatingLabelValue),
			string(statemanager.RemediationSucceededLabelValue),
		}
		err = helpers.StartNodeLabelWatcher(ctx, t, client, nodeName, desiredNVSentinelStateNodeLabels, true, nodeLabelSequenceObserved)
		assert.NoError(t, err, "failed to start node label watcher")

		// Sleep to ensure Kubernetes watch is fully established before triggering state changes
		// This prevents missing early label transitions due to watch startup latency
		t.Log("Waiting for watch to establish connection...")
		time.Sleep(2 * time.Second)
		t.Log("Watch established, ready for health event")

		return ctx
	})

	feature.Assess("Can send fatal health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		err := helpers.SendHealthEventsToNodes([]string{nodeName}, "data/fatal-health-event-restart-vm.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		// Wait for node condition to be updated to unhealthy
		t.Logf("Waiting for node %s condition to become unhealthy", nodeName)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName, "GpuXidError",
			"ErrorCode:79 GPU:0 XID error occurred Recommended Action=RESTART_VM;",
			"GpuXidErrorIsNotHealthy", v1.ConditionTrue)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after cordoning")

		assert.Equal(t, "NVSentinel", node.Labels["cordon-by"])
		assert.Equal(t, "Basic-Match-Rule", node.Labels["cordon-reason"])

		return ctx
	})

	feature.Assess("Drain label is set and pods are not evicted, delete the pod to move the process forward", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesWithLabel(ctx, t, client, []string{nodeName}, statemanager.NVSentinelStateLabelKey, string(statemanager.DrainingLabelValue))

		expectedDrainingNodeEvent := v1.Event{
			Type:   "NodeDraining",
			Reason: "AwaitingPodCompletion",
		}
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, expectedDrainingNodeEvent)

		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		return ctx
	})

	feature.Assess("RebootNode CR is created and completes", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		rebootNode := helpers.WaitForCR(ctx, t, client, nodeName, helpers.RebootNodeGVK)

		err = helpers.DeleteCR(ctx, client, rebootNode)
		assert.NoError(t, err, "failed to delete RebootNode CR")

		return ctx
	})

	feature.Assess("Audit logs are generated for API calls", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		components := []string{"fault-quarantine", "fault-remediation", "janitor"}
		helpers.VerifyAuditLogsExist(ctx, t, client, components)

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

	feature.Assess("Can send healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		err := helpers.SendHealthEventsToNodes([]string{nodeName}, "data/healthy-event.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		// Wait for node condition to be updated to healthy
		t.Logf("Waiting for node %s condition to become healthy", nodeName)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName, "GpuXidError",
			"No Health Failures", "GpuXidErrorIsHealthy", v1.ConditionFalse)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after uncordoning")
		assert.Equal(t, "NVSentinel", node.Labels["uncordon-by"])

		return ctx
	})

	feature.Assess("Observed NVSentinel expected state label changes", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// With state buffering (1000ms window) and out-of-order sorting implemented in
		// StartNodeLabelWatcher, we can now reliably observe rapid state transitions that occur
		// with PostgreSQL LISTEN/NOTIFY. The watcher buffers updates and sorts them by expected
		// sequence to capture all intermediate states even when Kubernetes watch delivers updates
		// out of order or with significant latency.
		select {
		case success := <-nodeLabelSequenceObserved:
			assert.True(t, success)
		default:
			assert.Fail(t, "did not observe expected label changes for nvsentinel-state")
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		helpers.RestoreQuarantineConfig(ctx, t, c)
		helpers.RestoreNodeDrainerConfig(ctx, t, c)

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

		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/basic-matching-configmap.yaml")
		ctx = helpers.ApplyNodeDrainerConfig(ctx, t, c, "data/nd-all-modes.yaml")

		// Use a real (non-KWOK) node for smoke test to validate actual container execution
		nodeName, err := helpers.GetRealNodeName(ctx, client)
		assert.NoError(t, err, "failed to get real node")
		t.Logf("Selected real node for smoke test: %s", nodeName)

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)

		return ctx
	})

	feature.Assess("Can send fatal health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// Delete any existing log-collector jobs for this node before sending event
		helpers.DeleteAllLogCollectorJobs(ctx, t, client)

		err = helpers.SendHealthEventsToNodes([]string{nodeName}, "data/unsupported-health-event.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		// Wait for node condition to be updated to unhealthy
		t.Logf("Waiting for node %s condition to become unhealthy", nodeName)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName, "GpuXidError",
			"ErrorCode:145 GPU:0 XID error occurred Recommended Action=CONTACT_SUPPORT;",
			"GpuXidErrorIsNotHealthy", v1.ConditionTrue)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after cordoning")
		assert.Equal(t, "NVSentinel", node.Labels["cordon-by"])
		assert.Equal(t, "Basic-Match-Rule", node.Labels["cordon-reason"])

		return ctx
	})

	feature.Assess("Drain label is set and pods are not evicted, delete the pod to move the process forward", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesWithLabel(ctx, t, client, []string{nodeName}, statemanager.NVSentinelStateLabelKey, string(statemanager.DrainingLabelValue))

		expectedDrainingNodeEvent := v1.Event{
			Type:   "NodeDraining",
			Reason: "AwaitingPodCompletion",
		}
		helpers.WaitForNodeEvent(ctx, t, client, nodeName, expectedDrainingNodeEvent)

		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		return ctx
	})

	feature.Assess("No log-collector job created for unsupported event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		// For unsupported events, log-collector should NOT be triggered
		// since shouldSkipEvent returns true and we return early before runLogCollector
		helpers.VerifyNoLogCollectorJobExists(ctx, t, client, nodeName)

		return ctx
	})

	feature.Assess("Remediation failed label is set", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesWithLabel(ctx, t, client, []string{nodeName}, statemanager.NVSentinelStateLabelKey, string(statemanager.RemediationFailedLabelValue))

		return ctx
	})

	feature.Assess("Can send healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		err := helpers.SendHealthEventsToNodes([]string{nodeName}, "data/healthy-event.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		// Wait for node condition to be updated to healthy
		t.Logf("Waiting for node %s condition to become healthy", nodeName)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName, "GpuXidError",
			"No Health Failures", "GpuXidErrorIsHealthy", v1.ConditionFalse)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		assert.NoError(t, err, "failed to get node after uncordoning")
		assert.Equal(t, "NVSentinel", node.Labels["uncordon-by"])

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		helpers.RestoreQuarantineConfig(ctx, t, c)
		helpers.RestoreNodeDrainerConfig(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
