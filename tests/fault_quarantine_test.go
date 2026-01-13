//go:build amd64_group
// +build amd64_group

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
	"testing"

	"tests/helpers"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestDontCordonIfEventDoesntMatchCELExpression(t *testing.T) {
	feature := features.New("TestCELExpressionFiltering").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/managed-by-nvsentinel-configmap.yaml")
		return newCtx
	})

	feature.Assess("event doesn't match CEL expression", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithCheckName("UnknownCheck").
			WithErrorCode("999")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Assess("node with ManagedByNVSentinel=false label ignored by CEL", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testCtx.NodeName, false)
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testCtx.NodeName, true)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestPreCordonedNodeHandling(t *testing.T) {
	feature := features.New("TestPreCordonedNodeHandling").
		WithLabel("suite", "fault-quarantine-special-modes")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")

		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Manually cordoning and tainting node %s", testCtx.NodeName)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = true
		node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
			Key:    "manual-taint",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		})

		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)
		t.Logf("Node %s pre-cordoned with manual taint", testCtx.NodeName)

		return newCtx
	})

	feature.Assess("FQ adds its taints to pre-cordoned node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		t.Log("Waiting for FQ to add its taint to pre-cordoned node")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				t.Logf("failed to get node: %v", err)
				return false
			}

			hasFQTaint := false
			hasManualTaint := false

			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" {
					hasFQTaint = true
				}
				if taint.Key == "manual-taint" {
					hasManualTaint = true
				}
			}

			t.Logf("Node state: hasFQTaint=%v, hasManualTaint=%v, cordoned=%v, taints=%+v",
				hasFQTaint, hasManualTaint, node.Spec.Unschedulable, node.Spec.Taints)

			if !hasFQTaint {
				t.Log("Waiting for FQ taint to be added")
				return false
			}

			return hasFQTaint && hasManualTaint && node.Spec.Unschedulable
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)
		t.Log("FQ taint successfully added to pre-cordoned node")

		return ctx
	})

	feature.Assess("FQ annotations added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.NotNil(t, node.Annotations)

		_, exists := node.Annotations["quarantineHealthEvent"]
		assert.True(t, exists)

		return ctx
	})

	feature.Assess("FQ clears its taints on healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			hasFQTaint := false
			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" {
					hasFQTaint = true
					break
				}
			}

			_, hasAnnotation := node.Annotations["quarantineHealthEvent"]

			return !hasFQTaint && !hasAnnotation
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		if err == nil {
			node.Spec.Unschedulable = false
			newTaints := []v1.Taint{}
			for _, taint := range node.Spec.Taints {
				if taint.Key != "manual-taint" {
					newTaints = append(newTaints, taint)
				}
			}
			node.Spec.Taints = newTaints
			client.Resources().Update(ctx, node)
		}

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestFaultQuarantineWithProcessingStrategy(t *testing.T) {
	feature := features.New("TestFaultQuarantineWithProcessingStrategy").
		WithLabel("suite", "fault-quarantine-with-processing-strategy")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("Check that node is not quarantined for STORE_ONLY events", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred").
			WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
			WithCheckName("SysLogsXIDError").
			WithProcessingStrategy(int(protos.ProcessingStrategy_STORE_ONLY))
		helpers.SendHealthEvent(ctx, t, event)

		t.Logf("Node %s should not have condition SysLogsXIDError", testCtx.NodeName)
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testCtx.NodeName, "SysLogsXIDError")

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		event = helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("DCGM_FR_CLOCK_THROTTLE_POWER").
			WithCheckName("GpuPowerWatch").
			WithFatal(false).
			WithProcessingStrategy(int(protos.ProcessingStrategy_STORE_ONLY))
		helpers.SendHealthEvent(ctx, t, event)

		t.Logf("Node %s should not have GpuPowerWatch node event", testCtx.NodeName)
		helpers.EnsureNodeEventNotPresent(ctx, t, client, testCtx.NodeName, "GpuPowerWatch", "GpuPowerWatchIsNotHealthy")

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Assess("Check that node is quarantined for EXECUTE_REMEDIATION events", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred").
			WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
			WithCheckName("SysLogsXIDError").
			WithProcessingStrategy(int(protos.ProcessingStrategy_EXECUTE_REMEDIATION))
		helpers.SendHealthEvent(ctx, t, event)

		t.Logf("Node %s should have condition SysLogsXIDError", testCtx.NodeName)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testCtx.NodeName, "SysLogsXIDError", "", "SysLogsXIDErrorIsNotHealthy", v1.ConditionTrue)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		event = helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("DCGM_FR_CLOCK_THROTTLE_POWER").
			WithCheckName("GpuPowerWatch").
			WithFatal(false).
			WithProcessingStrategy(int(protos.ProcessingStrategy_EXECUTE_REMEDIATION))
		helpers.SendHealthEvent(ctx, t, event)

		t.Logf("Node %s should have node event GpuPowerWatch", testCtx.NodeName)
		expectedEvent := v1.Event{
			Type:    "GpuPowerWatch",
			Reason:  "GpuPowerWatchIsNotHealthy",
			Message: "ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:0 Recommended Action=NONE;",
		}
		helpers.WaitForNodeEvent(ctx, t, client, testCtx.NodeName, expectedEvent)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithHealthy(true).
			WithAgent(helpers.SYSLOG_HEALTH_MONITOR_AGENT).
			WithCheckName("SysLogsXIDError")
		helpers.SendHealthEvent(ctx, t, event)

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
