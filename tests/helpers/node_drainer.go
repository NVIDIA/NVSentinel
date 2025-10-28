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
	backupPath, err := BackupConfigMap(ctx, client, "node-drainer-config", NVSentinelNamespace)
	require.NoError(t, err)
	t.Logf("Backup created at: %s", backupPath)
	testCtx.ConfigMapBackupPath = backupPath

	t.Logf("Applying test configmap: %s", configMapPath)
	err = createConfigMapFromFilePath(ctx, client, configMapPath, "node-drainer-config", NVSentinelNamespace)
	require.NoError(t, err)

	t.Log("Restarting node-drainer deployment")
	err = RestartDeployment(ctx, t, client, "nvsentinel-node-drainer", NVSentinelNamespace)
	require.NoError(t, err)

	nodeName := SelectTestNodeFromUnusedPool(ctx, t, client)

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
	DeleteNamespace(ctx, t, client, testNamespace)

	t.Logf("Cleaning up node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	node, err := GetNodeByName(ctx, client, nodeName)
	if err == nil && node.Spec.Unschedulable {
		t.Log("Manually uncordoning node")
		node.Spec.Unschedulable = false
		client.Resources().Update(ctx, node)
	}

	backupPathVal := ctx.Value(NDKeyConfigMapBackupPath)
	if backupPathVal != nil {
		backupPath := backupPathVal.(string)
		t.Logf("Restoring configmap from: %s", backupPath)
		err = createConfigMapFromFilePath(ctx, client, backupPath, "node-drainer-config", NVSentinelNamespace)
		assert.NoError(t, err)

		os.Remove(backupPath)
	}

	t.Log("Restarting node-drainer deployment")
	err = RestartDeployment(ctx, t, client, "nvsentinel-node-drainer", NVSentinelNamespace)
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
