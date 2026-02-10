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

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type k8sObjectMonitorContextKey int

const (
	k8sMonitorKeyNodeName     k8sObjectMonitorContextKey = iota
	k8sMonitorKeyOriginalArgs k8sObjectMonitorContextKey = iota

	testConditionType        = "TestCondition"
	gpuOperatorNamespace     = "gpu-operator"
	gpuOperatorPodPolicyName = "gpu-operator-pod-health"
)

func TestKubernetesObjectMonitor(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor - Node Not Ready Detection").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "node-monitoring")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", testNodeName)

		return context.WithValue(ctx, k8sMonitorKeyNodeName, testNodeName)
	})

	feature.Assess("Node NotReady triggers health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(k8sMonitorKeyNodeName).(string)
		t.Logf("Setting TestCondition to False on node %s", nodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		t.Log("Waiting for policy match annotation on node")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}

			annotation, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]
			if !exists {
				return false
			}

			t.Logf("Found policy match annotation: %s", annotation)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		helpers.WaitForNodeEvent(ctx, t, client, nodeName, v1.Event{
			Type:   "node-test-condition",
			Reason: "node-test-conditionIsNotHealthy",
		})

		return ctx
	})

	feature.Assess("Node Ready recovery clears annotation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(k8sMonitorKeyNodeName).(string)
		t.Logf("Setting TestCondition to True on node %s", nodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)

		t.Log("Waiting for policy match annotation to be cleared")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}

			annotation, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]
			if exists && annotation != "" {
				t.Logf("Annotation still exists: %s", annotation)
				return false
			}

			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestKubernetesObjectMonitorWithStoreOnlyStrategy(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor with STORE_ONLY strategy - Node Not Ready Detection").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "node-monitoring")

	var testCtx *helpers.KubernetesObjectMonitorTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", testNodeName)

		err = helpers.DeleteExistingNodeEvents(ctx, t, client, testNodeName, "node-test-condition", "node-test-conditionIsNotHealthy")
		require.NoError(t, err)

		originalArgs, err := helpers.SetDeploymentArgs(ctx, t, client, helpers.K8S_DEPLOYMENT_NAME, helpers.NVSentinelNamespace, helpers.K8S_CONTAINER_NAME, map[string]string{
			"--processing-strategy": "STORE_ONLY",
		})
		require.NoError(t, err)

		testCtx = &helpers.KubernetesObjectMonitorTestContext{
			NodeName: testNodeName,
		}

		ctx = context.WithValue(ctx, k8sMonitorKeyOriginalArgs, originalArgs)

		helpers.WaitForDeploymentRollout(ctx, t, client, helpers.K8S_DEPLOYMENT_NAME, helpers.NVSentinelNamespace)

		return context.WithValue(ctx, k8sMonitorKeyNodeName, testNodeName)
	})

	feature.Assess("Node NotReady triggers health event with STORE_ONLY strategy", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(k8sMonitorKeyNodeName).(string)
		t.Logf("Setting TestCondition to False on node %s", nodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		t.Log("Waiting for policy match annotation on node")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}

			annotation, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]
			if !exists {
				return false
			}

			t.Logf("Found policy match annotation: %s", annotation)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		t.Log("Check node event is not created")
		helpers.EnsureNodeEventNotPresent(ctx, t, client, nodeName, "node-test-condition", "node-test-conditionIsNotHealthy")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		originalArgs := ctx.Value(k8sMonitorKeyOriginalArgs).([]string)
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Setting TestCondition to True on node %s", testCtx.NodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, testCtx.NodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)

		helpers.TeardownKubernetesObjectMonitor(ctx, t, c, testCtx.ConfigMapBackup, originalArgs)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestKubernetesObjectMonitorWithRuleOverride(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor with Rule Override for processingStrategy=STORE_ONLY - Node Not Ready Detection").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "node-monitoring")

	var testCtx *helpers.KubernetesObjectMonitorTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", testNodeName)

		err = helpers.DeleteExistingNodeEvents(ctx, t, client, testNodeName, "node-test-condition", "node-test-conditionIsNotHealthy")
		require.NoError(t, err)

		t.Log("Backing up current configmap")

		backupData, err := helpers.BackupConfigMap(ctx, client, "kubernetes-object-monitor", helpers.NVSentinelNamespace)
		require.NoError(t, err)
		t.Log("Backup created in memory")

		testCtx = &helpers.KubernetesObjectMonitorTestContext{
			NodeName:        testNodeName,
			ConfigMapBackup: backupData,
		}

		helpers.UpdateKubernetesObjectMonitorConfigMap(ctx, t, client, "data/k8s-rule-override.yaml", "kubernetes-object-monitor")

		return context.WithValue(ctx, k8sMonitorKeyNodeName, testNodeName)
	})

	feature.Assess("Node NotReady triggers health event with STORE_ONLY strategy", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(k8sMonitorKeyNodeName).(string)
		t.Logf("Setting TestCondition to False on node %s", nodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		t.Log("Waiting for policy match annotation on node")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				t.Logf("Failed to get node: %v", err)
				return false
			}

			annotation, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]
			if !exists {
				return false
			}

			t.Logf("Found policy match annotation: %s", annotation)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		t.Log("Check node event is not created")
		helpers.EnsureNodeEventNotPresent(ctx, t, client, nodeName, "node-test-condition", "node-test-conditionIsNotHealthy")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Setting TestCondition to True on node %s", testCtx.NodeName)

		helpers.SetNodeConditionStatus(ctx, t, client, testCtx.NodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)

		t.Log("Restoring kubernetes-object-monitor state")

		helpers.TeardownKubernetesObjectMonitor(ctx, t, c, testCtx.ConfigMapBackup, nil)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// daemonSetTestContext holds context for DaemonSet pod health tests
