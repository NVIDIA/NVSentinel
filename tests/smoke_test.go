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
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

type testContextKey int

const (
	keyNodeName testContextKey = iota
	keyNamespace
	keyPodName
)

func TestFatalHealthEvent(t *testing.T) {
	feature := features.New("TestFatalHealthEventEndToEnd").
		WithLabel("suite", "smoke")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := "kwok-node-0"
		workloadNamespace := "workloads"
		podName := "test-gpu-pod"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		err = helpers.CreateGPUPod(ctx, client, workloadNamespace, podName, nodeName)
		assert.NoError(t, err, "failed to create GPU pod")

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		ctx = context.WithValue(ctx, keyPodName, podName)

		return ctx
	})

	feature.Assess("Can send fatal health event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		err := helpers.SendHealthEvent(nodeName, "data/fatal-health-event.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is cordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be cordoned", nodeName)
		node, err := helpers.WaitForNodeCordonState(ctx, t, client, nodeName, true)
		assert.NoError(t, err, "failed to check wait for node to be cordoned")

		assert.Equal(t, "NVSentinel", node.Labels["k8saas.nvidia.com/cordon-by"])
		assert.Equal(t, "GPU-fatal-error-ruleset", node.Labels["k8saas.nvidia.com/cordon-reason"])

		var nodeCondition *v1.NodeCondition
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == "GpuXidError" {
				nodeCondition = &node.Status.Conditions[i]
				break
			}
		}
		assert.NotNil(t, nodeCondition, "node condition GpuXidError not found")

		assert.Equal(t, "GpuXidError", string(nodeCondition.Type))
		assert.Equal(t, "True", string(nodeCondition.Status))
		assert.Equal(t, "GpuXidErrorIsNotHealthy", nodeCondition.Reason)
		assert.Equal(t, "ErrorCode:79 gpu:0 XID error occured Recommended Action=NODE_REBOOT;", nodeCondition.Message)

		return ctx
	})

	feature.Assess("Drain label is set and pods are not evicted, delete the pod to move the process forward", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)
		podName := ctx.Value(keyPodName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		_, err = helpers.WaitForNodeLabel(ctx, t, client, nodeName, "nvsentinel.dgxc.nvidia.com/node-drain-status", "IN_PROGRESS")
		assert.NoError(t, err, "failed to wait for node drain status label")

		isRunning, err := helpers.IsPodRunning(ctx, client, namespaceName, podName)
		assert.NoError(t, err, "failed to check pod status")
		assert.True(t, isRunning, "expected GPU pod to still be running (not evicted)")

		err = helpers.DeletePod(ctx, client, namespaceName, podName)
		assert.NoError(t, err, "failed to delete GPU pod")

		return ctx
	})

	feature.Assess("Remediation CR is created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		rebootNode, err := helpers.WaitForRebootNodeCR(ctx, t, client, nodeName)
		assert.NoError(t, err, "failed to wait for RebootNode CR")

		err = helpers.DeleteRebootNodeCR(ctx, client, rebootNode)
		assert.NoError(t, err, "failed to delete RebootNode CR")

		return ctx
	})

	feature.Assess("Can send healthy event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		err := helpers.SendHealthEvent(nodeName, "data/healthy-event.json")
		assert.NoError(t, err, "failed to send health event")

		return ctx
	})

	feature.Assess("Node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		t.Logf("Waiting for node %s to be uncordoned", nodeName)
		node, err := helpers.WaitForNodeCordonState(ctx, t, client, nodeName, false)
		assert.NoError(t, err, "failed to check wait for node to be uncordoned")

		assert.Equal(t, "NVSentinel", node.Labels["k8saas.nvidia.com/uncordon-by"])

		var nodeCondition *v1.NodeCondition
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == "GpuXidError" {
				nodeCondition = &node.Status.Conditions[i]
				break
			}
		}
		assert.NotNil(t, nodeCondition, "node condition GpuXidError not found")

		assert.Equal(t, "GpuXidError", string(nodeCondition.Type))
		assert.Equal(t, "False", string(nodeCondition.Status))
		assert.Equal(t, "GpuXidErrorIsHealthy", nodeCondition.Reason)
		assert.Equal(t, "No Health Failures", nodeCondition.Message)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
