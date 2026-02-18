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
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

// TestLogCollectorFailure validates that remediation proceeds even when log-collector fails.
// This is a negative test that uses a cluster-wide KWOK Stage to force log-collector failures.
//
// IMPORTANT: This test applies a KWOK Stage that affects ALL log-collector pods cluster-wide.
// The Stage is cleaned up in teardown, but this test MUST NOT run in parallel with other
// tests that create log-collector jobs (e.g., smoke tests that expect successful uploads).
//
// Test sequence:
// 1. Log-collector job is triggered and fails (via cluster-wide KWOK Stage)
// 2. Remediation succeeds despite log-collector failure (RebootNode CR created)
// 3. Log-collector failure is independent - doesn't set remediation-failed label
func TestLogCollectorFailure(t *testing.T) {
	feature := features.New("TestLogCollectorFailure").
		WithLabel("suite", "smoke")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "logcollector-failure-test"

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/basic-matching-configmap.yaml")
		ctx = helpers.ApplyNodeDrainerConfig(ctx, t, c, "data/nd-all-modes.yaml")

		// Clean up old log-collector jobs from previous tests to avoid confusion
		t.Logf("Cleaning up old log-collector jobs from previous tests")
		helpers.DeleteAllLogCollectorJobs(ctx, t, client)

		// Apply KWOK Stage FIRST to ensure it's active before any pods are created
		helpers.ApplyKwokStageFromFile(ctx, t, client, "data/log-collector-failure-stage.yaml")
		t.Logf("KWOK Stage applied - ALL log-collector pods will fail during this test")

		// Use a KWOK node for negative test
		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected KWOK node for log-collector failure test: %s", nodeName)

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		require.NoError(t, err, "failed to create workloads namespace")

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)

		return ctx
	})

	feature.Assess("Node is cordoned after fatal health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Send fatal health event with supported action (will trigger remediation + log-collector)
		err = helpers.SendHealthEventsToNodes([]string{nodeName}, "data/fatal-health-event-restart-vm.json")
		assert.NoError(t, err, "failed to send health event")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, true)

		return ctx
	})

	feature.Assess("Remediation succeeds despite log-collector failure", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Verify log-collector job failed
		t.Logf("Verifying log-collector job failed on node %s", nodeName)
		helpers.WaitForLogCollectorJobStatus(ctx, t, client, nodeName, "Failed")

		// Verify remediation succeeded - check for RebootNode CR creation and completion
		t.Logf("Verifying RebootNode CR was created and completed for node %s", nodeName)
		_ = helpers.WaitForCR(ctx, t, client, nodeName, helpers.RebootNodeGVK)

		// Verify no remediation-failed label
		t.Logf("Verifying no remediation-failed label on node %s", nodeName)
		helpers.VerifyNodeLabelNotEqual(ctx, t, client, nodeName, statemanager.NVSentinelStateLabelKey, string(statemanager.RemediationFailedLabelValue))

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.DeleteKwokStage(ctx, t, client, "log-collector-failure")

		nodeName := ctx.Value(keyNodeName).(string)
		t.Logf("Cleaning up node %s state", nodeName)
		err = helpers.SendHealthEventsToNodes([]string{nodeName}, "data/healthy-event.json")
		if err != nil {
			t.Logf("Warning: failed to send healthy event during cleanup: %v", err)
		}

		// Wait for node to be uncordoned (best effort - don't fail teardown if it times out)
		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		helpers.RestoreQuarantineConfig(ctx, t, c)
		helpers.RestoreNodeDrainerConfig(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
