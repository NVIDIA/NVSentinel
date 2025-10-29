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
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
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
				return true
			}
			if len(crList) > 1 {
				t.Logf("ERROR: Found %d CRs, duplicate created!", len(crList))
			} else {
				t.Logf("Waiting for stable CR count, currently: %d", len(crList))
			}
			return false
		}, 30*time.Second, helpers.WaitInterval, "should have exactly the original CR, no duplicates")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationModuleRestart(t *testing.T) {
	feature := features.New("TestRemediationModuleRestart").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext
	var podNames []string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "allowcompletion-test")
		newCtx = helpers.ApplyNodeDrainerConfig(newCtx, t, c, "data/nd-all-modes.yaml")

		return newCtx
	})

	feature.Assess("create blocking pods and trigger drain", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames = helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		return ctx
	})

	feature.Assess("drain starts but does not complete", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		return ctx
	})

	feature.Assess("restart module during in-progress drain", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		err = helpers.RestartDeployment(ctx, t, client, "nvsentinel-fault-remediation", helpers.NVSentinelNamespace)
		require.NoError(t, err)

		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		return ctx
	})

	feature.Assess("complete drain and verify CR creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, podNames)

		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		t.Log("Waiting for remediation to succeed")
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			statemanager.NVSentinelStateLabelKey, string(statemanager.RemediationSucceededLabelValue))

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		helpers.RestoreNodeDrainerConfig(ctx, t, c)

		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
