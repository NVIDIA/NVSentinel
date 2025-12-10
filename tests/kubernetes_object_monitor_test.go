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

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

type k8sObjectMonitorContextKey int

const (
	k8sMonitorKeyNodeName k8sObjectMonitorContextKey = iota

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
		require.NotEmpty(t, testNodeName, "no real (non-KWOK) nodes found in cluster")
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
