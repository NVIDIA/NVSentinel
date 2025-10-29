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
	"encoding/json"
	"os"
	"testing"
	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestBasicCELMatching(t *testing.T) {
	feature := features.New("TestBasicCELMatching").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")
		return newCtx
	})

	feature.Assess("event matches CEL expression", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("event doesn't match CEL expression", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithCheckName("UnknownCheck").
			WithErrorCode("999")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRulesetPriority(t *testing.T) {
	feature := features.New("TestRulesetPriority").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/ruleset-priority-configmap.yaml")
		return newCtx
	})

	feature.Assess("higher priority rule's taint effect wins", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithComponentClass("GPU").
			WithErrorCode("79").
			WithMessage("XID error occurred")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		t.Log("Verifying higher priority rule's taint effect is applied")
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectPreferNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestMultipleRulesMatching(t *testing.T) {
	feature := features.New("TestMultipleRulesMatching").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/multiple-rules-configmap.yaml")
		return newCtx
	})

	feature.Assess("XID 143 matches Rule A", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode("143").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("XID 143 cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("143").
			WithMessage("XID 143 error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "GPUHealth",
				Value:  "False",
				Effect: v1.TaintEffectPreferNoSchedule,
			},
			ExpectCordoned:   false,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("XID 79 matches Rule C", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode("79").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("XID 79 cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).WithErrorCode("79")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "GPU-Fatal",
				Value:  "True",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("GpuInforomWatch matches Rule B", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithCheckName("GpuInforomWatch").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("GPU InfoROM watch cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.WaitForQuarantineCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithCheckName("GpuInforomWatch").
			WithMessage("GPU InfoROM watch error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("XID 62 matches Rule D exclusion", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).WithErrorCode("62")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Assess("XID 999 matches no rule", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).WithErrorCode("999")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestCordonBehavior(t *testing.T) {
	feature := features.New("TestCordonBehavior").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/cordon-behavior-configmap.yaml")
		return newCtx
	})

	feature.Assess("shouldCordon=true cordons node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode("79").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("XID 79 cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).WithErrorCode("79")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "TestCordon",
				Value:  "True",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("shouldCordon=false doesn't cordon", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithErrorCode("143").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("XID 143 cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.SendHealthyEventAndWaitForCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).WithErrorCode("143")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "TestCordon",
				Value:  "False",
				Effect: v1.TaintEffectPreferNoSchedule,
			},
			ExpectCordoned:   false,
			ExpectAnnotation: true,
		})

		return ctx
	})

	feature.Assess("multiple rules with any shouldCordon=true cordons", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Cleanup(func() {
			healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
				WithCheckName("GpuInforomWatch").
				WithComponentClass("GPU").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("GpuInforomWatch cleared")
			tempFile := helpers.SendHealthEvent(ctx, t, healthyEvent)
			defer os.Remove(tempFile)
			helpers.WaitForQuarantineCleanup(ctx, t, client, testCtx.NodeName)
		})

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithCheckName("GpuInforomWatch").
			WithComponentClass("GPU")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestManualUncordonWithAnnotationRetention(t *testing.T) {
	feature := features.New("TestManualUncordonWithAnnotationRetention").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		tempFile := helpers.SendHealthEvent(newCtx, t, event)
		defer os.Remove(tempFile)

		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.AssertQuarantineState(newCtx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		return newCtx
	})

	feature.Assess("manual uncordon keeps annotation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = false
		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			if node.Spec.Unschedulable {
				return false
			}
			_, exists := node.Annotations["quarantineHealthEvent"]
			return exists
		}, helpers.WaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Assess("healthy event clears annotation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestMultipleEntityTracking(t *testing.T) {
	feature := features.New("TestMultipleEntityTracking").
		WithLabel("suite", "fault-quarantine-cel")

	var testCtx *helpers.QuarantineTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupQuarantineTest(ctx, t, c, "data/basic-matching-configmap.yaml")
		return newCtx
	})

	feature.Assess("first entity failure quarantines node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithEntity("GPU", "0").
			WithErrorCode("79")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectTaint: &v1.Taint{
				Key:    "AggregatedNodeHealth",
				Value:  "False",
				Effect: v1.TaintEffectNoSchedule,
			},
			ExpectCordoned:   true,
			ExpectAnnotation: true,
		})

		helpers.AssertAnnotationContains(ctx, t, client, testCtx.NodeName, `"entityValue":"0"`)

		return ctx
	})

	feature.Assess("second entity failure tracked", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithEntity("GPU", "1").
			WithErrorCode("79")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			annotation, exists := node.Annotations["quarantineHealthEvent"]
			if !exists {
				return false
			}

			var events []map[string]interface{}
			if err := json.Unmarshal([]byte(annotation), &events); err != nil {
				return false
			}

			return len(events) == 2
		}, helpers.WaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Assess("partial recovery keeps quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithEntity("GPU", "0").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("GPU 0 healthy")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		t.Logf("Waiting for partial recovery - GPU 0 should be removed, GPU 1 should remain")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}

			if !node.Spec.Unschedulable {
				t.Logf("Node is not cordoned (expected cordoned)")
				return false
			}

			annotation, exists := node.Annotations["quarantineHealthEvent"]
			if !exists {
				t.Logf("Quarantine annotation does not exist")
				return false
			}

			var events []map[string]any
			if err := json.Unmarshal([]byte(annotation), &events); err != nil {
				t.Logf("Failed to unmarshal annotation: %v", err)
				return false
			}

			t.Logf("Current annotation has %d events (expecting 1)", len(events))
			if len(events) != 1 {
				t.Logf("Annotation content: %s", annotation)
				return false
			}

			t.Logf("Partial recovery successful: 1 event remaining")
			return true
		}, helpers.WaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Assess("complete recovery clears quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithEntity("GPU", "1").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("GPU 1 healthy")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned:   false,
			ExpectAnnotation: false,
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
