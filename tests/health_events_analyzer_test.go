// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	"tests/helpers"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestMultipleRemediationsCompleted(t *testing.T) {
	feature := features.New("TestMultipleRemediationsCompleted").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test")

		t.Log("Waiting 90 seconds for the MultipleRemediations rule time window to complete")
		time.Sleep(90 * time.Second)

		t.Log("Triggering multiple remediations cycle")
		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.TriggerMultipleRemediationsCycle(ctx, t, client, testCtx.NodeName)

		return newCtx
	})

	feature.Assess("Check if MultipleRemediations node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")
		gpuNodeName := testCtx.NodeName

		event := helpers.NewHealthEvent(gpuNodeName).
			WithFatal(true).
			WithErrorCode(helpers.ERRORCODE_31).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
		helpers.SendHealthEvent(ctx, t, event)

		// FIXME(dims): This is not happening correctly and causing failures in CI.
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, gpuNodeName, "MultipleRemediations",
			"ErrorCode:31 GPU:0 Recommended Action=CONTACT_SUPPORT;", "MultipleRemediationsIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup, helpers.ERRORCODE_31)
	})

	testEnv.Test(t, feature.Feature())
}

func TestMultipleRemediationsNotTriggered(t *testing.T) {
	feature := features.New("TestMultipleRemediationsNotTriggered").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test")

		gpuNodeName := testCtx.NodeName

		t.Logf("Injecting non-fatal events to node %s", gpuNodeName)
		for i := 0; i < 5; i++ {
			event := helpers.NewHealthEvent(gpuNodeName).
				WithFatal(false).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))

			helpers.SendHealthEvent(ctx, t, event)

			helpers.SendHealthyEvent(ctx, t, gpuNodeName)
		}

		return newCtx
	})

	feature.Assess("Check if MultipleRemediations node condition is NOT added for non-fatal events", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		gpuNodeName := testCtx.NodeName

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		event := helpers.NewHealthEvent(gpuNodeName).
			WithFatal(false).
			WithErrorCode(helpers.ERRORCODE_13).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
		helpers.SendHealthEvent(ctx, t, event)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, gpuNodeName, "MultipleRemediations")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup, helpers.ERRORCODE_31)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXIDRule(t *testing.T) {
	// Works with both MongoDB ($setWindowFields pipeline) and PostgreSQL (XidBurstDetector).
	feature := features.New("TestRepeatedXIDRule").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		_, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test")
		return ctx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")
		t.Logf("Cleaning up any existing node conditions for node %s", testCtx.NodeName)
		// Note: Using default agent (gpu-health-monitor) instead of health-events-analyzer
		// because the reconciler filters out events from health-events-analyzer to prevent infinite loops
		errorsToInject := []string{helpers.ERRORCODE_119, helpers.ERRORCODE_120, helpers.ERRORCODE_48, helpers.ERRORCODE_79}
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithCheckName("RepeatedXidError").
				WithHealthy(true).
				WithFatal(false).
				WithErrorCode(xid)
			helpers.SendHealthEvent(ctx, t, event)
		}

		// Sticky XIDs: "74", "79", "95", "109", "119"
		t.Logf("Injecting fatal events to node %s", testCtx.NodeName)

		// Burst 1: 5 events within 10s gaps (same burst)
		// Burst 1 contents: XID 119 (x2), 120, 48, 31
		// Expectations: No trigger yet (need at least 2 bursts to trigger)
		errorsToInject = []string{helpers.ERRORCODE_119, helpers.ERRORCODE_120, helpers.ERRORCODE_48, helpers.ERRORCODE_119, helpers.ERRORCODE_31}
		message := ""
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode(xid).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
			helpers.SendHealthEvent(ctx, t, event)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testCtx.NodeName, "RepeatedXidError")

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 2: XID 120 (non-sticky) creates new burst after 12s gap
		// Burst 2 initial contents: XID 120, 79
		// Expectations: XID 120 triggers (appears in Burst 1 and Burst 2)
		errorsToInject = []string{helpers.ERRORCODE_120, helpers.ERRORCODE_79}
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode(xid).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
			helpers.SendHealthEvent(ctx, t, event)
		}
		message = fmt.Sprintf("ErrorCode:%s GPU:0 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_120)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testCtx.NodeName, "RepeatedXidError",
			message, "RepeatedXidErrorIsNotHealthy", v1.ConditionTrue)

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 2 (continued): XID 119 (sticky) arrives but merges into existing Burst 2
		// because XID 79 (sticky) occurred 12s ago (within 20s window)
		// Burst 2 final contents: XID 120, 79, 119 (x2), 48
		// Expectations: 119 and 48 trigger (both appear in Burst 1 and Burst 2)
		errorsToInject = []string{helpers.ERRORCODE_119, helpers.ERRORCODE_48, helpers.ERRORCODE_119}
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode(xid).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
			helpers.SendHealthEvent(ctx, t, event)
		}

		t.Logf("Verifying RepeatedXidError condition exists after events merged into Burst 2")
		message += fmt.Sprintf("ErrorCode:%s GPU:0 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_119)
		message += fmt.Sprintf("ErrorCode:%s GPU:0 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_48)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testCtx.NodeName, "RepeatedXidError",
			message, "RepeatedXidErrorIsNotHealthy", v1.ConditionTrue)

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 3: XID 13 (non-sticky) creates new burst after 12s gap
		// Burst 3 contents: XID 13, 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 3 with targetXidCount=1 in maxBurst)
		errorsToInject = []string{helpers.ERRORCODE_13, helpers.ERRORCODE_31}
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode(xid).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
			helpers.SendHealthEvent(ctx, t, event)
		}
		time.Sleep(5 * time.Second)

		// Burst 3 (continued): XID 13 arrives again after 5s gap (< 10s), stays in same burst
		// Burst 3 final contents: XID 13 (x2), 31 (x1)
		// Expectations: XID 13 will NOT trigger (only appears in Burst 3, and targetXidCount=2 in maxBurst)
		errorsToInject = []string{helpers.ERRORCODE_13}
		for _, xid := range errorsToInject {
			event := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode(xid).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
			helpers.SendHealthEvent(ctx, t, event)
		}

		message += fmt.Sprintf("ErrorCode:%s GPU:0 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_31)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testCtx.NodeName, "RepeatedXidError",
			message, "RepeatedXidErrorIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup, helpers.ERRORCODE_119)
	})

	testEnv.Test(t, feature.Feature())
}
