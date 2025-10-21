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
			WithMessage("XID 79 error")
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
			eventsAfter := &v1.EventList{}
			client.Resources().List(ctx, eventsAfter)

			for _, event := range eventsAfter.Items {
				if event.InvolvedObject.Kind == "Node" &&
					event.InvolvedObject.Name == testCtx.NodeName &&
					event.Type == "NodeDraining" {
					// Check if event was created or updated after restart
					if event.FirstTimestamp.After(restartTime) || event.LastTimestamp.After(restartTime) {
						t.Logf("Found event created/updated after restart: %s (FirstTimestamp: %v, LastTimestamp: %v, RestartTime: %v)",
							event.Reason, event.FirstTimestamp.Time, event.LastTimestamp.Time, restartTime)
						return true
					}
				}
			}
			return false
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
			WithMessage("XID 79 error")
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

// func TestStuckTerminatingPods(t *testing.T) {
// 	feature := features.New("TestStuckTerminatingPods").
// 		WithLabel("suite", "node-drainer-advanced")

// 	var testCtx *helpers.NodeDrainerTestContext

// 	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
// 		newCtx, tc := helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "delete-timeout-test")
// 		testCtx = tc
// 		return newCtx
// 	})

// 	feature.Assess("stuck terminating pod doesn't block drain completion", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
// 		client, err := c.NewClient()
// 		require.NoError(t, err)

// 		t.Log("Creating normal pods and one pod with finalizer (will get stuck)")
// 		normalPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
// 		stuckPods := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-with-finalizer.yaml", testCtx.NodeName, testCtx.TestNamespace)

// 		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, normalPods)
// 		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, stuckPods)

// 		t.Log("Triggering drain")
// 		event := helpers.NewHealthEvent(testCtx.NodeName).
// 			WithErrorCode("79").
// 			WithMessage("XID 79 error")
// 		tempFile := helpers.SendHealthEvent(ctx, t, event)
// 		defer os.Remove(tempFile)

// 		t.Log("Waiting for drain to start")
// 		require.Eventually(t, func() bool {
// 			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
// 			if err != nil {
// 				return false
// 			}
// 			labelValue := node.Labels[helpers.NVSentinelStateLabelKey]
// 			// Accept draining, drain-succeeded, or if any pod has DeletionTimestamp
// 			if labelValue == helpers.DrainingLabelValue || labelValue == helpers.DrainSucceededLabelValue {
// 				t.Logf("Drain started: label=%s", labelValue)
// 				return true
// 			}
// 			// Also check if eviction has started (any pod has DeletionTimestamp)
// 			pod := &v1.Pod{}
// 			for _, podName := range append(normalPods, stuckPods...) {
// 				if err := client.Resources().Get(ctx, podName, testCtx.TestNamespace, pod); err == nil {
// 					if pod.DeletionTimestamp != nil {
// 						t.Logf("Drain started: pod %s has DeletionTimestamp", podName)
// 						return true
// 					}
// 				}
// 			}
// 			return false
// 		}, helpers.WaitTimeout, helpers.WaitInterval)

// 		t.Log("Verifying stuck pod in Terminating state (check early before force-delete)")
// 		require.Eventually(t, func() bool {
// 			pod := &v1.Pod{}
// 			err := client.Resources().Get(ctx, stuckPods[0], testCtx.TestNamespace, pod)
// 			if err != nil {
// 				t.Logf("Failed to get stuck pod: %v", err)
// 				return false
// 			}
// 			if pod.DeletionTimestamp != nil {
// 				t.Logf("Stuck pod in Terminating state (has DeletionTimestamp)")
// 				return true
// 			}
// 			t.Logf("Stuck pod not yet terminating")
// 			return false
// 		}, helpers.WaitTimeout, helpers.WaitInterval)

// 		t.Log("Verifying normal pods evicted")
// 		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, normalPods)

// 		t.Log("Verifying drain completes despite stuck pod")
// 		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, helpers.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

// 		t.Log("Removing finalizer to allow pod deletion")
// 		pod := &v1.Pod{}
// 		err = client.Resources().Get(ctx, stuckPods[0], testCtx.TestNamespace, pod)
// 		require.NoError(t, err)
// 		pod.Finalizers = nil
// 		client.Resources().Update(ctx, pod)

// 		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, stuckPods)

// 		return ctx
// 	})

// 	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
// 		return helpers.TeardownNodeDrainerTest(ctx, t, c)
// 	})

// 	testEnv.Test(t, feature.Feature())
// }
