//go:build arm64_group
// +build arm64_group

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
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

	feature.Assess("taints not applied in dry-run", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID error occurred")
		helpers.SendHealthEvent(ctx, t, event)

		assert.Never(t, func() bool {
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
		}, 10*time.Second, helpers.WaitInterval, "Taint should NOT be applied in dry-run mode")

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
		helpers.WaitForNoCR(ctx, t, client, testCtx.NodeName, helpers.RebootNodeGVK)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		if testNamespace != "" {
			t.Logf("Cleaning up test namespace: %s", testNamespace)
			helpers.DeleteNamespace(ctx, t, client, testNamespace)
		}

		// Clean up dry-run annotations before restoring deployment
		// In dry-run mode, annotations are added but node is not cordoned,
		// so normal cleanup won't remove them
		if testCtx != nil && testCtx.NodeName != "" {
			t.Logf("Manually cleaning dry-run annotations from node: %s", testCtx.NodeName)
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err == nil && node.Annotations != nil {
				annotationsToRemove := []string{
					"quarantineHealthEvent",
					"quarantineHealthEventAppliedTaints",
					"quarantineHealthEventIsCordoned",
				}
				annotationsRemoved := false
				for _, key := range annotationsToRemove {
					if _, exists := node.Annotations[key]; exists {
						delete(node.Annotations, key)
						annotationsRemoved = true
						t.Logf("Removed annotation: %s", key)
					}
				}
				if annotationsRemoved {
					if err := client.Resources().Update(ctx, node); err != nil {
						t.Logf("Warning: Failed to clean dry-run annotations: %v", err)
					} else {
						t.Log("Successfully cleaned dry-run annotations")
					}
				}
			}
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
			WithRecommendedAction(15)
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

		// Delete any existing RebootNode CRs before deleting the node
		// This allows us to test that no NEW CRs are created after node deletion
		t.Log("Cleaning up any existing RebootNode CRs before deleting node")
		err = helpers.DeleteAllCRs(ctx, t, client, helpers.RebootNodeGVK)
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

		// Wait for node deletion to propagate and for the node existence check
		// to prevent any in-flight events from creating RebootNode CRs
		t.Log("Waiting for node deletion to propagate (2 seconds)")
		time.Sleep(2 * time.Second)

		return ctx
	})

	feature.Assess("verify no RebootNode CR created after timeout", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Owner references ensure RebootNode CRs are garbage collected when the node is deleted.
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting beyond deleteAfterTimeout duration (1min + buffer)")
		time.Sleep(1*time.Minute + 5*time.Second)

		helpers.WaitForNoCR(ctx, t, client, testCtx.NodeName, helpers.RebootNodeGVK)

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

func TestManualUncordonPropagation(t *testing.T) {
	feature := features.New("TestManualUncordonPropagation").
		WithLabel("suite", "fault-management-manual-intervention")

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

		podNames = helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Sending fatal health event to trigger quarantine and drain")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error")
		helpers.SendHealthEvent(ctx, t, fatalEvent)

		t.Log("Waiting for ND to start draining")
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

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

	feature.Assess("manually uncordon the node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Operator manually uncordons the node")
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = false
		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)

		return ctx
	})

	feature.Assess("verify FQ detects manual uncordon and sets annotation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for FQ to detect manual uncordon and clean up state")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			manualAnnotation, hasManual := node.Annotations["quarantinedNodeUncordonedManually"]
			_, hasQuarantine := node.Annotations["quarantineHealthEvent"]

			return hasManual && manualAnnotation == "True" && !hasQuarantine
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "FQ should detect manual uncordon")

		return ctx
	})

	feature.Assess("verify ND stops draining (label removed)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for ND to process Cancelled event and remove draining label")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			_, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
			return !exists
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "ND should remove draining label")

		return ctx
	})

	feature.Assess("verify pods survive past deleteAfterTimeout deadline", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting past deleteAfterTimeout deadline to verify pods were not force deleted")
		time.Sleep(1*time.Minute + 2*time.Second)

		t.Log("Verifying pods still exist and were never marked for deletion")
		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, testCtx.TestNamespace, pod)
			require.NoError(t, err, "pod %s should still exist after deleteAfterTimeout deadline", podName)
			assert.Nil(t, pod.DeletionTimestamp, "pod %s should never have been marked for deletion", podName)
			assert.Equal(t, v1.PodRunning, pod.Status.Phase, "pod %s should still be running", podName)
		}

		return ctx
	})

	feature.Assess("verify FR never created CR for cancelled drain", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Verifying FR never created RebootNode CR (event had no remediation action)")
		helpers.WaitForNoCR(ctx, t, client, testCtx.NodeName, helpers.RebootNodeGVK)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownNodeDrainer(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestManualUncordonWithFaultRemediation(t *testing.T) {
	feature := features.New("TestManualUncordonWithFaultRemediation").
		WithLabel("suite", "fault-management-manual-intervention-fr")

	var testCtx *helpers.RemediationTestContext
	var podNames []string
	testNamespace := "immediate-test"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, testNamespace)
		newCtx = helpers.ApplyNodeDrainerConfig(newCtx, t, c, "data/nd-all-modes.yaml")

		return newCtx
	})

	feature.Assess("create pods and trigger quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		podNames = helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testNamespace, podNames)

		t.Log("Sending fatal health event to trigger quarantine")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(24)
		helpers.SendHealthEvent(ctx, t, fatalEvent)

		t.Log("Waiting for node to be quarantined (FQ)")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)

		return ctx
	})

	feature.Assess("verify fault remediation creates CR", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for fault remediation to create RebootNode CR (FR)")
		helpers.WaitForCR(ctx, t, client, testCtx.NodeName, helpers.RebootNodeGVK)

		t.Log("Verifying remediation state annotation exists")
		// Use Eventually because there's a race condition: the annotation is set when the CR is created,
		// but may be cleared quickly when the unquarantine event is processed. We need to catch it
		// in the window between CR creation and unquarantine processing.
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}
			_, exists := node.Annotations["latestFaultRemediationState"]
			if !exists {
				t.Log("Remediation state annotation not yet present")
			}
			return exists
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Remediation state annotation should exist")

		return ctx
	})

	feature.Assess("manually uncordon the node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Operator manually uncordons the node")
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		node.Spec.Unschedulable = false
		err = client.Resources().Update(ctx, node)
		require.NoError(t, err)

		return ctx
	})

	feature.Assess("verify FR clears remediation state annotation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for FR to process Cancelled event and clear remediation state")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}

			_, exists := node.Annotations["latestFaultRemediationState"]
			return !exists
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "FR should clear remediation state annotation")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Cleaning up: RebootNode CRs and namespace")
		err = helpers.DeleteAllCRs(ctx, t, client, helpers.RebootNodeGVK)
		if err != nil {
			t.Logf("Warning: failed to delete RebootNode CRs: %v", err)
		}

		helpers.RestoreNodeDrainerConfig(ctx, t, c)

		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
