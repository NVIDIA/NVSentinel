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

// TestJanitorWebhookRejectsDuplicateReboots tests that the janitor webhook
// correctly rejects attempts to create multiple RebootNode CRs for the same node
// when an active reboot is already in progress.
func TestJanitorWebhookRejectsDuplicateReboots(t *testing.T) {
	// TODO: fix flake, the test is known to have a high failure rate and it is unclear why
	// skipping this test till we can figure out the root cause
	t.Skip()

	feature := features.New("TestJanitorWebhookRejectsDuplicateReboots").
		WithLabel("suite", "webhook").
		WithLabel("component", "janitor")

	var selectedNodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err, "failed to get cluster nodes")
		require.True(t, len(nodes) > 0, "no nodes found in cluster")

		selectedNodeName = nodes[len(nodes)-1]
		t.Logf("Selected node for webhook test: %s", selectedNodeName)

		return ctx
	})

	feature.Assess("First RebootNode CR creation succeeds", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		crName := fmt.Sprintf("reboot-%s-first", selectedNodeName)
		_, err = helpers.CreateRebootNodeCR(
			ctx,
			client,
			selectedNodeName,
			crName,
		)
		require.NoError(t, err, "first RebootNode CR creation should succeed")

		return ctx
	})

	feature.Assess("Second RebootNode CR creation is rejected by webhook", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		crName := fmt.Sprintf("reboot-%s-second", selectedNodeName)
		_, err = helpers.CreateRebootNodeCR(
			ctx,
			client,
			selectedNodeName,
			crName,
		)

		require.Error(t, err, "second RebootNode CR creation should be rejected")

		statusErr, ok := err.(*apierrors.StatusError)
		require.True(t, ok, "error should be a StatusError")

		assert.True(t,
			apierrors.IsForbidden(err) || apierrors.IsInvalid(err),
			"error should be Forbidden or Invalid, got: %v", statusErr.ErrStatus.Code)

		assert.Contains(t, err.Error(), "already has an active",
			"error message should mention active reboot")

		return ctx
	})

	feature.Assess("After first reboot completes, new reboot can be created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		completedRebootNode := helpers.WaitForCR(ctx, t, client, selectedNodeName, helpers.RebootNodeGVK)
		require.NotNil(t, completedRebootNode, "first RebootNode should complete")

		crName := fmt.Sprintf("reboot-%s-third", selectedNodeName)
		_, err = helpers.CreateRebootNodeCR(
			ctx,
			client,
			selectedNodeName,
			crName,
		)
		require.NoError(t, err, "third RebootNode CR creation should succeed after first completed")

		completedRebootNode = helpers.WaitForCR(ctx, t, client, selectedNodeName, helpers.RebootNodeGVK)
		assert.NotNil(t, completedRebootNode, "third RebootNode should complete")

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

		// use a KWOK node for the second RebootNode
		nodes, err := helpers.GetAllNodesNames(ctx, client)
		require.NoError(t, err, "failed to get cluster nodes")
		require.True(t, len(nodes) > 0, "no nodes found in cluster")
		kwokNodeName := nodes[len(nodes)-1]

		// Create a RebootNode and GPUReset targeting the same node and a second RebootNode targeting a different node
		rebootNodeCRName := fmt.Sprintf("reboot-%s", nodeName)
		_, err = helpers.CreateRebootNodeCR(ctx, client, nodeName, rebootNodeCRName)
		require.NoError(t, err, "RebootNode should be created successfully")

		rebootNodeCRName2 := fmt.Sprintf("reboot-%s", kwokNodeName)
		_, err = helpers.CreateRebootNodeCR(ctx, client, kwokNodeName, rebootNodeCRName2)
		require.NoError(t, err, "RebootNode should be created successfully")

		gpuResetCRName := fmt.Sprintf("gpu-reset-%s", nodeName)
		_, err = helpers.CreateGPUResetCR(ctx, client, nodeName, gpuResetCRName, "GPU-455d8f70-2051-db6c-0430-ffc457bff834")
		require.NoError(t, err, "GPUReset should be created successfully")
		t.Logf("Created RebootNodes: %s, %s and GPUReset %s", rebootNodeCRName, rebootNodeCRName2, gpuResetCRName)

		// Wait for all 3 CRs to reach a terminal status
		rebootNodeCR := helpers.WaitForCR(ctx, t, client, nodeName, helpers.RebootNodeGVK)
		rebootNodeCR2 := helpers.WaitForCR(ctx, t, client, kwokNodeName, helpers.RebootNodeGVK)
		gpuResetCR := helpers.WaitForCR(ctx, t, client, nodeName, helpers.GPUResetGVK)

		// Confirm that start and completion times have no overlap for the RebootNode and GPUReset CRs targeting the same
		// node. The 2 RebootNodes on different nodes should overlap.
		startTimeReboot, completionTimeReboot, err := helpers.GetStartAndCompletionTimes(rebootNodeCR)
		require.NoError(t, err)
		startTimeReboot2, completionTimeReboot2, err := helpers.GetStartAndCompletionTimes(rebootNodeCR2)
		require.NoError(t, err)
		startTimeReset, completionTimeReset, err := helpers.GetStartAndCompletionTimes(gpuResetCR)
		require.NoError(t, err)

		t.Logf("RebootNode startTime: %s completionTime: %s", startTimeReboot.Format(time.RFC3339),
			completionTimeReboot.Format(time.RFC3339))
		t.Logf("RebootNode startTime: %s completionTime: %s", startTimeReboot2.Format(time.RFC3339),
			completionTimeReboot2.Format(time.RFC3339))
		t.Logf("GPUReset startTime: %s completionTime: %s", startTimeReset.Format(time.RFC3339),
			completionTimeReset.Format(time.RFC3339))

		periodOverlapsOnNode1 := startTimeReboot.Before(*completionTimeReset) && startTimeReset.Before(*completionTimeReboot)
		periodOverlapsOnNode1And2 := startTimeReboot.Before(*completionTimeReboot2) && startTimeReboot2.Before(*completionTimeReboot)
		assert.False(t, periodOverlapsOnNode1, "RebootNode and GPUReset periods should not overlap")
		assert.True(t, periodOverlapsOnNode1And2, "RebootNode periods on different nodes should overlap")

		// Clean up both CRs
		err = helpers.DeleteCR(ctx, client, rebootNodeCR)
		require.NoError(t, err, "RebootNode should be deleted successfully")
		err = helpers.DeleteCR(ctx, client, rebootNodeCR2)
		require.NoError(t, err, "RebootNode should be deleted successfully")
		err = helpers.DeleteCR(ctx, client, gpuResetCR)
		require.NoError(t, err, "GPUReset should be deleted successfully")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
