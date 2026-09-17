//go:build amd64_group
// +build amd64_group

// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

	"tests/helpers"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestMaintenanceRequestLifecycle covers the ADR-051 contract end to end:
// creating a MaintenanceRequest prepares the node, and deleting it releases the
// node again.
//
// It is also the only test that exercises the platform-connector token the
// lifecycle-manager chart projects. The MR names a node the publisher is not
// running on, and platform-connector pins a caller to its own node unless that
// caller is on the cross-node allowlist AND presents a token minted for the
// configured audience. So the cordon in the second step cannot happen unless
// the flag, the projected volume and the derived allowlist entry are all
// correct: a missing token shows up here as an event that is silently rejected
// and a node that never cordons.
func TestMaintenanceRequestLifecycle(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	mrName := "e2e-mr-" + suffix
	// Unique per run so the assertion below cannot pass on a health event some
	// other test left in the node's quarantine annotation.
	checkName := "E2EMaintenanceRequest" + suffix

	feature := features.New("TestMaintenanceRequestLifecycle").
		WithLabel("suite", "maintenance-request")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/maintenance-request-configmap.yaml")

		publisherNode, err := helpers.NodeRunningLifecycleManager(ctx, client)
		require.NoError(t, err, "failed to locate the lifecycle-manager pod")

		targetNode := helpers.SelectMaintenanceTargetNode(ctx, t, client, publisherNode)

		return context.WithValue(ctx, keyNodeName, targetNode)
	})

	feature.Assess("controller accepts the MaintenanceRequest and emits the opening event",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			targetNode := ctx.Value(keyNodeName).(string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			_, err = helpers.CreateMaintenanceRequest(ctx, client, mrName, targetNode, checkName)
			require.NoError(t, err, "MaintenanceRequest should be admitted by the validating webhook")

			mr := helpers.WaitForMaintenanceRequestEmitted(ctx, t, client, mrName)

			require.Contains(t, mr.GetFinalizers(), helpers.MaintenanceRequestFinalizer,
				"controller should hold the MR with its finalizer so the clearing event cannot be skipped by a delete")

			return ctx
		})

	feature.Assess("fault-quarantine cordons the target node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			targetNode := ctx.Value(keyNodeName).(string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			// The shipped ruleset matches on the maintenanceRequestName metadata
			// key, which the mutating webhook sets. Asserting the annotation
			// carries this run's checkName proves the cordon came from our event
			// rather than from residue or another suite.
			helpers.AssertQuarantineState(ctx, t, client, targetNode, helpers.QuarantineAssertion{
				ExpectCordoned: true,
				AnnotationChecks: []helpers.AnnotationCheck{
					{Key: helpers.QuarantineHealthEventAnnotationKey, Pattern: checkName, ShouldExist: true},
					{Key: helpers.QuarantineHealthEventAnnotationKey, Pattern: mrName, ShouldExist: true},
				},
			})

			return ctx
		})

	feature.Assess("deleting the MaintenanceRequest clears the fault and uncordons the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			targetNode := ctx.Value(keyNodeName).(string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			// Waiting for the object to leave the API is the assertion: the
			// controller only drops its finalizer after the clearing event has
			// been published, so a rejected publish leaves the MR Terminating
			// here rather than failing somewhere quieter.
			helpers.DeleteMaintenanceRequestIfPresent(ctx, t, client, mrName)

			helpers.WaitForNodesCordonState(ctx, t, client, []string{targetNode}, false)

			helpers.AssertQuarantineState(ctx, t, client, targetNode, helpers.QuarantineAssertion{
				ExpectCordoned: false,
				AnnotationChecks: []helpers.AnnotationCheck{
					{Key: helpers.QuarantineHealthEventAnnotationKey, Pattern: checkName, ShouldExist: false},
				},
			})

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Idempotent: the delete assessment normally removed it already, but an
		// earlier failure would leave the MR holding the node cordoned for
		// every test that runs after this one.
		helpers.DeleteMaintenanceRequestIfPresent(ctx, t, client, mrName)

		if nodeNameVal := ctx.Value(keyNodeName); nodeNameVal != nil {
			nodeName := nodeNameVal.(string)

			helpers.SendHealthyEvent(ctx, t, nodeName)

			if err := helpers.SetNodeCordon(ctx, client, nodeName, false); err != nil {
				t.Logf("failed to uncordon node %s during teardown: %v", nodeName, err)
			}
		}

		helpers.RestoreQuarantineConfig(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
