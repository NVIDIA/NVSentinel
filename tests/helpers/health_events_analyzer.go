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
	"math/rand"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	ERRORCODE_13                 = "13"
	ERRORCODE_48                 = "48"
	ERRORCODE_31                 = "31"
	ERRORCODE_119                = "119"
	ERRORCODE_120                = "120"
	ERRORCODE_79                 = "79"
	HEALTH_EVENTS_ANALYZER_AGENT = "health-events-analyzer"
)

type HealthEventsAnalyzerTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TestNamespace   string
}

func SetupHealthEventsAnalyzerTest(ctx context.Context,
	t *testing.T,
	c *envconf.Config,
	configMapPath, testNamespace string) (
	context.Context, *HealthEventsAnalyzerTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	gpuNodes, err := GetAllNodesNames(ctx, client)
	require.NoError(t, err, "failed to get nodes")
	require.True(t, len(gpuNodes) > 0, "no gpu nodes found")

	gpuNodeName := gpuNodes[rand.Intn(len(gpuNodes))] // #nosec G404 - weak random acceptable for test node selection

	testCtx := &HealthEventsAnalyzerTestContext{
		TestNamespace: testNamespace,
		NodeName:      gpuNodeName,
	}

	t.Logf("Cleaning up any existing node conditions for node %s", testCtx.NodeName)
	// Note: Using default agent (gpu-health-monitor) instead of health-events-analyzer
	// because the reconciler filters out events from health-events-analyzer to prevent infinite loops
	event := NewHealthEvent(testCtx.NodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("MultipleRemediations")
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(testCtx.NodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXidError")
	SendHealthEvent(ctx, t, event)

	t.Log("Backing up current health-events-analyzer configmap")

	backupData, err := BackupConfigMap(ctx, client, "health-events-analyzer-config", NVSentinelNamespace)
	require.NoError(t, err)
	t.Log("Backup created in memory")

	testCtx.ConfigMapBackup = backupData

	err = applyHealthEventsAnalyzerConfigAndRestart(ctx, t, client, configMapPath)
	require.NoError(t, err)

	return ctx, testCtx
}

func applyHealthEventsAnalyzerConfigAndRestart(
	ctx context.Context, t *testing.T, client klient.Client, configMapPath string,
) error {
	t.Helper()
	t.Logf("Applying health-events-analyzer configmap: %s", configMapPath)

	err := createConfigMapFromFilePath(ctx, client, configMapPath, "health-events-analyzer-config", NVSentinelNamespace)
	if err != nil {
		return err
	}

	t.Log("Restarting health-events-analyzer deployment")

	err = RestartDeployment(ctx, t, client, "health-events-analyzer", NVSentinelNamespace)
	if err != nil {
		return err
	}

	return nil
}

func TriggerMultipleRemediationsCycle(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	xidsToInject := []string{ERRORCODE_13, ERRORCODE_48, ERRORCODE_13, ERRORCODE_48, ERRORCODE_13}

	// inject 5 fatal errors and let the remediation cycle finish
	t.Logf("Injecting fatal errors to node %s", nodeName)

	for _, xid := range xidsToInject {
		waitForRemediationToComplete(ctx, t, client, nodeName, xid)
	}
}

func waitForRemediationToComplete(ctx context.Context, t *testing.T, client klient.Client, nodeName, xid string) {
	event := NewHealthEvent(nodeName).
		WithErrorCode(xid).
		WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
	SendHealthEvent(ctx, t, event)

	rebootNodeCR := WaitForRebootNodeCR(ctx, t, client, nodeName)
	require.NotNil(t, rebootNodeCR, "RebootNode CR should be created for XID error")

	err := DeleteRebootNodeCR(ctx, client, rebootNodeCR)
	require.NoError(t, err, "failed to delete RebootNode CR")

	SendHealthyEvent(ctx, t, nodeName)

	t.Logf("Waiting for node %s to be fully uncordoned and cleaned up", nodeName)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}

		if node.Spec.Unschedulable {
			return false
		}

		if node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				return false
			}

			if _, exists := node.Annotations["latestFaultRemediationState"]; exists {
				return false
			}
		}

		return true
	}, EventuallyWaitTimeout, WaitInterval, "node should be fully cleaned up before next remediation cycle")
}

func TeardownHealthEventsAnalyzer(ctx context.Context, t *testing.T,
	c *envconf.Config, nodeName string, configMapBackup []byte, xid string) context.Context {
	t.Logf("Starting cleanup for node %s", nodeName)

	// Note: Using default agent (gpu-health-monitor) instead of health-events-analyzer
	// because the reconciler filters out events from health-events-analyzer to prevent infinite loops
	event := NewHealthEvent(nodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithCheckName("MultipleRemediations").
		WithErrorCode(xid)

	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithCheckName("RepeatedXidError")
	SendHealthEvent(ctx, t, event)

	SendHealthyEvent(ctx, t, nodeName)

	restoreHealthEventsAnalyzerConfig(ctx, t, c, configMapBackup)

	return ctx
}

// restoreHealthEventsAnalyzerConfig restores the health-events-analyzer config from backup and restarts the deployment.
func restoreHealthEventsAnalyzerConfig(ctx context.Context, t *testing.T, c *envconf.Config, configMapBackup []byte) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	t.Log("Restoring configmap from memory")

	err = createConfigMapFromBytes(ctx, client, configMapBackup, "health-events-analyzer-config", NVSentinelNamespace)
	require.NoError(t, err)

	err = RestartDeployment(ctx, t, client, "health-events-analyzer", NVSentinelNamespace)
	require.NoError(t, err)
}
