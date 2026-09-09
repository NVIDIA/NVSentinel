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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

// TestJanitorDuplicateRebootDetection tests that a second RebootNode CR targeting
// the same node while a first is active gets NodeAlreadyUnderMaintenance from the
// reconciler (PR #1678 moved this check from webhook to reconciler).
func TestJanitorDuplicateRebootDetection(t *testing.T) {
	feature := features.New("TestJanitorDuplicateRebootDetection").
		WithLabel("suite", "contention").
		WithLabel("component", "janitor")

	var nodeName string
	const firstCRName = "reboot-contention-first"
	const secondCRName = "reboot-contention-second"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected node: %s", nodeName)

		return ctx
	})

	feature.Assess("Second RebootNode gets NodeAlreadyUnderMaintenance", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateRebootNodeCR(ctx, client, nodeName, firstCRName)
		require.NoError(t, err, "first RebootNode should be admitted")

		// The duplicate-contention logic only fires while CR#1 holds the lock with
		// completionTime==nil. Wait for CR#1 to show SignalSent=True (reboot signal sent,
		// node rebooting) before creating CR#2. Skip if the CSP provider fails fast.
		signalSent, _ := helpers.WaitForCRConditionByName(ctx, t, client, firstCRName, helpers.RebootNodeGVK, "SignalSent", "True")
		if !signalSent {
			t.Skip("CSP provider did not send reboot signal; NodeAlreadyUnderMaintenance contention test requires an active long-running reboot")
		}

		_, err = helpers.CreateRebootNodeCR(ctx, client, nodeName, secondCRName)
		require.NoError(t, err, "second RebootNode should be admitted by webhook (reconciler handles contention)")

		completedCR := helpers.WaitForCRByName(ctx, t, client, secondCRName, helpers.RebootNodeGVK)
		require.NotNil(t, completedCR, "second RebootNode should reach terminal state")

		cond := helpers.GetCRCondition(completedCR, "NodeReady")
		require.NotNil(t, cond, "NodeReady condition should be set")
		assert.Equal(t, "False", cond["status"], "NodeReady should be False for duplicate")
		assert.Equal(t, "NodeAlreadyUnderMaintenance", cond["reason"])
		assert.Contains(t, cond["message"], firstCRName, "message should name the holder CR")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("teardown: failed to create client: %v", err)
			return ctx
		}
		if err := helpers.DeleteAllCRs(ctx, t, client, helpers.RebootNodeGVK); err != nil {
			t.Logf("teardown: failed to delete RebootNode CRs: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestJanitorDuplicateTerminateNodeDetection tests that a second TerminateNode CR
// targeting the same node while a first is active gets NodeAlreadyUnderMaintenance.
func TestJanitorDuplicateTerminateNodeDetection(t *testing.T) {
	feature := features.New("TestJanitorDuplicateTerminateNodeDetection").
		WithLabel("suite", "contention").
		WithLabel("component", "janitor")

	var nodeName string
	const firstCRName = "terminate-contention-first"
	const secondCRName = "terminate-contention-second"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected node: %s", nodeName)

		return ctx
	})

	feature.Assess("Second TerminateNode gets NodeAlreadyUnderMaintenance", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateTerminateNodeCR(ctx, client, nodeName, firstCRName)
		require.NoError(t, err, "first TerminateNode should be admitted")

		// The duplicate-contention logic only fires while CR#1 holds the lock with
		// completionTime==nil. Wait for CR#1 to show SignalSent=True (CSP acknowledged,
		// node termination in progress) before creating CR#2. If CR#1 completes first
		// (gRPC fails fast or no provider configured), skip gracefully.
		signalSent, _ := helpers.WaitForCRConditionByName(ctx, t, client, firstCRName, helpers.TerminateNodeGVK, "SignalSent", "True")
		if !signalSent {
			t.Skip("CSP provider did not send TerminateNode signal; NodeAlreadyUnderMaintenance contention test requires an active long-running termination")
		}

		_, err = helpers.CreateTerminateNodeCR(ctx, client, nodeName, secondCRName)
		require.NoError(t, err, "second TerminateNode should be admitted by webhook (reconciler handles contention)")

		completedCR := helpers.WaitForCRByName(ctx, t, client, secondCRName, helpers.TerminateNodeGVK)
		require.NotNil(t, completedCR, "second TerminateNode should reach terminal state")

		cond := helpers.GetCRCondition(completedCR, "NodeTerminated")
		require.NotNil(t, cond, "NodeTerminated condition should be set")
		assert.Equal(t, "False", cond["status"], "NodeTerminated should be False for duplicate")
		assert.Equal(t, "NodeAlreadyUnderMaintenance", cond["reason"])
		assert.Contains(t, cond["message"], firstCRName, "message should name the holder CR")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("teardown: failed to create client: %v", err)
			return ctx
		}
		if err := helpers.DeleteAllCRs(ctx, t, client, helpers.TerminateNodeGVK); err != nil {
			t.Logf("teardown: failed to delete TerminateNode CRs: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestJanitorDuplicateGPUResetOverlappingGPUs tests that a second GPUReset targeting
// the same node and overlapping GPU UUIDs gets GPUAlreadyUnderMaintenance.
func TestJanitorDuplicateGPUResetOverlappingGPUs(t *testing.T) {
	feature := features.New("TestJanitorDuplicateGPUResetOverlappingGPUs").
		WithLabel("suite", "contention").
		WithLabel("component", "janitor")

	var nodeName string
	const sharedUUID = "GPU-455d8f70-2051-db6c-0430-ffc457bff834"
	const firstCRName = "gpu-reset-contention-first"
	const secondCRName = "gpu-reset-contention-second"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected node: %s", nodeName)

		return ctx
	})

	feature.Assess("Second GPUReset with overlapping UUID gets GPUAlreadyUnderMaintenance", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, firstCRName, sharedUUID)
		require.NoError(t, err, "first GPUReset should be admitted")

		// The duplicate-contention logic only fires while CR#1 holds the lock with
		// completionTime==nil. Wait for CR#1 to show Ready=True (lock acquired, services
		// are being torn down) before creating CR#2. If CR#1 completes first (GPU not
		// present or job fails immediately), skip gracefully.
		ready, _ := helpers.WaitForCRConditionByName(ctx, t, client, firstCRName, helpers.GPUResetGVK, "Ready", "True")
		if !ready {
			t.Skip("GPUReset CR#1 completed before reaching Ready=True; GPUAlreadyUnderMaintenance contention test requires an active long-running reset")
		}

		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, secondCRName, sharedUUID)
		require.NoError(t, err, "second GPUReset should be admitted by webhook (reconciler handles contention)")

		completedCR := helpers.WaitForCRByName(ctx, t, client, secondCRName, helpers.GPUResetGVK)
		require.NotNil(t, completedCR, "second GPUReset should reach terminal state")

		cond := helpers.GetCRCondition(completedCR, "Complete")
		require.NotNil(t, cond, "Complete condition should be set")
		assert.Equal(t, "True", cond["status"], "Complete should be True for terminal failure")
		assert.Equal(t, "GPUAlreadyUnderMaintenance", cond["reason"])
		assert.Contains(t, cond["message"], firstCRName, "message should name the holder CR")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("teardown: failed to create client: %v", err)
			return ctx
		}
		if err := helpers.DeleteAllCRs(ctx, t, client, helpers.GPUResetGVK); err != nil {
			t.Logf("teardown: failed to delete GPUReset CRs: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestJanitorDuplicateGPUResetNonOverlappingGPUs tests that a second GPUReset
// targeting the same node but a different (non-overlapping) GPU UUID is NOT
// rejected with GPUAlreadyUnderMaintenance — it should queue and eventually run.
func TestJanitorDuplicateGPUResetNonOverlappingGPUs(t *testing.T) {
	feature := features.New("TestJanitorDuplicateGPUResetNonOverlappingGPUs").
		WithLabel("suite", "contention").
		WithLabel("component", "janitor")

	var nodeName string
	const uuidA = "GPU-455d8f70-2051-db6c-0430-ffc457bff834"
	const uuidB = "GPU-b0b0b0b0-aaaa-bbbb-cccc-dddddddddddd"
	const firstCRName = "gpu-reset-nonoverlap-first"
	const secondCRName = "gpu-reset-nonoverlap-second"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected node: %s", nodeName)

		return ctx
	})

	feature.Assess("Second GPUReset with non-overlapping UUID is not immediately rejected", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, firstCRName, uuidA)
		require.NoError(t, err, "first GPUReset should be admitted")

		// Wait for CR#1 to hold the lock (Ready=True) before creating CR#2, so that the
		// non-overlap contention check actually runs in the reconciler. If CR#1 completes
		// first the test still exercises the assertion (no GPUAlreadyUnderMaintenance).
		helpers.WaitForCRConditionByName(ctx, t, client, firstCRName, helpers.GPUResetGVK, "Ready", "True")

		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, secondCRName, uuidB)
		require.NoError(t, err, "second GPUReset with different GPU should be admitted")

		// Wait for CR#2 to reach a terminal state (completionTime set). It must queue
		// behind CR#1's lock and then complete normally — not fail with contention.
		completedCR := helpers.WaitForCRByName(ctx, t, client, secondCRName, helpers.GPUResetGVK)
		require.NotNil(t, completedCR, "second GPUReset should reach terminal state")

		cond := helpers.GetCRCondition(completedCR, "Complete")
		require.NotNil(t, cond, "Complete condition should be set on second GPUReset")
		assert.NotEqual(t, "GPUAlreadyUnderMaintenance", cond["reason"],
			"non-overlapping GPUReset should not get GPUAlreadyUnderMaintenance")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("teardown: failed to create client: %v", err)
			return ctx
		}
		if err := helpers.DeleteAllCRs(ctx, t, client, helpers.GPUResetGVK); err != nil {
			t.Logf("teardown: failed to delete GPUReset CRs: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestJanitorWebhookRejectsNonExistentNode tests that the janitor webhook
// rejects RebootNode creation for nodes that don't exist in the cluster.
func TestJanitorWebhookRejectsNonExistentNode(t *testing.T) {
	feature := features.New("TestJanitorWebhookRejectsNonExistentNode").
		WithLabel("suite", "webhook").
		WithLabel("component", "janitor")

	feature.Assess("RebootNode for non-existent node is rejected", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nonExistentNode := "node-that-does-not-exist-12345"
		crName := fmt.Sprintf("reboot-%s", nonExistentNode)
		_, err = helpers.CreateRebootNodeCR(
			ctx,
			client,
			nonExistentNode,
			crName,
		)

		require.Error(t, err, "RebootNode for non-existent node should be rejected")

		statusErr, ok := err.(*apierrors.StatusError)
		require.True(t, ok, "error should be a StatusError")

		assert.True(t,
			apierrors.IsNotFound(err),
			"error should beNotFound, got: %v", statusErr.ErrStatus.Code)

		assert.Contains(t, err.Error(), "not found",
			"error message should mention node not found")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestJanitorRebootWithTLSAuth validates the full TLS + SA token auth chain between
// janitor and janitor-provider. The janitor sends a projected SA token over a TLS
// connection; the provider validates it via TokenReview and processes the reboot.
// If TLS or auth is broken the RebootNode CR will fail to reach SignalSent=True.
func TestJanitorRebootWithTLSAuth(t *testing.T) {
	feature := features.New("TestJanitorRebootWithTLSAuth").
		WithLabel("suite", "tls-auth").
		WithLabel("component", "janitor")

	var selectedNodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err, "failed to get cluster nodes")
		require.True(t, len(nodes) > 0, "no nodes found in cluster")

		selectedNodeName = nodes[0]
		t.Logf("Selected node for TLS auth test: %s", selectedNodeName)

		return ctx
	})

	feature.Assess("RebootNode completes successfully over TLS with auth", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		crName := fmt.Sprintf("reboot-tls-%s", selectedNodeName)
		_, err = helpers.CreateRebootNodeCR(ctx, client, selectedNodeName, crName)
		require.NoError(t, err, "RebootNode CR creation should succeed")

		completedCR := helpers.WaitForCRByName(ctx, t, client, crName, helpers.RebootNodeGVK)
		require.NotNil(t, completedCR, "RebootNode should complete")

		// Verify SignalSent condition is True (proves the gRPC call over TLS+auth succeeded)
		signalSent := helpers.GetCRCondition(completedCR, "SignalSent")
		require.NotNil(t, signalSent, "SignalSent condition should exist")
		assert.Equal(t, "True", signalSent["status"], "SignalSent should be True")

		// Verify NodeReady condition is True (reboot completed successfully)
		nodeReady := helpers.GetCRCondition(completedCR, "NodeReady")
		require.NotNil(t, nodeReady, "NodeReady condition should exist")
		assert.Equal(t, "True", nodeReady["status"], "NodeReady should be True")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("failed to create kubernetes client for teardown: %v", err)
			return ctx
		}

		err = helpers.DeleteAllCRs(ctx, t, client, helpers.RebootNodeGVK)
		if err != nil {
			t.Logf("failed to delete RebootNode CRs: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestJanitorNodeLocking(t *testing.T) {
	feature := features.New("TestJanitorNodeLocking").
		WithLabel("suite", "node-locking").
		WithLabel("component", "janitor")

	feature.Assess("RebootNode and GPUReset for same node run sequentially", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// use a real node for the first RebootNode and GPUReset
		nodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected real node for Janitor node-level locking test: %s", nodeName)

		// Create a RebootNode and GPUReset targeting the same node
		rebootNodeCRName := fmt.Sprintf("reboot-%s", nodeName)
		_, err = helpers.CreateRebootNodeCR(ctx, client, nodeName, rebootNodeCRName)
		require.NoError(t, err, "RebootNode should be created successfully")

		gpuResetCRName := fmt.Sprintf("gpu-reset-%s", nodeName)
		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, gpuResetCRName, "GPU-455d8f70-2051-db6c-0430-ffc457bff834")
		require.NoError(t, err, "GPUReset should be created successfully")
		t.Logf("Created RebootNodes: %s and GPUReset %s", rebootNodeCRName, gpuResetCRName)

		// Wait for the 2 CRs to reach a terminal status
		rebootNodeCR := helpers.WaitForCR(ctx, t, client, nodeName, helpers.RebootNodeGVK)
		gpuResetCR := helpers.WaitForCR(ctx, t, client, nodeName, helpers.GPUResetGVK)

		// Confirm that start and completion times have no overlap for the RebootNode and GPUReset CRs targeting the same
		// node.
		startTimeReboot, completionTimeReboot, err := helpers.GetStartAndCompletionTimes(rebootNodeCR)
		require.NoError(t, err)
		startTimeReset, completionTimeReset, err := helpers.GetStartAndCompletionTimes(gpuResetCR)
		require.NoError(t, err)

		t.Logf("RebootNode startTime: %s completionTime: %s", startTimeReboot.Format(time.RFC3339),
			completionTimeReboot.Format(time.RFC3339))
		t.Logf("GPUReset startTime: %s completionTime: %s", startTimeReset.Format(time.RFC3339),
			completionTimeReset.Format(time.RFC3339))

		// Same-node pair must not overlap (NodeLock serializes them). Strict
		// Before — intervals that merely touch at a boundary still satisfy
		// "did not overlap".
		periodOverlapsOnNode1 := startTimeReboot.Before(*completionTimeReset) && startTimeReset.Before(*completionTimeReboot)
		assert.False(t, periodOverlapsOnNode1, "RebootNode and GPUReset periods should not overlap")

		// Clean up both CRs
		err = helpers.DeleteCR(ctx, t, client, rebootNodeCR, false)
		require.NoError(t, err, "RebootNode should be deleted successfully")
		err = helpers.DeleteCR(ctx, t, client, gpuResetCR, false)
		require.NoError(t, err, "GPUReset should be deleted successfully")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
