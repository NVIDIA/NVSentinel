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
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	ERRORCODE_13                           = "13"
	ERRORCODE_48                           = "48"
	ERRORCODE_31                           = "31"
	ERRORCODE_119                          = "119"
	ERRORCODE_120                          = "120"
	ERRORCODE_79                           = "79"
	ERRORCODE_74                           = "74"
	HEALTH_EVENTS_ANALYZER_AGENT           = "health-events-analyzer"
	SYSLOG_HEALTH_MONITOR_AGENT            = "syslog-health-monitor"
	HEALTH_EVENTS_ANALYZER_DEPLOYMENT_NAME = "health-events-analyzer"
	HEALTH_EVENTS_ANALYZER_CONTAINER_NAME  = "health-events-analyzer"
)

type HealthEventsAnalyzerTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TestNamespace   string
}

func SetupHealthEventsAnalyzerTest(ctx context.Context,
	t *testing.T,
	c *envconf.Config,
	configMapPath, testNamespace string, testNodeName string) (
	context.Context, *HealthEventsAnalyzerTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	gpuNodeName := ""

	if testNodeName != "" {
		gpuNodeName = testNodeName
	} else {
		// Use node pool to get an unused node (prevents event contamination from previous tests)
		gpuNodeName = AcquireNodeFromPool(ctx, t, client, DefaultExpiry)
	}

	testCtx := &HealthEventsAnalyzerTestContext{
		TestNamespace: testNamespace,
		NodeName:      gpuNodeName,
	}

	clearHealthEventsAnalyzerConditions(ctx, t, gpuNodeName)

	if configMapPath != "" {
		t.Log("Backing up current health-events-analyzer configmap")

		backupData, err := BackupConfigMap(ctx, client, "health-events-analyzer-config", NVSentinelNamespace)
		require.NoError(t, err)
		t.Log("Backup created in memory")

		testCtx.ConfigMapBackup = backupData

		err = createConfigMapFromFilePath(ctx, client, configMapPath, "health-events-analyzer-config", NVSentinelNamespace)
		require.NoError(t, err)
	}

	t.Logf("Restarting %s deployment", HEALTH_EVENTS_ANALYZER_DEPLOYMENT_NAME)

	err = RestartDeployment(ctx, t, client, HEALTH_EVENTS_ANALYZER_DEPLOYMENT_NAME, NVSentinelNamespace)
	require.NoError(t, err)

	return ctx, testCtx
}

func clearHealthEventsAnalyzerConditions(ctx context.Context, t *testing.T, nodeName string) {
	t.Logf("Cleaning up any existing node conditions for node %s", nodeName)

	event := NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("MultipleRemediations")

	event.EntitiesImpacted = []EntityImpacted{}

	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXIDErrorOnSameGPU")

	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXID31OnSameGPU")

	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXID31OnDifferentGPU")
	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXID13OnSameGPCAndTPC")

	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("RepeatedXID13OnDifferentGPCAndTPC")

	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)

	event = NewHealthEvent(nodeName).
		WithAgent(HEALTH_EVENTS_ANALYZER_AGENT).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithCheckName("XIDErrorSoloNoBurst")

	event.EntitiesImpacted = []EntityImpacted{}
	SendHealthEvent(ctx, t, event)
}

func TriggerMultipleRemediationsCycle(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	xidsToInject := []string{ERRORCODE_79, ERRORCODE_48}

	t.Log("Delete any existing RebootNode CR")

	err := DeleteAllCRs(ctx, t, client, RebootNodeGVK)
	require.NoError(t, err, "failed to delete all RebootNode CRs")

	// inject 2 fatal errors and let the remediation cycle finish
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

	rebootNodeCR := WaitForCR(ctx, t, client, nodeName, RebootNodeGVK)
	require.NotNil(t, rebootNodeCR, "RebootNode CR should be created for XID error")

	SendHealthyEvent(ctx, t, nodeName)

	t.Logf("Waiting for node %s to be fully uncordoned and cleaned up", nodeName)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}

		if node.Spec.Unschedulable {
			t.Logf("Node %s is still cordoned", nodeName)
			return false
		}

		if node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				t.Logf("Node %s has quarantineHealthEvent annotation", nodeName)
				return false
			}

			if _, exists := node.Annotations["latestFaultRemediationState"]; exists {
				t.Logf("Node %s has latestFaultRemediationState annotation", nodeName)
				return false
			}
		}

		slog.Info("Node fully cleaned up", "node", nodeName)

		return true
	}, EventuallyWaitTimeout, WaitInterval, "node should be fully cleaned up before next remediation cycle")

	err := DeleteCR(ctx, client, rebootNodeCR)
	require.NoError(t, err, "failed to delete RebootNode CR")
}

func TeardownHealthEventsAnalyzer(ctx context.Context, t *testing.T,
	c *envconf.Config, nodeName string, configMapBackup []byte) context.Context {
	t.Logf("Starting cleanup for node %s", nodeName)

	clearHealthEventsAnalyzerConditions(ctx, t, nodeName)

	if configMapBackup != nil {
		t.Log("Restoring health-events-analyzer configmap from memory")
		restoreHealthEventsAnalyzerConfig(ctx, t, c, configMapBackup)
	}

	return ctx
}

// restoreHealthEventsAnalyzerConfig restores the health-events-analyzer config from backup and restarts the deployment.
func restoreHealthEventsAnalyzerConfig(ctx context.Context, t *testing.T, c *envconf.Config, configMapBackup []byte) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	if configMapBackup != nil {
		t.Log("Restoring configmap from memory")

		err = createConfigMapFromBytes(ctx, client, configMapBackup, "health-events-analyzer-config", NVSentinelNamespace)
		require.NoError(t, err)
	}

	err = RestartDeployment(ctx, t, client, "health-events-analyzer", NVSentinelNamespace)
	require.NoError(t, err)
}