type daemonSetTestContext struct {
	NodeName       string
	DaemonSetNames []string
	Namespace      string
}

// TestKubernetesObjectMonitorDaemonSetPodHealth tests DaemonSet pod health monitoring.
// Scenarios:
// 1. Init container blocking (sleep) - pod stuck in Pending/Init state
// 2. Multi-pod tracking - partial recovery keeps node cordoned
// 3. DaemonSet deletion uncordons node
// 4. Pod deletion with unhealthy replacement re-cordons node
// 5. Init container CrashLoopBackOff detection + recovery
func TestKubernetesObjectMonitorInitContainerFailures(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor - Init Container Failures").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "init-container-failures")

	var testCtx *daemonSetTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test worker node: %s", testNodeName)

		// Ensure gpu-operator namespace exists
		err = helpers.CreateNamespace(ctx, client, gpuOperatorNamespace)
		require.NoError(t, err, "failed to create namespace %s", gpuOperatorNamespace)

		testCtx = &daemonSetTestContext{
			NodeName:       testNodeName,
			DaemonSetNames: []string{"test-ds-init-1", "test-ds-init-2"},
			Namespace:      gpuOperatorNamespace,
		}

		return ctx
	})

	// Consolidated test: Multi-DaemonSet failure, partial recovery, full recovery
	feature.Assess("Multi-DaemonSet failures with partial and full recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// --- Phase 1: Create two failing DaemonSets ---
		t.Log("=== Phase 1: Creating two failing DaemonSets ===")
		for _, dsName := range testCtx.DaemonSetNames {
			ds := helpers.BuildTestDaemonSet(dsName, testCtx.Namespace, testCtx.NodeName, helpers.DaemonSetInitBlocking)
			err = client.Resources().Create(ctx, ds)
			require.NoError(t, err)
			t.Logf("Created DaemonSet %s with blocking init container", dsName)
		}

		// Wait for both pods to be created
		require.Eventually(t, func() bool {
			pods1, _ := helpers.ListDaemonSetPods(ctx, client, testCtx.Namespace, testCtx.DaemonSetNames[0])
			pods2, _ := helpers.ListDaemonSetPods(ctx, client, testCtx.Namespace, testCtx.DaemonSetNames[1])
			if len(pods1) > 0 && len(pods2) > 0 {
				t.Logf("Both pods created: %s (phase: %s), %s (phase: %s)",
					pods1[0].Name, pods1[0].Status.Phase, pods2[0].Name, pods2[0].Status.Phase)
				return true
			}
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		// Wait for node to be cordoned
		t.Log("Waiting for node to be cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: true},
				{Key: helpers.K8sObjectMonitorAnnotationKey, Pattern: testCtx.DaemonSetNames[0], ShouldExist: true},
				{Key: helpers.K8sObjectMonitorAnnotationKey, Pattern: testCtx.DaemonSetNames[1], ShouldExist: true},
			},
		})
		t.Log("Node cordoned with both pod failures tracked")

		// --- Phase 2: First DaemonSet recovery (node stays cordoned) ---
		t.Log("=== Phase 2: First DaemonSet recovery ===")
		t.Logf("Updating %s to healthy", testCtx.DaemonSetNames[0])
		err = helpers.UpdateDaemonSetToHealthy(ctx, client, testCtx.Namespace, testCtx.DaemonSetNames[0])
		require.NoError(t, err)

		helpers.WaitForDaemonSetPodRunning(ctx, t, client, testCtx.Namespace, testCtx.DaemonSetNames[0], testCtx.NodeName)

		// Node should STILL be cordoned
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.K8sObjectMonitorAnnotationKey, Pattern: testCtx.DaemonSetNames[0], ShouldExist: false},
				{Key: helpers.K8sObjectMonitorAnnotationKey, Pattern: testCtx.DaemonSetNames[1], ShouldExist: true},
			},
		})
		t.Log("Node remains cordoned - second DaemonSet still unhealthy")

		// --- Phase 3: Full recovery (node uncordoned) ---
		t.Log("=== Phase 3: Full recovery ===")
		t.Logf("Updating %s to healthy", testCtx.DaemonSetNames[1])
		err = helpers.UpdateDaemonSetToHealthy(ctx, client, testCtx.Namespace, testCtx.DaemonSetNames[1])
		require.NoError(t, err)

		helpers.WaitForDaemonSetPodRunning(ctx, t, client, testCtx.Namespace, testCtx.DaemonSetNames[1], testCtx.NodeName)

		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})
		t.Log("SUCCESS: Node uncordoned after all failures resolved")

		return ctx
	})

	// Consolidated test: DaemonSet deletion and pod replacement cycle
	feature.Assess("DaemonSet deletion and pod replacement cycle", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("=== Phase 1: DaemonSet deletion uncordons node ===")
		dsName := "test-ds-init-deletion"
		ds := helpers.BuildTestDaemonSet(dsName, testCtx.Namespace, testCtx.NodeName, helpers.DaemonSetInitBlocking)
		err = client.Resources().Create(ctx, ds)
		require.NoError(t, err)
		t.Logf("Created DaemonSet %s", dsName)

		require.Eventually(t, func() bool {
			pods, err := helpers.ListDaemonSetPods(ctx, client, testCtx.Namespace, dsName)
			return err == nil && len(pods) > 0
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		t.Log("Waiting for node to be cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)

		t.Log("Deleting DaemonSet...")
		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, dsName)
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, false)
		t.Log("Node uncordoned after DaemonSet deletion")

		// --- Phase 2: Pod deletion with unhealthy replacement re-cordons ---
		t.Log("=== Phase 2: Pod deletion with unhealthy replacement ===")
		dsName = "test-ds-init-cycle"
		ds = helpers.BuildTestDaemonSet(dsName, testCtx.Namespace, testCtx.NodeName, helpers.DaemonSetInitBlocking)
		err = client.Resources().Create(ctx, ds)
		require.NoError(t, err)
		t.Logf("Created DaemonSet %s", dsName)

		var originalPodName string
		require.Eventually(t, func() bool {
			pods, err := helpers.ListDaemonSetPods(ctx, client, testCtx.Namespace, dsName)
			if err == nil && len(pods) > 0 {
				originalPodName = pods[0].Name
				return true
			}
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		t.Log("Waiting for node to be cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)

		t.Logf("Deleting pod %s manually", originalPodName)
		err = helpers.DeletePod(ctx, t, client, testCtx.Namespace, originalPodName, true)
		require.NoError(t, err)

		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, false)
		t.Log("Node uncordoned after pod deletion")

		require.Eventually(t, func() bool {
			pods, err := helpers.ListDaemonSetPods(ctx, client, testCtx.Namespace, dsName)
			if err == nil && len(pods) > 0 && pods[0].Name != originalPodName {
				t.Logf("Replacement pod created: %s", pods[0].Name)
				return true
			}
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		t.Log("Waiting for node to be re-cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)
		t.Log("SUCCESS: Node re-cordoned due to unhealthy replacement pod")

		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, dsName)

		return ctx
	})

	// Consolidated test: Init container CrashLoopBackOff detection and recovery
	feature.Assess("Init container CrashLoopBackOff detection and recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		dsName := "test-ds-init-crashloop"

		// --- Phase 1: CrashLoopBackOff detection ---
		t.Log("=== Phase 1: Init container CrashLoopBackOff detection ===")
		ds := helpers.BuildTestDaemonSet(dsName, testCtx.Namespace, testCtx.NodeName, helpers.DaemonSetInitCrashLoop)
		err = client.Resources().Create(ctx, ds)
		require.NoError(t, err)
		t.Logf("Created DaemonSet %s with crashing init container", dsName)

		t.Log("Waiting for init container to enter CrashLoopBackOff...")
		pod := helpers.WaitForCrashLoopBackOff(ctx, t, client, testCtx.Namespace, dsName, true)
		require.NotNil(t, pod, "pod init container should be in CrashLoopBackOff")
		t.Logf("Pod %s: phase=%s (Pending because init container crashing)", pod.Name, pod.Status.Phase)
		require.Equal(t, v1.PodPending, pod.Status.Phase)

		t.Log("Waiting for node to be cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, true)
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.K8sObjectMonitorAnnotationKey, ShouldExist: true, Pattern: dsName},
			},
		})
		t.Log("Init container CrashLoopBackOff detected via 'phase != Running' check")

		// --- Phase 2: Recovery ---
		t.Log("=== Phase 2: Init container CrashLoopBackOff recovery ===")
		t.Log("Fixing the crashing init container (changing 'exit 1' to 'exit 0')...")
		err = helpers.FixCrashingInitContainer(ctx, client, testCtx.Namespace, dsName)
		require.NoError(t, err)

		helpers.WaitForDaemonSetPodRunning(ctx, t, client, testCtx.Namespace, dsName, testCtx.NodeName)
		t.Log("New pod is Running (init container completed successfully)")

		t.Log("Waiting for node to be uncordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testCtx.NodeName}, false)
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.K8sObjectMonitorAnnotationKey, ShouldExist: false},
			},
		})
		t.Log("SUCCESS: Node uncordoned after init container CrashLoopBackOff recovery")

		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, dsName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// Clean up all DaemonSets
		t.Log("Cleaning up test DaemonSets")
		for _, dsName := range testCtx.DaemonSetNames {
			helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, dsName)
		}
		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, "test-ds-init-crashloop")
		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, "test-ds-init-deletion")
		helpers.CleanupDaemonSet(ctx, t, client, testCtx.Namespace, "test-ds-init-cycle")

		// Ensure node is uncordoned
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		if err != nil {
			t.Logf("Warning: failed to get node for cleanup: %v", err)
		} else if node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			if updateErr := client.Resources().Update(ctx, node); updateErr != nil {
				t.Logf("Warning: failed to uncordon node during teardown: %v", updateErr)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestKubernetesObjectMonitorMainContainerFailures tests main container failure scenarios.
// Main container CrashLoopBackOff has phase=Running (not Pending), so the policy must check
// containerStatuses in addition to phase to detect unhealthy pods.
//
// This test validates:
// 1. Main container CrashLoopBackOff detection (phase=Running, containerStatuses check)
// 2. Recovery after fixing the container
func TestKubernetesObjectMonitorMainContainerFailures(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor - Main Container Failures").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "main-container-failures")

	var testNodeName string
	const dsName = "test-ds-crashloop"

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		testNodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test worker node: %s", testNodeName)

		// Ensure gpu-operator namespace exists
		err = helpers.CreateNamespace(ctx, client, gpuOperatorNamespace)
		require.NoError(t, err)

		return ctx
	})

	// Consolidated test: Main container CrashLoopBackOff detection and recovery
	feature.Assess("Main container CrashLoopBackOff detection and recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// --- Phase 1: CrashLoopBackOff detection ---
		t.Log("=== Phase 1: Main container CrashLoopBackOff detection ===")
		ds := helpers.BuildTestDaemonSet(dsName, gpuOperatorNamespace, testNodeName, helpers.DaemonSetMainCrashLoop)
		err = client.Resources().Create(ctx, ds)
		require.NoError(t, err)
		t.Logf("Created DaemonSet %s with crashing container", dsName)

		t.Log("Waiting for pod to enter CrashLoopBackOff state...")
		pod := helpers.WaitForCrashLoopBackOff(ctx, t, client, gpuOperatorNamespace, dsName, false)
		require.NotNil(t, pod, "pod should be in CrashLoopBackOff")

		// Verify pod phase is "Running" - this is why we need containerStatuses check
		t.Logf("Pod %s: phase=%s (Running even though container is crashing)", pod.Name, pod.Status.Phase)
		require.Equal(t, v1.PodRunning, pod.Status.Phase, "CrashLoopBackOff pod should have phase=Running")

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				t.Logf("  Container %s: ready=%v, restartCount=%d, state=Waiting (reason=%s)",
					cs.Name, cs.Ready, cs.RestartCount, cs.State.Waiting.Reason)
				require.Equal(t, "CrashLoopBackOff", cs.State.Waiting.Reason)
			}
		}

		t.Log("Waiting for node to be cordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testNodeName}, true)
		helpers.AssertQuarantineState(ctx, t, client, testNodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.K8sObjectMonitorAnnotationKey, ShouldExist: true, Pattern: dsName},
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: true},
			},
		})
		t.Log("Node cordoned - policy detected CrashLoopBackOff via containerStatuses")

		// --- Phase 2: Recovery ---
		t.Log("=== Phase 2: Main container recovery ===")
		t.Log("Fixing the crashing container (changing 'exit 1' to 'sleep 3600')...")
		err = helpers.FixCrashingContainer(ctx, client, gpuOperatorNamespace, dsName)
		require.NoError(t, err)

		t.Log("Waiting for new healthy pod...")
		helpers.WaitForDaemonSetPodRunning(ctx, t, client, gpuOperatorNamespace, dsName, testNodeName)

		// Verify the pod is actually healthy
		pods, err := helpers.ListDaemonSetPods(ctx, client, gpuOperatorNamespace, dsName)
		require.NoError(t, err)
		require.Len(t, pods, 1, "expected exactly one pod")

		healthyPod := pods[0]
		t.Logf("Pod %s after fix: phase=%s", healthyPod.Name, healthyPod.Status.Phase)
		require.Equal(t, v1.PodRunning, healthyPod.Status.Phase)

		for _, cs := range healthyPod.Status.ContainerStatuses {
			t.Logf("  Container %s: ready=%v, restartCount=%d", cs.Name, cs.Ready, cs.RestartCount)
			require.True(t, cs.Ready, "container should be ready")
			require.NotNil(t, cs.State.Running, "container should be in Running state")
		}

		t.Log("Waiting for node to be uncordoned...")
		helpers.WaitForNodesCordonState(ctx, t, client, []string{testNodeName}, false)
		helpers.AssertQuarantineState(ctx, t, client, testNodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.K8sObjectMonitorAnnotationKey, ShouldExist: false},
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})
		t.Log("SUCCESS: Node uncordoned after CrashLoopBackOff recovery")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// Uncordon node first to allow cleanup (in case test failed midway)
		node, err := helpers.GetNodeByName(ctx, client, testNodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			if updateErr := client.Resources().Update(ctx, node); updateErr != nil {
				t.Logf("Warning: failed to uncordon node: %v", updateErr)
			}
		}

		// Clean up DaemonSet
		helpers.CleanupDaemonSet(ctx, t, client, gpuOperatorNamespace, dsName)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
