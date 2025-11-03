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
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestDryRunMode(t *testing.T) {
	feature := features.New("TestDryRunMode").
		WithLabel("suite", "fault-quarantine-special-modes")

	var testCtx *helpers.QuarantineTestContext
	var originalDeployment *appsv1.Deployment
	var podNames []string
	testNamespace := "immediate-test"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		dryRunEnabled := true
		var newCtx context.Context
		newCtx, testCtx, originalDeployment = helpers.SetupQuarantineTestWithOptions(ctx, t, c,
			"data/basic-matching-configmap.yaml",
			&helpers.QuarantineSetupOptions{
				DryRun: &dryRunEnabled,
			})

		t.Logf("Creating test namespace: %s", testNamespace)
		err = helpers.CreateNamespace(newCtx, client, testNamespace)
		require.NoError(t, err)

		podNames = helpers.CreatePodsFromTemplate(newCtx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testNamespace)
		helpers.WaitForPodsRunning(newCtx, t, client, testNamespace, podNames)

		return newCtx
	})

	// Taints should not be applied in dry-run mode
	// TODO: Verify the same after https://github.com/NVIDIA/NVSentinel/issues/193 is fixed
	feature.Assess("taints applied in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			for _, taint := range node.Spec.Taints {
				if taint.Key == "AggregatedNodeHealth" &&
					taint.Value == "False" &&
					taint.Effect == v1.TaintEffectNoSchedule {
					return true
				}
			}
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		return ctx
	})

	feature.Assess("annotations set in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.NotNil(t, node.Annotations)

		_, exists := node.Annotations["quarantineHealthEvent"]
		assert.True(t, exists)

		return ctx
	})

	feature.Assess("node NOT cordoned in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.AssertNodeNeverQuarantined(ctx, t, client, testCtx.NodeName, false)

		return ctx
	})

	feature.Assess("immediate mode pods NOT drained in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Verifying immediate mode pods never get drained in dry-run mode")
		helpers.AssertPodsNeverDeleted(ctx, t, client, testNamespace, podNames)

		return ctx
	})

	feature.Assess("RebootNode CR NOT created in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Verifying no RebootNode CR is created in dry-run mode")
		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		if testNamespace != "" {
			t.Logf("Cleaning up test namespace: %s", testNamespace)
			helpers.DeleteNamespace(ctx, t, client, testNamespace)
		}

		if originalDeployment != nil {
			t.Log("Restoring original deployment (disabling dry-run mode)")
			helpers.RestoreFQDeployment(ctx, t, client, originalDeployment)
		}

		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())

}

func TestNodeDeletedDuringDrain(t *testing.T) {
	feature := features.New("TestNodeDeletedDuringDrain").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext
	var podNames []string
	var nodeBackup *v1.Node
	testNamespace := "delete-timeout-test"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, testNamespace)
		newCtx = helpers.ApplyNodeDrainerConfig(newCtx, t, c, "data/nd-all-modes.yaml")

		return newCtx
	})

	feature.Assess("create pod in deleteAfterTimeout namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames = helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testNamespace, podNames)

		return ctx
	})

	feature.Assess("trigger drain with fatal health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		helpers.SendHealthEvent(ctx, t, fatalEvent)

		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)

		return ctx
	})

	feature.Assess("wait for draining state", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		return ctx
	})

	feature.Assess("delete node from cluster", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		nodeBackup = node.DeepCopy()
		nodeBackup.ResourceVersion = ""
		nodeBackup.UID = ""

		err = client.Resources().Delete(ctx, node)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			_, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			return err != nil
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "node should be deleted")

		return ctx
	})

	feature.Assess("verify no RebootNode CR created after timeout", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting beyond deleteAfterTimeout duration (1min + buffer)")
		time.Sleep(1*time.Minute + 20*time.Second)

		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		if nodeBackup != nil {
			err = client.Resources().Create(ctx, nodeBackup)
			if err != nil {
				t.Logf("Warning: Failed to recreate node from backup: %v", err)
			} else {
				require.Eventually(t, func() bool {
					_, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
					return err == nil
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "recreated node should exist")
			}
		}

		helpers.RestoreNodeDrainerConfig(ctx, t, c)

		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestNodeRecoveryDuringDrain(t *testing.T) {
	feature := features.New("TestNodeRecoveryDuringDrain").
		WithLabel("suite", "node-drainer-advanced")

	var testCtx *helpers.NodeDrainerTestContext
	var podNames []string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "delete-timeout-test")
		return newCtx
	})

	feature.Assess("create pods and trigger drain", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames = helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus")
		helpers.SendHealthEvent(ctx, t, event)

		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		return ctx
	})

	feature.Assess("wait for deleteAfterTimeout timer to start", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			found, _ := helpers.CheckNodeEventExists(ctx, client, testCtx.NodeName, "NodeDraining", "WaitingBeforeForceDelete", time.Time{})
			return found
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "WaitingBeforeForceDelete event should be created")

		return ctx
	})

	feature.Assess("send healthy event before deleteAfterTimeout expires", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthyEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("XID 79 cleared during drain")
		helpers.SendHealthEvent(ctx, t, healthyEvent)

		return ctx
	})

	feature.Assess("node gets uncordoned after healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, false)

		return ctx
	})

	feature.Assess("pods remain after drain abort", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, testCtx.TestNamespace, pod)
			require.NoError(t, err, "pod %s should still exist after drain abort", podName)
		}

		return ctx
	})

	feature.Assess("drain label cleared after recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		labelValue, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
		t.Logf("Node label after recovery: exists=%v, value=%s", exists, labelValue)

		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownNodeDrainer(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
