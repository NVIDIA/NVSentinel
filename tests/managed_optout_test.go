//go:build arm64_group
// +build arm64_group

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

// TestExtRRManagedOptOutSuppressesFaultQuarantine proves that fault-quarantine's
// skipNodeLabels gate blocks quarantine actions on nodes opted out via ExtRR.
//
// Flow:
//
//	Baseline: unhealthy event quarantines the node (proves FQ works)
//	ExtRR created -> janitor stamps managed=false -> same event is silently dropped
//	ExtRR Complete=True -> managed label cleared -> event quarantines again
func TestExtRRManagedOptOut_SkipLabelApplied_SuppressesFaultQuarantine(t *testing.T) {
	feature := features.New("ExtRR managed=false suppresses fault-quarantine").
		WithLabel("suite", "managed-optout").
		WithLabel("component", "fault-quarantine")

	const crName = "extrr-fq-optout"

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("baseline: unhealthy event quarantines node when managed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: true},
			},
		})
		t.Log("baseline quarantine confirmed")

		t.Log("recovering node with healthy event")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return !node.Spec.Unschedulable
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "node should be uncordoned after healthy event")

		return ctx
	})

	feature.Assess("ExtRR opt-out suppresses fault-quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateExtRRCR(ctx, client, crName, testCtx.NodeName, "fq-optout")
		require.NoError(t, err)

		helpers.WaitForExtRRCondition(ctx, t, client, crName, "NVSentinelOwnershipReleased", "True")

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Labels[managedLabelKey] == "false"
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "node must carry managed=false after ExtRR apply")

		t.Logf("sending unhealthy event to opted-out node %s", testCtx.NodeName)
		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testCtx.NodeName, true)
		t.Log("fault-quarantine correctly skipped the opted-out node")

		return ctx
	})

	feature.Assess("completing ExtRR restores fault-quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
			"True", "RemediationSucceeded", "node returned to service"))
		helpers.WaitForNodeReleaseStateCleared(ctx, t, client, testCtx.NodeName)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		_, hasLabel := node.Labels[managedLabelKey]
		assert.False(t, hasLabel, "managed label must be gone after ExtRR completion")

		t.Logf("re-sending unhealthy event to re-managed node %s", testCtx.NodeName)
		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: true},
			},
		})
		t.Log("fault-quarantine resumed after ExtRR completion")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)

		if testCtx != nil && testCtx.NodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, testCtx.NodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", testCtx.NodeName, err)
			}
		}

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
