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

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestFullRemediationFlow tests the complete remediation lifecycle including
// label transitions, CR creation, annotation tracking, MongoDB status updates,
// and recovery (annotation cleanup after healthy event)
func TestFullRemediationFlow(t *testing.T) {
	feature := features.New("TestFullRemediationFlow").
		WithLabel("suite", "fault-remediation")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("complete remediation flow with all validations", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Step 1: Send fatal event to trigger quarantine and drain")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		t.Log("Step 2: Wait for quarantine")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Step 3: Verify MongoDB drain status (drain completed)")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		t.Log("Step 4: Wait for remediation-succeeded label")
		helpers.WaitForNodeRemediationLabel(ctx, t, client, testCtx.NodeName,
			helpers.RemediationSucceededLabelValue)

		t.Log("Step 5: Verify RebootNode CR was created")
		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)
		crName := cr.GetName()

		t.Log("Step 6: Verify remediation state annotation contains CR name")
		annotation := helpers.WaitForNodeAnnotation(ctx, t, client, testCtx.NodeName,
			"latestFaultRemediationState")
		require.Contains(t, annotation, crName,
			"Annotation should contain CR name")

		t.Log("Step 7: Verify MongoDB faultRemediated status and timestamp")
		helpers.WaitForMongoRemediationStatus(ctx, t, client, testCtx.NodeName, true)

		t.Log("Verifying lastRemediationTimestamp is set")
		require.Eventually(t, func() bool {
			event := helpers.GetLatestHealthEvent(ctx, t, client, testCtx.NodeName)
			if event == nil {
				return false
			}
			status, ok := event["healtheventstatus"].(map[string]interface{})
			if !ok {
				return false
			}
			_, hasTimestamp := status["lastremediationtimestamp"]
			return hasTimestamp
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Step 8: Send healthy event to trigger recovery/unquarantine")
		healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithHealthy(true).
			WithFatal(false).
			WithMessage("Node healthy - cleared error")
		tempFile2 := helpers.SendHealthEvent(ctx, t, healthyEvent)
		defer os.Remove(tempFile2)

		t.Log("Step 9: Verify remediation state annotation cleared after recovery")
		helpers.WaitForNoNodeAnnotation(ctx, t, client, testCtx.NodeName,
			"latestFaultRemediationState")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

// TestSkipRemediationCR verifies scenarios where no remediation CR should be created
func TestSkipRemediationCR(t *testing.T) {
	feature := features.New("TestSkipRemediationCR").
		WithLabel("suite", "fault-remediation")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("NONE action (0) skips CR creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Triggering remediation flow with NONE action")
		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 0)

		t.Log("Verifying no RebootNode CR was created")
		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		return ctx
	})

	feature.Assess("unsupported action (5) skips CR and sets failed label", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Cleaning up from previous assess")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		time.Sleep(5 * time.Second)

		t.Log("Triggering remediation flow with CONTACT_SUPPORT action")
		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 5)

		t.Log("Verifying no RebootNode CR created for CONTACT_SUPPORT action")
		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		t.Log("Verifying remediation-failed label is set")
		helpers.WaitForNodeRemediationLabel(ctx, t, client, testCtx.NodeName,
			helpers.RemediationFailedLabelValue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
