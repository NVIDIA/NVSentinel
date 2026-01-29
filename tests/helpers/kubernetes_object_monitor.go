// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package helpers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	K8S_DEPLOYMENT_NAME = "kubernetes-object-monitor"
	K8S_CONTAINER_NAME  = "kubernetes-object-monitor"
)

type KubernetesObjectMonitorTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TestNamespace   string
}

func TeardownKubernetesObjectMonitor(
	ctx context.Context, t *testing.T, c *envconf.Config, configMapBackup []byte, originalArgs []string,
) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	if configMapBackup != nil {
		t.Log("Restoring configmap from memory")

		err = createConfigMapFromBytes(ctx, client, configMapBackup, "kubernetes-object-monitor", NVSentinelNamespace)
		require.NoError(t, err)

		err = RestartDeployment(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
		require.NoError(t, err)
	}

	err = RestoreDeploymentArgs(t, ctx, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace, K8S_CONTAINER_NAME, originalArgs)
	require.NoError(t, err)

	WaitForDeploymentRollout(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
}

func UpdateKubernetesObjectMonitorConfigMap(ctx context.Context, t *testing.T, client klient.Client,
	configMapPath string, configName string) {
	t.Helper()

	if configMapPath == "" {
		t.Fatalf("configMapPath is empty")
	}

	t.Logf("Updating configmap %s", configName)

	err := createConfigMapFromFilePath(ctx, client, configMapPath, configName, NVSentinelNamespace)
	require.NoError(t, err)

	t.Logf("Restarting %s deployment", K8S_DEPLOYMENT_NAME)

	err = RestartDeployment(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
	require.NoError(t, err)
}
