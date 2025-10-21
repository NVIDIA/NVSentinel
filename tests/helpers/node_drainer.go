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

package helpers

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"
)

type NodeDrainerTestContextKey int

const (
	NDKeyNodeName NodeDrainerTestContextKey = iota
	NDKeyConfigMapBackupPath
	NDKeyTestNamespace
)

type NodeDrainerTestContext struct {
	NodeName            string
	ConfigMapBackupPath string
	TestNamespace       string
}

const (
	NVSentinelStateLabelKey  = "dgxc.nvidia.com/nvsentinel-state"
	DrainingLabelValue       = "draining"
	DrainSucceededLabelValue = "drain-succeeded"
)

func SetupNodeDrainerTest(ctx context.Context, t *testing.T, c *envconf.Config, configMapPath, testNamespace string) (context.Context, *NodeDrainerTestContext) {
	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &NodeDrainerTestContext{
		TestNamespace: testNamespace,
	}

	t.Log("Backing up current node-drainer configmap")
	backupPath, err := BackupConfigMap(ctx, client, "node-drainer-config", "nvsentinel")
	require.NoError(t, err)
	t.Logf("Backup created at: %s", backupPath)
	testCtx.ConfigMapBackupPath = backupPath

	t.Logf("Applying test configmap: %s", configMapPath)
	err = CreateConfigMapFromFilePath(ctx, client, configMapPath, "node-drainer-config", "nvsentinel")
	require.NoError(t, err)

	t.Log("Restarting node-drainer deployment")
	err = RestartDeployment(ctx, client, "nvsentinel-node-drainer", "nvsentinel")
	require.NoError(t, err)

	t.Log("Selecting test node")
	nodes, err := GetAllNodesNames(ctx, client)
	require.NoError(t, err)
	require.NotEmpty(t, nodes)

	startIdx := int(float64(len(nodes)) * 0.50)
	if startIdx >= len(nodes) {
		startIdx = len(nodes) - 1
	}
	unusedNodes := nodes[startIdx:]

	var nodeName string
	for _, name := range unusedNodes {
		node, err := GetNodeByName(ctx, client, name)
		if err != nil {
			continue
		}
		if !node.Spec.Unschedulable {
			nodeName = name
			break
		}
	}
	if nodeName == "" {
		nodeName = unusedNodes[0]
	}
	t.Logf("Selected node: %s", nodeName)

	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, NDKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, NDKeyConfigMapBackupPath, testCtx.ConfigMapBackupPath)
	ctx = context.WithValue(ctx, NDKeyTestNamespace, testNamespace)

	t.Logf("Creating test namespace: %s", testNamespace)
	err = CreateNamespace(ctx, client, testNamespace)
	require.NoError(t, err)

	return ctx, testCtx
}

func TeardownNodeDrainerTest(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	client, err := c.NewClient()
	require.NoError(t, err)

	nodeName := ctx.Value(NDKeyNodeName).(string)
	testNamespace := ctx.Value(NDKeyTestNamespace).(string)

	t.Logf("Cleaning up test namespace: %s", testNamespace)
	DeleteNamespace(ctx, t, client, testNamespace)

	t.Logf("Cleaning up node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	node, err := GetNodeByName(ctx, client, nodeName)
	if err == nil && node.Spec.Unschedulable {
		t.Log("Manually uncordoning node")
		node.Spec.Unschedulable = false
		client.Resources().Update(ctx, node)
	}

	backupPath := ctx.Value(NDKeyConfigMapBackupPath).(string)
	t.Logf("Restoring configmap from: %s", backupPath)
	err = CreateConfigMapFromFilePath(ctx, client, backupPath, "node-drainer-config", "nvsentinel")
	assert.NoError(t, err)

	os.Remove(backupPath)

	t.Log("Restarting node-drainer deployment")
	err = RestartDeployment(ctx, client, "nvsentinel-node-drainer", "nvsentinel")
	assert.NoError(t, err)

	return ctx
}

func CreatePodsFromTemplate(ctx context.Context, t *testing.T, client klient.Client, templatePath, nodeName, namespace string) []string {
	t.Logf("Creating pod from template: %s on node %s in namespace %s", templatePath, nodeName, namespace)

	content, err := os.ReadFile(templatePath)
	require.NoError(t, err)

	contentStr := strings.ReplaceAll(string(content), "NODE_NAME", nodeName)
	contentStr = strings.ReplaceAll(contentStr, "test-namespace", namespace)

	var pod v1.Pod
	err = yaml.Unmarshal([]byte(contentStr), &pod)
	require.NoError(t, err)

	pod.Namespace = namespace
	err = client.Resources().Create(ctx, &pod)
	require.NoError(t, err)

	t.Logf("Created pod: %s", pod.Name)
	return []string{pod.Name}
}

func WaitForNodeLabel(ctx context.Context, t *testing.T, client klient.Client, nodeName, labelKey, expectedValue string) {
	t.Logf("Waiting for node %s to have label %s=%s", nodeName, labelKey, expectedValue)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}
		if node.Labels == nil {
			return false
		}
		value, exists := node.Labels[labelKey]
		if !exists {
			return false
		}
		return value == expectedValue
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s has label %s=%s", nodeName, labelKey, expectedValue)
}

func WaitForPodsDeleted(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Logf("Waiting for %d pods to be deleted from namespace %s", len(podNames), namespace)
	require.Eventually(t, func() bool {
		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, namespace, pod)
			if err == nil {
				t.Logf("Pod %s still exists", podName)
				return false
			}
		}
		return true
	}, WaitTimeout, WaitInterval)
	t.Logf("All pods deleted from namespace %s", namespace)
}

func WaitForPodsRunning(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Logf("Waiting for %d pods to be running in namespace %s", len(podNames), namespace)
	for _, podName := range podNames {
		require.Eventually(t, func() bool {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, namespace, pod)
			if err != nil {
				return false
			}
			return pod.Status.Phase == v1.PodRunning
		}, WaitTimeout, WaitInterval)
	}
	t.Logf("All %d pods running", len(podNames))
}

func DeletePodsByNames(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Logf("Deleting %d pods from namespace %s", len(podNames), namespace)
	for _, podName := range podNames {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
			},
		}
		client.Resources().Delete(ctx, pod)
	}
}

func WaitForNodeDrainEvent(ctx context.Context, t *testing.T, client klient.Client, nodeName, eventType, eventReason string) {
	t.Logf("Waiting for node event: type=%s, reason=%s on node %s", eventType, eventReason, nodeName)
	require.Eventually(t, func() bool {
		events := &v1.EventList{}
		err := client.Resources().List(ctx, events)
		if err != nil {
			return false
		}

		for _, event := range events.Items {
			if event.InvolvedObject.Kind == "Node" &&
				event.InvolvedObject.Name == nodeName &&
				event.Type == eventType &&
				event.Reason == eventReason {
				t.Logf("Found node event: %s/%s - %s", eventType, eventReason, event.Message)
				return true
			}
		}
		return false
	}, WaitTimeout, WaitInterval)
}
