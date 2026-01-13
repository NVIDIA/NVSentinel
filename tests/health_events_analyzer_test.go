//go:build amd64_group && mongodb
// +build amd64_group,mongodb

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
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// All tests in this file are not yet supported for PostgreSQL.
// So we skip them for now using a build tag (mongodb) that excludes them from PostgreSQL tests.
// Github Issue: https://github.com/NVIDIA/NVSentinel/issues/606

func TestMultipleRemediationsCompleted(t *testing.T) {
	feature := features.New("TestMultipleRemediationsCompleted").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", "")

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

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, gpuNodeName, "MultipleRemediations",
			"ErrorCode:31 GPU:0 Recommended Action=CONTACT_SUPPORT;", "MultipleRemediationsIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestMultipleRemediationsNotTriggered(t *testing.T) {
	feature := features.New("TestMultipleRemediationsNotTriggered").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		testNodeName := helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		gpuNodeName := testCtx.NodeName

		t.Logf("Injecting non-fatal events to node %s", gpuNodeName)
		for range 5 {
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
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXIDOnSameGPU(t *testing.T) {
	// Works with both MongoDB ($setWindowFields pipeline) and PostgreSQL (XidBurstDetector).
	feature := features.New("TestRepeatedXIDOnSameGPU").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		return newCtx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		entities := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
		}

		entitiesImpacted = append(entitiesImpacted, entities)

		// Burst 1: 5 events within 10s gaps (same burst)
		// Burst 1 contents: XID 119 (x2), 120, 48, 31
		// Expectations: No trigger yet (need at least 2 bursts to trigger)
		errorCodes := []string{helpers.ERRORCODE_119, helpers.ERRORCODE_120, helpers.ERRORCODE_48, helpers.ERRORCODE_119, helpers.ERRORCODE_31}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU")

		t.Log("Waiting 22s to create burst gap (>20s required)")
		time.Sleep(22 * time.Second)

		// Burst 2: XID 120 (non-sticky) creates new burst after 22s gap
		// Burst 2 initial contents: XID 120, 79
		// Expectations: XID 120 triggers (appears in Burst 1 and Burst 2)
		errorCodes = []string{helpers.ERRORCODE_120, helpers.ERRORCODE_79}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		message := fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_120)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		t.Log("Waiting 22s to create burst gap (>20s required)")
		time.Sleep(22 * time.Second)

		// Burst 2 (continued): XID 119 (sticky) arrives but merges into existing Burst 2
		// because XID 79 (sticky) occurred 22s ago (within 30s sticky window)
		// Burst 2 final contents: XID 120, 79, 119, 48
		// Expectations: 119 and 48 trigger (both appear in Burst 1 and Burst 2)
		errorCodes = []string{helpers.ERRORCODE_119, helpers.ERRORCODE_48}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		t.Logf("Verifying RepeatedXIDErrorOnSameGPU condition exists after events merged into Burst 2")
		message += fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_119)
		message += fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_48)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		t.Log("Waiting 22s to create burst gap (>20s required)")
		time.Sleep(22 * time.Second)

		// Burst 3: XID 13 (non-sticky) creates new burst after 16s gap
		// Burst 3 contents: XID 13, 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 3)
		errorCodes = []string{helpers.ERRORCODE_13, helpers.ERRORCODE_31}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		time.Sleep(5 * time.Second)

		// Burst 3 (continued): XID 13 arrives again after 5s gap (< 20s), stays in same burst
		// Burst 3 final contents: XID 13 (x2), 31 (x1)
		// Expectations: XID 13 will NOT trigger (only appears in Burst 3, and targetXidCount=2 in maxBurst),
		// 				 XID 31 will also not trigger as we are excluding XID 31 from RepeatedXIDErrorOnSameGPU rule
		helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
			WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
			WithCheckName("SysLogsXIDError").
			WithEntitiesImpacted(entities).
			WithFatal(true).
			WithErrorCode(helpers.ERRORCODE_13).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)))

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")

			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXID31OnSameGPU(t *testing.T) {
	feature := features.New("TestRepeatedXID31OnSameGPU").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		t.Logf("Using node: %s", testNodeName)

		return ctx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
		}

		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0002:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-22222222-2222-2222-2222-222222222222",
			},
		}

		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		// Burst 1: 5 events within 10s gaps (same burst)
		// Burst 1 contents: XID 119, 31
		// Expectations: No trigger yet (need at least 2 bursts to trigger)
		errorCodes := []string{helpers.ERRORCODE_119, helpers.ERRORCODE_31}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID31OnDifferentGPU")

		t.Log("Waiting 22s to create burst gap (>20s required)")
		time.Sleep(22 * time.Second)

		// Burst 2: XID 31 (non-sticky) creates new burst after 25s gap
		// Burst 2 initial contents: XID 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 2 but with different PCI addresses)
		errorCodes = []string{helpers.ERRORCODE_31}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		expectedEvent := v1.Event{
			Type:    "RepeatedXID31OnDifferentGPU",
			Reason:  "RepeatedXID31OnDifferentGPUIsNotHealthy",
			Message: "ErrorCode:31 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 App passing bad data or using incorrect GPU methods. check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID31OnSameGPU")

		t.Log("Waiting 22s to create burst gap (>20s required)")
		time.Sleep(22 * time.Second)

		// Burst 3: XID 13 (non-sticky) creates new burst after 16s gap
		// Burst 3 contents: XID 13, 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 3)
		errorCodes = []string{helpers.ERRORCODE_13, helpers.ERRORCODE_31}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		message := fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 if DCGM EUD tests pass, run field diagnostics Recommended Action=RUN_DCGMEUD;", helpers.ERRORCODE_31)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXID31OnSameGPU",
			message, "RepeatedXID31OnSameGPUIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")

			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXIDErrorOnGPCAndTPC(t *testing.T) {
	feature := features.New("TestXIDErrorOnGPCAndTPC").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		testNodeName = testCtx.NodeName
		t.Logf("Using node: %s", testNodeName)

		entities1 := []helpers.EntityImpacted{

			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "GPC",
				EntityValue: "0",
			},
			{
				EntityType:  "TPC",
				EntityValue: "1",
			},
			{
				EntityType:  "SM",
				EntityValue: "0",
			},
		}
		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "GPC",
				EntityValue: "0",
			},
			{
				EntityType:  "TPC",
				EntityValue: "0",
			},
			{
				EntityType:  "SM",
				EntityValue: "1",
			},
		}

		entities3 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "GPC",
				EntityValue: "0",
			},
			{
				EntityType:  "TPC",
				EntityValue: "1",
			},
			{
				EntityType:  "SM",
				EntityValue: "1",
			},
		}

		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)
		entitiesImpacted = append(entitiesImpacted, entities3)

		// STEP 1: Inject two XID 13 errors on GPC:0, TPC:1, SM:0
		// EXPECTED: This alone won't trigger the "same" rule yet as it needs multiple occurrences
		// on the same GPC/TPC combination.
		t.Log("Inject XID 13 events on GPC: 0, TPC: 1, SM: 0")
		xidEvents := []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID13OnDifferentGPCAndTPC")

		t.Log("Waiting 17s to create burst gap (>15s required)")
		time.Sleep(17 * time.Second)

		// STEP 2: Inject XID 13 error on GPC:0, TPC:0, SM:1
		// EXPECTED: This differs from the previous errors which were on GPC:0, TPC:1.
		// This should trigger the "RepeatedXID13OnDifferentGPCAndTPC" condition
		// because we have errors occurring on different processing clusters, indicating
		// a potentially broader GPU issue rather than a localized problem.
		t.Log("Inject XID 13 events on GPC: 0, TPC: 0, SM: 1")
		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		expectedEvent := v1.Event{
			Type:    "RepeatedXID13OnDifferentGPCAndTPC",
			Reason:  "RepeatedXID13OnDifferentGPCAndTPCIsNotHealthy",
			Message: "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:0 SM:1 App passing bad data or using incorrect GPU methods. check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		// EXPECTED: RepeatedXID13OnSameGPCAndTPC is not present.
		// Burst 1: XID 13 on GPC: 0, TPC: 1, SM: 0
		//          XID 13 on GPC: 0, TPC: 1, SM: 0
		// Burst 2: XID 13 on GPC: 0, TPC: 0, SM: 1
		// Errors on different GPC/TPC combinations.
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID13OnSameGPCAndTPC")

		t.Log("Waiting 17s to create burst gap (>15s required)")
		time.Sleep(17 * time.Second)

		// STEP 3: Inject XID 13 error on GPC:0, TPC:1, SM:1
		// EXPECTED: This creates a third burst on GPC:0, TPC:1, the same GPC/TPC as Burst 1.
		// Now we have:
		//   Burst 1: GPC:0, TPC:1
		//   Burst 2: GPC:0, TPC:0
		//   Burst 3: GPC:0, TPC:1
		// This should trigger the "RepeatedXID13OnDifferentGPCAndTPC" condition (bursts on different GPC/TPC)
		// and also set up the condition for "RepeatedXID13OnSameGPCAndTPC" (bursts 1 and 3 on same GPC/TPC).
		t.Log("Inject XID 13 events on GPC: 0, TPC: 1, SM: 1")
		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities3).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		expectedEvent = v1.Event{
			Type:    "RepeatedXID13OnDifferentGPCAndTPC",
			Reason:  "RepeatedXID13OnDifferentGPCAndTPCIsNotHealthy",
			Message: "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:1 SM:1 App passing bad data or using incorrect GPU methods. check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		return ctx
	})

	feature.Assess("Check if RepeatedXID13OnSameGPCAndTPC node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// EXPECTED: RepeatedXID13OnSameGPCAndTPC is present.
		// We have injected XID 13 errors in three separate bursts (>20s gaps):
		// Burst 1: XID 13 on GPC: 0, TPC: 1, SM: 0 (two events combined)
		// Burst 2: XID 13 on GPC: 0, TPC: 0, SM: 1 (different TPC)
		// Burst 3: XID 13 on GPC: 0, TPC: 1, SM: 1 (same GPC/TPC as Burst 1)
		// Bursts 1 and 3 both occur on GPC:0, TPC:1, triggering the rule.
		message := "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:1 SM:1 if DCGM EUD tests pass, run field diagnostics Recommended Action=RUN_DCGMEUD;"
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXID13OnSameGPCAndTPC",
			message, "RepeatedXID13OnSameGPCAndTPCIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestSoloNoBurstRule(t *testing.T) {
	feature := features.New("TestSoloNoBurstRule").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)
		t.Logf("Using node: %s", testNodeName)

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "GPC",
				EntityValue: "0",
			},
			{
				EntityType:  "TPC",
				EntityValue: "1",
			},
			{
				EntityType:  "SM",
				EntityValue: "2",
			},
		}

		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0002:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-22222222-2222-2222-2222-222222222222",
			},
		}

		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		errorCodes := []string{helpers.ERRORCODE_13, helpers.ERRORCODE_13}
		for _, errorCode := range errorCodes {
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(errorCode).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
			)
		}

		t.Log("Waiting 5s to create burst gap")
		time.Sleep(5 * time.Second)

		helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testNodeName).
			WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
			WithCheckName("SysLogsXIDError").
			WithEntitiesImpacted(entities2).
			WithFatal(true).
			WithErrorCode(helpers.ERRORCODE_13).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)))

		return ctx
	})

	feature.Assess("Check if XIDErrorSoloNoBurst node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		expectedEvent := v1.Event{
			Type:    "XIDErrorSoloNoBurst",
			Reason:  "XIDErrorSoloNoBurstIsNotHealthy",
			Message: "ErrorCode:13 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 App passing bad data or using incorrect GPU methods. check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXID74Reg0SoloNVLinkError(t *testing.T) {
	feature := features.New("TestXID74Reg0SoloNVLinkError").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)
		testNodeName = testCtx.NodeName
		t.Logf("Using node: %s", testNodeName)

		return ctx
	})

	feature.Assess("Check if XID74Reg0SoloNVLinkError node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "14",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 1<<20),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}
		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
		}
		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		xidEvents := []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "SysLogsXIDError",
			"ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=RESTART_VM;",
			"SysLogsXIDErrorIsNotHealthy", v1.ConditionTrue)

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "XID74Reg0SoloNVLinkError")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities2).
				WithCheckName("SysLogsXIDError").
				WithErrorCode(helpers.ERRORCODE_13).
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU"),
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "XID74Reg0SoloNVLinkError",
			fmt.Sprintf("ErrorCode:74 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 "+
				"NVLINK:14 "+
				"REG0:%032b REG1:%032b "+
				"REG2:%032b REG3:%032b "+
				"REG4:%032b REG5:%032b "+
				"REG6:%032b one of the bits (1 or 20) is set in register 0, unexpected error please open an NVBug Recommended Action=CONTACT_SUPPORT;",
				1<<20, 0, 0, 0, 0, 0, 0),
			"XID74Reg0SoloNVLinkErrorIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXID74Reg0ECCParityError(t *testing.T) {
	feature := features.New("TestXID74Reg0ECCParityError").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)
		testNodeName = testCtx.NodeName
		t.Logf("Using node: %s", testNodeName)

		return ctx
	})

	feature.Assess("Check if XID74Reg0ECCParityError node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "14",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 1<<4),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}

		// Same GPU and same bits are set but different NVLink
		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "15",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 1<<4),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}

		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		xidEvents := []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should not be triggered as error has occurred only 1 time on the same NVLink and GPU")
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "XID74Reg0ECCParityError")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should not be triggered as error has occurred on different NVLink")
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "XID74Reg0ECCParityError")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should be triggered as error has occurred more than 1 time on the same NVLink and GPU")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "XID74Reg0ECCParityError",
			fmt.Sprintf("ErrorCode:74 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 "+
				"NVLINK:14 REG0:%032b REG1:%032b "+
				"REG2:%032b REG3:%032b "+
				"REG4:%032b REG5:%032b "+
				"REG6:%032b one of the bits (4 or 5) is set in register 0 and its repeating on same NVLink and GPU, likely a HW issue with ECC/Parity Recommended Action=CONTACT_SUPPORT;",
				1<<4, 0, 0, 0, 0, 0, 0),
			"XID74Reg0ECCParityErrorIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXID74Reg2Bit16Or19SetRule(t *testing.T) {
	feature := features.New("TestXID74Reg2Bit16Or19SetRule").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)
		testNodeName = testCtx.NodeName
		t.Logf("Using node: %s", testNodeName)

		return ctx
	})

	feature.Assess("Check if RepeatedXID74Reg2Bit16Or19Set node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "14",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 1<<16),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}
		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0002:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-22222222-2222-2222-2222-222222222222",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "14",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 1<<16),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}
		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		xidEvents := []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID74Reg2Bit16Or19Set")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID74Reg2Bit16Or19Set")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}
		t.Log("Rule should be triggered as error has occurred with bit 16 or 19 set")
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXID74Reg2Bit16Or19Set",
			fmt.Sprintf("ErrorCode:74 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 "+
				"NVLINK:14 REG0:%032b REG1:%032b "+
				"REG2:%032b REG3:%032b "+
				"REG4:%032b REG5:%032b "+
				"REG6:%032b one of the bits (16 or 19) is set in register 2 and its repeating on same GPU, request for field diagnosis Recommended Action=CONTACT_SUPPORT;",
				0, 0, 1<<16, 0, 0, 0, 0),
			"RepeatedXID74Reg2Bit16Or19SetIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXID74Reg4ECCError(t *testing.T) {
	feature := features.New("TestXID74Reg4ECCError").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var entitiesImpacted [][]helpers.EntityImpacted

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)
		testNodeName = testCtx.NodeName
		t.Logf("Using node: %s", testNodeName)

		return ctx
	})
	feature.Assess("Check if XID74Reg4ECCError node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		entities1 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "14",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 1<<26),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}
		entities2 := []helpers.EntityImpacted{
			{
				EntityType:  "PCI",
				EntityValue: "0001:00:00",
			},
			{
				EntityType:  "GPU_UUID",
				EntityValue: "GPU-11111111-1111-1111-1111-111111111111",
			},
			{
				EntityType:  "NVLINK",
				EntityValue: "15",
			},
			{
				EntityType:  "REG0",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG1",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG2",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG3",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG4",
				EntityValue: fmt.Sprintf("%032b", 1<<26),
			},
			{
				EntityType:  "REG5",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
			{
				EntityType:  "REG6",
				EntityValue: fmt.Sprintf("%032b", 0),
			},
		}
		entitiesImpacted = append(entitiesImpacted, entities1)
		entitiesImpacted = append(entitiesImpacted, entities2)

		xidEvents := []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}

		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should not be triggered as error has occurred only 1 time on the same NVLink and GPU")
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "XID74Reg4ECCError")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities2).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should not be triggered as error has occurred on different NVLink")
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "XID74Reg4ECCError")

		xidEvents = []*helpers.HealthEventTemplate{
			helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithCheckName("SysLogsXIDError").
				WithEntitiesImpacted(entities1).
				WithFatal(true).
				WithErrorCode(helpers.ERRORCODE_74).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM)),
		}
		for _, xidEvent := range xidEvents {
			helpers.SendHealthEvent(ctx, t, xidEvent)
		}

		t.Log("Rule should be triggered as error has occurred more than 1 time on the same NVLink and GPU")
		expectedMessage := fmt.Sprintf("ErrorCode:74 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 "+
			"NVLINK:14 REG0:%032b REG1:%032b "+
			"REG2:%032b REG3:%032b "+
			"REG4:%032b REG5:%032b "+
			"REG6:%032b one of the bits (20, 23, 26, 29) is set in register 4, request for field diagnosis if user jobs are interrupted or error occurs repeatedly Recommended Action=NONE;",
			0, 0, 0, 0, 1<<26, 0, 0)

		expectedEvent := v1.Event{
			Type:    "XID74Reg4ECCError",
			Reason:  "XID74Reg4ECCErrorIsNotHealthy",
			Message: expectedMessage,
		}
		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, entities := range entitiesImpacted {
			syslogHealthEvent := helpers.NewHealthEvent(testNodeName).
				WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
				WithEntitiesImpacted(entities).
				WithCheckName("SysLogsXIDError").
				WithFatal(false).
				WithHealthy(true).
				WithMessage("No health failures").
				WithComponentClass("GPU")
			helpers.SendHealthEvent(ctx, t, syslogHealthEvent)
		}

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}
