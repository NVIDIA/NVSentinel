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

package helpers

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

type NodeDrainerTestContextKey int

const (
	NDKeyNodeName NodeDrainerTestContextKey = iota
	NDKeyConfigMapBackup
	NDKeyTestNamespace
)

type NodeDrainerTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TestNamespace   string
}

const (
	DrainingLabelValue       = "draining"
	DrainSucceededLabelValue = "drain-succeeded"
)

// applyNodeDrainerConfigAndRestart applies a node-drainer configmap and restarts the deployment.
func applyNodeDrainerConfigAndRestart(
	ctx context.Context, t *testing.T, client klient.Client, configMapPath string,
) error {
	t.Helper()
	t.Logf("Applying node-drainer configmap: %s", configMapPath)

	err := createConfigMapFromFilePath(ctx, client, configMapPath, "node-drainer", NVSentinelNamespace)
	if err != nil {
		return err
	}

	t.Log("Restarting node-drainer deployment")
	err = RestartDeployment(ctx, t, client, "node-drainer", NVSentinelNamespace)

	return err
}

func SetupNodeDrainerTest(
	ctx context.Context, t *testing.T, c *envconf.Config, configMapPath, testNamespace string,
) (context.Context, *NodeDrainerTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &NodeDrainerTestContext{
		TestNamespace: testNamespace,
	}

	t.Log("Backing up current node-drainer configmap")

	backupData, err := BackupConfigMap(ctx, client, "node-drainer", NVSentinelNamespace)
	require.NoError(t, err)
	t.Log("Backup created in memory")

	testCtx.ConfigMapBackup = backupData

	err = applyNodeDrainerConfigAndRestart(ctx, t, client, configMapPath)
	require.NoError(t, err)

	nodeName := SelectTestNodeFromUnusedPool(ctx, t, client)

	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, NDKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, NDKeyConfigMapBackup, testCtx.ConfigMapBackup)
	ctx = context.WithValue(ctx, NDKeyTestNamespace, testNamespace)

	t.Logf("Creating test namespace: %s", testNamespace)
	err = CreateNamespace(ctx, client, testNamespace)
	require.NoError(t, err)

	return ctx, testCtx
}

func ResetNodeAndTriggerDrain(
	ctx context.Context, t *testing.T, client klient.Client, nodeName, namespace string,
) []string {
	t.Helper()

	SendHealthyEvent(ctx, t, nodeName)
	WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

	podNames := CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", nodeName, namespace)
	WaitForPodsRunning(ctx, t, client, namespace, podNames)

	event := NewHealthEvent(nodeName).
		WithErrorCode("79").
		WithMessage("GPU Fallen off the bus")
	SendHealthEvent(ctx, t, event)

	WaitForNodeLabel(ctx, t, client, nodeName, statemanager.NVSentinelStateLabelKey, DrainingLabelValue)

	return podNames
}

func TeardownNodeDrainer(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	nodeNameVal := ctx.Value(NDKeyNodeName)
	if nodeNameVal == nil {
		t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
		return ctx
	}

	nodeName := nodeNameVal.(string)

	testNamespaceVal := ctx.Value(NDKeyTestNamespace)

	testNamespace := ""
	if testNamespaceVal != nil {
		testNamespace = testNamespaceVal.(string)
	}

	t.Logf("Cleaning up test namespace: %s", testNamespace)

	if err := DeleteNamespace(ctx, t, client, testNamespace); err != nil {
		t.Logf("Failed to delete test namespace %s: %v", testNamespace, err)
	}

	t.Logf("Cleaning up node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return err
		}

		if node.Spec.Unschedulable {
			t.Log("Manually uncordoning node")

			node.Spec.Unschedulable = false

			return client.Resources().Update(ctx, node)
		}

		return nil
	})
	if err != nil {
		require.Fail(t, "failed to uncordon node", "nodeName", nodeName)
	}

	backupDataVal := ctx.Value(NDKeyConfigMapBackup)
	if backupDataVal != nil {
		backupData := backupDataVal.([]byte)

		t.Log("Restoring node-drainer configmap from memory")

		err = createConfigMapFromBytes(ctx, client, backupData, "node-drainer", NVSentinelNamespace)
		if err == nil {
			err = RestartDeployment(ctx, t, client, "node-drainer", NVSentinelNamespace)
		}

		assert.NoError(t, err)
	}

	return ctx
}

func CreatePodsFromTemplate(
	ctx context.Context, t *testing.T, client klient.Client, templatePath, nodeName, namespace string,
) []string {
	t.Helper()
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

// ApplyNodeDrainerConfig backs up the current node-drainer config, applies the test config,
// and restarts the node-drainer deployment. Returns context with backup data stored.
func ApplyNodeDrainerConfig(
	ctx context.Context, t *testing.T, c *envconf.Config, configMapPath string,
) context.Context {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	t.Log("Backing up current node-drainer configmap")

	backupData, err := BackupConfigMap(ctx, client, "node-drainer", NVSentinelNamespace)
	require.NoError(t, err)
	t.Log("Backup created in memory")

	err = applyNodeDrainerConfigAndRestart(ctx, t, client, configMapPath)
	require.NoError(t, err)

	return context.WithValue(ctx, FRKeyNodeDrainerConfigMapBackup, backupData)
}

// RestoreNodeDrainerConfig restores the node-drainer config from backup and restarts the deployment.
func RestoreNodeDrainerConfig(ctx context.Context, t *testing.T, c *envconf.Config) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	backupDataVal := ctx.Value(FRKeyNodeDrainerConfigMapBackup)
	if backupDataVal == nil {
		t.Log("No node-drainer configmap backup to restore")
		return
	}

	backupData := backupDataVal.([]byte)

	t.Log("Restoring node-drainer configmap from memory")

	err = createConfigMapFromBytes(ctx, client, backupData, "node-drainer", NVSentinelNamespace)
	require.NoError(t, err)

	err = RestartDeployment(ctx, t, client, "node-drainer", NVSentinelNamespace)
	require.NoError(t, err)
}
