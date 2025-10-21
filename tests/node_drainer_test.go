// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

func TestNodeDrainerEvictionModes(t *testing.T) {
	feature := features.New("TestNodeDrainerEvictionModes").
		WithLabel("suite", "node-drainer")

	var testCtx *helpers.NodeDrainerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "immediate-test")

		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.CreateNamespace(ctx, client, "allowcompletion-test"))
		require.NoError(t, helpers.CreateNamespace(ctx, client, "delete-timeout-test"))

		return newCtx
	})

	feature.Assess("immediate mode evicts pods quickly", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		immediatePods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "immediate-test")
		helpers.WaitForPodsRunning(ctx, t, client, "immediate-test", immediatePods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		t.Log("Verifying immediate-test pod evicted quickly")
		helpers.WaitForPodsDeleted(ctx, t, client, "immediate-test", immediatePods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + Succeeded")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		return ctx
	})

	feature.Assess("allowCompletion mode allows pods to complete", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		allowCompletionPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "allowcompletion-test")
		helpers.WaitForPodsRunning(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + InProgress")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "InProgress")

		t.Log("Verifying allowCompletion pod NOT evicted")
		time.Sleep(30 * time.Second)
		for _, podName := range allowCompletionPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "allowcompletion-test", pod)
			require.NoError(t, err, "allowCompletion pod %s evicted but should still be running", podName)
		}

		t.Log("Manually deleting pod to complete drain")
		helpers.DeletePodsByNames(ctx, t, client, "allowcompletion-test", allowCompletionPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + Succeeded")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		return ctx
	})

	feature.Assess("deleteAfterTimeout mode force deletes after timeout", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		deleteTimeoutPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "delete-timeout-test")
		helpers.WaitForPodsRunning(ctx, t, client, "delete-timeout-test", deleteTimeoutPods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying pod NOT immediately deleted")
		time.Sleep(30 * time.Second)
		for _, podName := range deleteTimeoutPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "delete-timeout-test", pod)
			require.NoError(t, err, "pod %s deleted too early", podName)
		}

		t.Log("Waiting for deleteAfterTimeout (1min + buffer)")
		time.Sleep(1*time.Minute + 45*time.Second)

		t.Log("Verifying pod force deleted after timeout")
		helpers.WaitForPodsDeleted(ctx, t, client, "delete-timeout-test", deleteTimeoutPods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + Succeeded")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		return ctx
	})

	feature.Assess("namespace exclusion protects system namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		kubeSystemPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "kube-system")
		immediatePods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "immediate-test")

		helpers.WaitForPodsRunning(ctx, t, client, "kube-system", kubeSystemPods)
		helpers.WaitForPodsRunning(ctx, t, client, "immediate-test", immediatePods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 error")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying immediate-test pod evicted")
		helpers.WaitForPodsDeleted(ctx, t, client, "immediate-test", immediatePods)

		t.Log("Verifying kube-system pod NOT evicted (excluded)")
		time.Sleep(10 * time.Second)
		for _, podName := range kubeSystemPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "kube-system", pod)
			require.NoError(t, err, "kube-system pod %s evicted but should be excluded", podName)
		}

		helpers.DeletePodsByNames(ctx, t, client, "kube-system", kubeSystemPods)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.DeleteNamespace(ctx, t, client, "allowcompletion-test")
		helpers.DeleteNamespace(ctx, t, client, "delete-timeout-test")

		return helpers.TeardownNodeDrainerTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
