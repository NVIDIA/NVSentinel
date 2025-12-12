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

		completedRebootNode := helpers.WaitForRebootNodeCR(ctx, t, client, selectedNodeName)
		require.NotNil(t, completedRebootNode, "first RebootNode should complete")

		crName := fmt.Sprintf("reboot-%s-third", selectedNodeName)
		_, err = helpers.CreateRebootNodeCR(
			ctx,
			client,
			selectedNodeName,
			crName,
		)
		require.NoError(t, err, "third RebootNode CR creation should succeed after first completed")

		completedRebootNode = helpers.WaitForRebootNodeCR(ctx, t, client, selectedNodeName)
		assert.NotNil(t, completedRebootNode, "third RebootNode should complete")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("failed to create kubernetes client for teardown: %v", err)
			return ctx
		}

		err = helpers.DeleteAllRebootNodeCRs(ctx, t, client)
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
