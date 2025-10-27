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
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestMultipleNamespacesMatchWildcardPattern(t *testing.T) {
	feature := features.New("TestMultipleNamespacesMatchWildcardPattern").
		WithLabel("suite", "node-drainer-advanced")

	var testCtx *helpers.NodeDrainerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c,
			"data/nd-wildcard-nonprod.yaml", "test-non-prod")

		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.CreateNamespace(ctx, client, "dev-non-prod"))
		require.NoError(t, helpers.CreateNamespace(ctx, client, "staging-non-prod"))

		return newCtx
	})

	feature.Assess("multiple namespaces match wildcard pattern", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		time.Sleep(10 * time.Second)

		t.Log("Creating pods in multiple *non-prod* namespaces")
		testPods := helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, "test-non-prod")
		devPods := helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, "dev-non-prod")
		stagingPods := helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, "staging-non-prod")

		helpers.WaitForPodsRunning(ctx, t, client, "test-non-prod", testPods)
		helpers.WaitForPodsRunning(ctx, t, client, "dev-non-prod", devPods)
		helpers.WaitForPodsRunning(ctx, t, client, "staging-non-prod", stagingPods)

		t.Log("Triggering drain")
		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + InProgress")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "InProgress")

		t.Log("Verifying all pods in matching namespaces are waiting")
		time.Sleep(30 * time.Second)
		for _, podName := range testPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "test-non-prod", pod)
			require.NoError(t, err, "test-non-prod pod should still exist")
		}
		for _, podName := range devPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "dev-non-prod", pod)
			require.NoError(t, err, "dev-non-prod pod should still exist")
		}
		for _, podName := range stagingPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "staging-non-prod", pod)
			require.NoError(t, err, "staging-non-prod pod should still exist")
		}

		t.Log("Completing test-non-prod pods")
		helpers.DeletePodsByNames(ctx, t, client, "test-non-prod", testPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "test-non-prod", testPods)

		time.Sleep(10 * time.Second)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		labelValue := node.Labels[helpers.NVSentinelStateLabelKey]
		require.Equal(t, helpers.DrainingLabelValue, labelValue, "should still be draining")

		t.Log("Completing all remaining pods")
		helpers.DeletePodsByNames(ctx, t, client, "dev-non-prod", devPods)
		helpers.DeletePodsByNames(ctx, t, client, "staging-non-prod", stagingPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "dev-non-prod", devPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "staging-non-prod", stagingPods)

		t.Log("Verifying drain succeeds after all pods complete")
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + Succeeded")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.DeleteNamespace(ctx, t, client, "dev-non-prod")
		helpers.DeleteNamespace(ctx, t, client, "staging-non-prod")

		return helpers.TeardownNodeDrainerTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
