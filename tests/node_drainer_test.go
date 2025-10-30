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
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestNodeDrainerEvictionModes(t *testing.T) {
	feature := features.New("TestNodeDrainerEvictionModes").
		WithLabel("suite", "node-drainer")

	var testCtx *helpers.NodeDrainerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		err = helpers.ApplyKWOKStage(ctx, t, client, "data/kwok-pod-delete-respect-finalizers.yaml")
		require.NoError(t, err)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "immediate-test")

		require.NoError(t, helpers.CreateNamespace(ctx, client, "allowcompletion-test"))
		require.NoError(t, helpers.CreateNamespace(ctx, client, "delete-timeout-test"))

		return newCtx
	})

	feature.Assess("immediate mode evicts pods and ignores terminating pods", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		immediatePods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "immediate-test")
		finalizerPodNames := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pod-with-finalizer.yaml", testCtx.NodeName, "immediate-test")
		require.Len(t, finalizerPodNames, 1)
		finalizerPod := finalizerPodNames[0]

		defer func() {
			var p v1.Pod
			if err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p); err == nil {
				p.Finalizers = []string{}
				_ = client.Resources().Update(ctx, &p)
			}
			_ = client.Resources().Delete(ctx, &p)
		}()

		helpers.WaitForPodsRunning(ctx, t, client, "immediate-test", append(immediatePods, finalizerPod))

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		helpers.WaitForPodsDeleted(ctx, t, client, "immediate-test", immediatePods)

		err = helpers.DeletePod(ctx, client, "immediate-test", finalizerPod)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			var p v1.Pod
			err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p)
			if err != nil {
				return false
			}
			return p.DeletionTimestamp != nil
		}, helpers.WaitTimeout, helpers.WaitInterval)

		require.Never(t, func() bool {
			var p v1.Pod
			err := client.Resources().Get(ctx, finalizerPod, "immediate-test", &p)
			return err != nil
		}, 30*time.Second, 5*time.Second)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		return ctx
	})

	feature.Assess("allowCompletion mode allows pods to complete", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		t.Log("Waiting for node to be uncordoned after healthy event")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return !node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		allowCompletionPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "allowcompletion-test")
		helpers.WaitForPodsRunning(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying allowCompletion pod NOT evicted")
		helpers.AssertPodsNeverDeleted(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		t.Log("Manually deleting pod to complete drain")
		helpers.DeletePodsByNames(ctx, t, client, "allowcompletion-test", allowCompletionPods)
		helpers.WaitForPodsDeleted(ctx, t, client, "allowcompletion-test", allowCompletionPods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		return ctx
	})

	feature.Assess("deleteAfterTimeout mode force deletes after timeout", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)

		t.Log("Waiting for node to be uncordoned after healthy event")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return !node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		deleteTimeoutPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "delete-timeout-test")
		helpers.WaitForPodsRunning(ctx, t, client, "delete-timeout-test", deleteTimeoutPods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying pod NOT immediately deleted (checking first 30 seconds)")
		require.Never(t, func() bool {
			for _, podName := range deleteTimeoutPods {
				pod := &v1.Pod{}
				err := client.Resources().Get(ctx, podName, "delete-timeout-test", pod)
				if err != nil {
					t.Logf("Pod %s was deleted too early", podName)
					return true
				}
			}
			return false
		}, 15*time.Second, 5*time.Second, "pods should not be deleted immediately")

		t.Log("Waiting for deleteAfterTimeout (1min + buffer)")
		// This sleep is intentional - we're waiting for the timeout to trigger
		time.Sleep(1 * time.Minute)

		t.Log("Verifying pod force deleted after timeout")
		helpers.WaitForPodsDeleted(ctx, t, client, "delete-timeout-test", deleteTimeoutPods)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		return ctx
	})

	feature.Assess("namespace exclusion protects system namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, false)

		kubeSystemPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "kube-system")
		immediatePods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, "immediate-test")

		helpers.WaitForPodsRunning(ctx, t, client, "kube-system", kubeSystemPods)
		helpers.WaitForPodsRunning(ctx, t, client, "immediate-test", immediatePods)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		helpers.WaitForPodsDeleted(ctx, t, client, "immediate-test", immediatePods)

		helpers.AssertPodsNeverDeleted(ctx, t, client, "kube-system", kubeSystemPods)

		helpers.DeletePodsByNames(ctx, t, client, "kube-system", kubeSystemPods)

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

func TestNodeDrainerRestart(t *testing.T) {
	feature := features.New("TestNodeDrainerRestart").
		WithLabel("suite", "node-drainer-advanced")

	var testCtx *helpers.NodeDrainerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "allowcompletion-test")
		return newCtx
	})

	feature.Assess("drainer resumes drain after restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Draining started (allowCompletion mode - pods won't evict yet)")
		t.Log("Immediately restarting node-drainer deployment")
		restartTime := time.Now()
		err = helpers.RestartDeployment(ctx, t, client, "nvsentinel-node-drainer", helpers.NVSentinelNamespace)
		require.NoError(t, err)

		t.Log("Checking for new node events after restart (proves drainer picked up work)")
		require.Eventually(t, func() bool {
			found, event := helpers.CheckNodeEventExists(ctx, client, testCtx.NodeName, "NodeDraining", "", restartTime)
			if found {
				t.Logf("Found event created/updated after restart: %s (FirstTimestamp: %v, LastTimestamp: %v, RestartTime: %v)",
					event.Reason, event.FirstTimestamp.Time, event.LastTimestamp.Time, restartTime)
			}
			return found
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Manually completing drain by deleting pods")
		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, podNames)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownNodeDrainer(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
