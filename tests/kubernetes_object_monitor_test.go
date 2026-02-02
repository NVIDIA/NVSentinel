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

	annotationKey     = "nvsentinel.nvidia.com/k8s-object-monitor-policy-matches"
	testConditionType = "TestCondition"
)

func TestKubernetesObjectMonitor(t *testing.T) {
	feature := features.New("Kubernetes Object Monitor - Node Not Ready Detection").
		WithLabel("suite", "kubernetes-object-monitor").
		WithLabel("component", "node-monitoring")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeList := &v1.NodeList{}
		err = client.Resources().List(ctx, nodeList)
		require.NoError(t, err)

		var testNodeName string
		for _, node := range nodeList.Items {
			if node.Labels["type"] != "kwok" {
				testNodeName = node.Name
				break
			}
		}
		require.NotEmpty(t, testNodeName, "no worker node found in cluster")
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

			annotation, exists := node.Annotations[annotationKey]
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

			annotation, exists := node.Annotations[annotationKey]
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

		// Find the test node first
		nodeList := &v1.NodeList{}
		err = client.Resources().List(ctx, nodeList)
		require.NoError(t, err)

		var testNodeName string
		for _, node := range nodeList.Items {
			if node.Labels["type"] != "kwok" {
				testNodeName = node.Name
				break
			}
		}
		require.NotEmpty(t, testNodeName, "no worker node found in cluster")
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

			annotation, exists := node.Annotations[annotationKey]
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

		// Find the test node first
		nodeList := &v1.NodeList{}
		err = client.Resources().List(ctx, nodeList)
		require.NoError(t, err)

		var testNodeName string
		for _, node := range nodeList.Items {
			if node.Labels["type"] != "kwok" {
				testNodeName = node.Name
				break
			}
		}
		require.NotEmpty(t, testNodeName, "no worker node found in cluster")
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

			annotation, exists := node.Annotations[annotationKey]
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
