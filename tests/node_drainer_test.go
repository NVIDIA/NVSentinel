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
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

func TestNodeDrainerEvictionModes(t *testing.T) {
	feature := features.New("TestNodeDrainerEvictionModes").
		WithLabel("suite", "node-drainer")

	var testCtx *helpers.NodeDrainerTestContext
	var kubeSystemPods, immediatePods, allowCompletionPods, deleteTimeoutPods []string
	var finalizerPod string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "immediate-test")

		require.NoError(t, helpers.CreateNamespace(ctx, client, "allowcompletion-test"))
		require.NoError(t, helpers.CreateNamespace(ctx, client, "delete-timeout-test"))

		kubeSystemPods = helpers.CreatePodsFromTemplate(newCtx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "kube-system")
		immediatePods = helpers.CreatePodsFromTemplate(newCtx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "immediate-test")
		finalizerPodNames := helpers.CreatePodsFromTemplate(newCtx, t, client, "data/busybox-pod-with-finalizer.yaml", testCtx.NodeName, "immediate-test")
		allowCompletionPods = helpers.CreatePodsFromTemplate(newCtx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "allowcompletion-test")
		deleteTimeoutPods = helpers.CreatePodsFromTemplate(newCtx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "delete-timeout-test")

		require.Len(t, finalizerPodNames, 1)
		finalizerPod = finalizerPodNames[0]

		helpers.WaitForPodsRunning(newCtx, t, client, "kube-system", kubeSystemPods)
		helpers.WaitForPodsRunning(newCtx, t, client, "immediate-test", append(immediatePods, finalizerPod))
		helpers.WaitForPodsRunning(newCtx, t, client, "allowcompletion-test", allowCompletionPods)
		helpers.WaitForPodsRunning(newCtx, t, client, "delete-timeout-test", deleteTimeoutPods)

		return newCtx
	})

	feature.Assess("all eviction modes in single drain cycle", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		defer func() {
			var p v1.Pod
			if err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p); err == nil {
				p.Finalizers = []string{}
				_ = client.Resources().Update(ctx, &p)
			}
			_ = client.Resources().Delete(ctx, &p)
		}()

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Phase 1: Immediate mode evicts pods immediately")
		helpers.WaitForPodsDeleted(ctx, t, client, "immediate-test", immediatePods)

		t.Log("Phase 1: kube-system pods NOT evicted (namespace exclusion)")
		helpers.AssertPodsNeverDeleted(ctx, t, client, "kube-system", kubeSystemPods)

		t.Log("Phase 1: Finalizer pod stuck in Terminating")
		err = helpers.DeletePod(ctx, t, client, "immediate-test", finalizerPod, false)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			var p v1.Pod
			err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p)
			if err != nil {
				return false
			}
			return p.DeletionTimestamp != nil
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		require.Never(t, func() bool {
			var p v1.Pod
			err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p)
			return err != nil
		}, helpers.NeverWaitTimeout, helpers.WaitInterval)

		t.Log("Phase 2: Both allowCompletion and deleteAfterTimeout waiting (verify for 15s)")
		require.Never(t, func() bool {
			for _, podName := range allowCompletionPods {
				pod := &v1.Pod{}
				if err := client.Resources().Get(ctx, podName, "allowcompletion-test", pod); err != nil {
					return true
				}
			}
			for _, podName := range deleteTimeoutPods {
				pod := &v1.Pod{}
				if err := client.Resources().Get(ctx, podName, "delete-timeout-test", pod); err != nil {
					return true
				}
			}
			return false
		}, helpers.NeverWaitTimeout, helpers.WaitInterval, "both mode pods should wait, not be deleted immediately")

		t.Log("Phase 3: Waiting for deleteAfterTimeout to expire (~60s)")
		// The deleteAfterTimeoutMinutes is set to 1 minute in nd-all-modes.yaml
		// Adding 10s buffer to account for processing time
		time.Sleep(70 * time.Second)

		t.Log("Phase 4: DeleteAfterTimeout pods force-deleted after timeout")
		// This verifies that DeleteAfterTimeout is processed before AllowCompletion (not blocked by it)
		helpers.WaitForPodsDeleted(ctx, t, client, "delete-timeout-test", deleteTimeoutPods)

		t.Log("Phase 5: Verify AllowCompletion pods are still waiting (priority verification)")
		// AllowCompletion pods should still exist - they wait indefinitely for natural completion
		for _, podName := range allowCompletionPods {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, "allowcompletion-test", pod)
			require.NoError(t, err, "AllowCompletion pod %s should still exist after DeleteAfterTimeout completes", podName)
			require.Nil(t, pod.DeletionTimestamp, "AllowCompletion pod %s should not be terminating", podName)
		}

		t.Log("Phase 6: Manually completing AllowCompletion pods to finish drain")
		helpers.DeletePodsByNames(ctx, t, client, "allowcompletion-test", allowCompletionPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		helpers.DeletePodsByNames(ctx, t, client, "kube-system", kubeSystemPods)

		return ctx
	})

	feature.Assess("drainer resumes work after restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames := helpers.ResetNodeAndTriggerDrain(ctx, t, client, testCtx.NodeName, "allowcompletion-test")

		restartTime := time.Now()
		err = helpers.RestartDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			found, event := helpers.CheckNodeEventExists(ctx, client, testCtx.NodeName, "NodeDraining", "", restartTime)
			if found {
				t.Logf("Found event after restart: %s", event.Reason)
			}
			return found
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		helpers.DeletePodsByNames(ctx, t, client, "allowcompletion-test", podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, "allowcompletion-test", podNames)
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.DeleteNamespace(ctx, t, client, "allowcompletion-test")
		helpers.DeleteNamespace(ctx, t, client, "delete-timeout-test")

		return helpers.TeardownNodeDrainer(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
