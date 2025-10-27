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
	"os"
	"testing"
	"tests/helpers"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestExistingCRPreventsNewCreation(t *testing.T) {
	feature := features.New("TestExistingCRPreventsNewCreation").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("existing CR prevents duplicate creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		cr1Name := cr1.GetName()
		t.Logf("First CR created: %s", cr1Name)

		t.Log("Triggering remediation flow again without cleanup")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		t.Log("Verifying no duplicate CR was created - should have exactly the original CR")
		require.Eventually(t, func() bool {
			crList, err := helpers.GetRebootNodeCRsForNode(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			if len(crList) == 1 && crList[0] == cr1Name {
				return true // Exactly 1 CR with the original name
			}
			if len(crList) > 1 {
				t.Logf("ERROR: Found %d CRs, duplicate created!", len(crList))
			} else {
				t.Logf("Waiting for stable CR count, currently: %d", len(crList))
			}
			return false
		}, helpers.WaitTimeout, helpers.WaitInterval, "should have exactly the original CR, no duplicates")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationModuleRestart(t *testing.T) {
	feature := features.New("TestRemediationModuleRestart").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("module handles restart gracefully", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Step 1: Trigger drain to reach succeeded state")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Step 2: Restart fault-remediation module before CR creation")
		helpers.RestartFaultRemediationDeployment(ctx, t, client)

		t.Log("Step 3: Verify CR is still created after restart")
		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			helpers.NVSentinelStateLabelKey, helpers.RemediationSucceededLabelValue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestFailedCRRetry(t *testing.T) {
	feature := features.New("TestFailedCRRetry").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("failed CR allows retry on new event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		cr1Name := cr1.GetName()
		t.Logf("First CR created: %s", cr1Name)

		t.Log("Simulating CR failure by updating status")
		helpers.UpdateRebootNodeCRStatus(ctx, t, client, cr1Name, "Failed")

		t.Log("Cleaning up and triggering new remediation")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		t.Log("Waiting for healthy event to be processed")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			// Node should be uncordoned after healthy event
			return !node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		t.Log("Verifying new CR was created after previous failure")
		cr2 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr2)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationWithoutDrainCompletion(t *testing.T) {
	feature := features.New("TestRemediationWithoutDrainCompletion").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "allowcompletion-test")
		return newCtx
	})

	feature.Assess("no CR created if drain not completed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Creating pods that won't be evicted")
		podNames := helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Sending fatal event to trigger quarantine")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Waiting to verify drain starts but doesn't complete")
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying no RebootNode CR created while drain in progress")
		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		t.Log("Cleaning up pods to complete drain")
		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Now CR should be created after drain completes")
		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
