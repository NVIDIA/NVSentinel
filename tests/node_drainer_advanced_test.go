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
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

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

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + InProgress")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "InProgress")

		t.Log("Draining started (allowCompletion mode - pods won't evict yet)")
		t.Log("Immediately restarting node-drainer pod")
		restartTime := time.Now()
		err = helpers.RestartDeployment(ctx, client, "nvsentinel-node-drainer", "nvsentinel")
		require.NoError(t, err)

		t.Log("Waiting for deployment to be ready after restart")
		require.Eventually(t, func() bool {
			deployment := &appsv1.Deployment{}
			err := client.Resources().Get(ctx, "nvsentinel-node-drainer", "nvsentinel", deployment)
			if err != nil {
				return false
			}
			return deployment.Status.ReadyReplicas > 0 &&
				deployment.Status.UpdatedReplicas == deployment.Status.Replicas
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Checking for new node events after restart (proves drainer picked up work)")
		require.Eventually(t, func() bool {
			found, event := helpers.CheckNodeEventExistsAfterTime(ctx, client, testCtx.NodeName, "NodeDraining", restartTime)
			if found {
				t.Logf("Found event created/updated after restart: %s (FirstTimestamp: %v, LastTimestamp: %v, RestartTime: %v)",
					event.Reason, event.FirstTimestamp.Time, event.LastTimestamp.Time, restartTime)
			}
			return found
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Manually completing drain by deleting pods")
		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, podNames)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + Succeeded")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownNodeDrainerTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestNodeRecoveryDuringDrain(t *testing.T) {
	feature := features.New("TestNodeRecoveryDuringDrain").
		WithLabel("suite", "node-drainer-advanced")

	var testCtx *helpers.NodeDrainerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "allowcompletion-test")
		return newCtx
	})

	feature.Assess("healthy event during drain aborts eviction process", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Creating long-running pods in allowCompletion mode namespace")
		podNames := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Triggering drain")
		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		tempFile := helpers.SendHealthEvent(ctx, t, event)
		defer os.Remove(tempFile)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying MongoDB event status: Quarantined + InProgress")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "InProgress")

		t.Log("Node is draining, pods still running (allowCompletion mode)")

		t.Log("Sending healthy event WHILE drain is in progress")
		healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("XID 79 cleared during drain")
		tempHealthy := helpers.SendHealthEvent(ctx, t, healthyEvent)
		defer os.Remove(tempHealthy)

		t.Log("Verifying drain aborted: node uncordoned")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			if node.Spec.Unschedulable {
				t.Log("Node still cordoned, waiting for uncordon")
				return false
			}
			return true
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Verifying drain aborted: pods NOT evicted")
		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, testCtx.TestNamespace, pod)
			require.NoError(t, err, "pod %s was deleted but drain should have aborted", podName)
		}

		t.Log("Verifying MongoDB event status shows recovery")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"UnQuarantined", "Succeeded")

		t.Log("Verifying label cleared or shows recovery")
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		labelValue, exists := node.Labels[helpers.NVSentinelStateLabelKey]
		t.Logf("Node label after recovery: exists=%v, value=%s", exists, labelValue)

		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownNodeDrainerTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
