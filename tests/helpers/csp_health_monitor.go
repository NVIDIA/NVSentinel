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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	cspHealthMonitorConfigMapName  = "csp-health-monitor"
	cspHealthMonitorDeploymentName = "csp-health-monitor"
	cspHealthMonitorContainerName  = "csp-health-monitor"

	testMaintenanceEventPollIntervalSeconds = 10
	testAPIPollingIntervalSeconds           = 30
	testPostMaintenanceHealthyDelayMinutes  = 1
	testTriggerQuarantineWorkflowMinutes    = 30
)

type CSPHealthMonitorTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TargetCSP       CSPType
	CSPClient       *CSPAPIMockClient
}

// cspHealthMonitorConfig represents the config.toml structure for CSP health monitor
type cspHealthMonitorConfig struct {
	MaintenanceEventPollIntervalSeconds       int                    `toml:"maintenanceEventPollIntervalSeconds"`
	TriggerQuarantineWorkflowTimeLimitMinutes int                    `toml:"triggerQuarantineWorkflowTimeLimitMinutes"`
	PostMaintenanceHealthyDelayMinutes        int                    `toml:"postMaintenanceHealthyDelayMinutes"`
	NodeReadinessTimeoutMinutes               int                    `toml:"nodeReadinessTimeoutMinutes"`
	ClusterName                               string                 `toml:"clusterName"`
	GCP                                       cspHealthMonitorGCPCfg `toml:"gcp"`
	AWS                                       cspHealthMonitorAWSCfg `toml:"aws"`
}

type cspHealthMonitorGCPCfg struct {
	Enabled                   bool   `toml:"enabled"`
	TargetProjectID           string `toml:"targetProjectId"`
	APIPollingIntervalSeconds int    `toml:"apiPollingIntervalSeconds"`
	GCPServiceAccountName     string `toml:"gcpServiceAccountName"`
	LogFilter                 string `toml:"logFilter"`
	EndpointOverride          string `toml:"endpointOverride"`
}

type cspHealthMonitorAWSCfg struct {
	Enabled                bool   `toml:"enabled"`
	AccountID              string `toml:"accountId"`
	PollingIntervalSeconds int    `toml:"pollingIntervalSeconds"`
	Region                 string `toml:"region"`
	EndpointOverride       string `toml:"endpointOverride"`
}

func SetupCSPHealthMonitorTest(
	ctx context.Context, t *testing.T, c *envconf.Config, targetCSP CSPType,
) (context.Context, *CSPHealthMonitorTestContext) {
	return setupCSPHealthMonitorTest(ctx, t, c, targetCSP, testTriggerQuarantineWorkflowMinutes)
}

func SetupCSPHealthMonitorTestWithThreshold(
	ctx context.Context, t *testing.T, c *envconf.Config, targetCSP CSPType, thresholdMinutes int,
) (context.Context, *CSPHealthMonitorTestContext) {
	return setupCSPHealthMonitorTest(ctx, t, c, targetCSP, thresholdMinutes)
}

func setupCSPHealthMonitorTest(
	ctx context.Context, t *testing.T, c *envconf.Config, targetCSP CSPType, thresholdMinutes int,
) (context.Context, *CSPHealthMonitorTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &CSPHealthMonitorTestContext{
		TargetCSP: targetCSP,
	}

	backupData, err := BackupConfigMap(ctx, client, cspHealthMonitorConfigMapName, NVSentinelNamespace)
	require.NoError(t, err, "failed to backup ConfigMap")

	testCtx.ConfigMapBackup = backupData

	err = updateCSPHealthMonitorConfig(ctx, client, targetCSP, thresholdMinutes)
	require.NoError(t, err, "failed to update ConfigMap")

	if targetCSP == CSPAWS {
		err = SetDeploymentEnvVars(ctx, client, cspHealthMonitorDeploymentName, NVSentinelNamespace,
			cspHealthMonitorContainerName, map[string]string{
				"AWS_ACCESS_KEY_ID":     "test-access-key",
				"AWS_SECRET_ACCESS_KEY": "test-secret-key",
			})
		require.NoError(t, err, "failed to set AWS credentials")
	}

	testCtx.NodeName = SelectTestNodeWithEmptyProviderID(ctx, t, client)
	testCtx.CSPClient, err = NewCSPAPIMockClient(t, client)
	require.NoError(t, err, "failed to create CSP API Mock client")

	// Reset poll count before restart so we can detect the first poll from the new pod
	require.NoError(t, testCtx.CSPClient.ResetPollCount(targetCSP), "failed to reset poll count")

	err = RestartDeployment(ctx, t, client, cspHealthMonitorDeploymentName, NVSentinelNamespace)
	require.NoError(t, err, "failed to restart deployment")

	// Wait for the CSP health monitor to complete at least one poll cycle
	t.Log("Waiting for CSP health monitor to complete first poll cycle...")
	waitForFirstPoll(t, testCtx.CSPClient, targetCSP)

	return ctx, testCtx
}

func TeardownCSPHealthMonitorTest(
	ctx context.Context, t *testing.T, c *envconf.Config, testCtx *CSPHealthMonitorTestContext,
) context.Context {
	client, err := c.NewClient()
	if err != nil {
		t.Logf("Warning: failed to create client for teardown: %v", err)
		return ctx
	}

	if testCtx != nil && testCtx.CSPClient != nil {
		_ = testCtx.CSPClient.ClearEvents(CSPGCP)
		_ = testCtx.CSPClient.ClearEvents(CSPAWS)
		testCtx.CSPClient.Close()
	}

	if testCtx != nil && testCtx.NodeName != "" {
		cleanupCSPTestNode(ctx, client, testCtx.NodeName)
	}

	if testCtx != nil && testCtx.TargetCSP == CSPAWS {
		_ = RemoveDeploymentEnvVars(ctx, client, cspHealthMonitorDeploymentName, NVSentinelNamespace,
			cspHealthMonitorContainerName, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"})
	}

	if testCtx != nil && testCtx.ConfigMapBackup != nil {
		_ = createConfigMapFromBytes(ctx, client, testCtx.ConfigMapBackup, cspHealthMonitorConfigMapName, NVSentinelNamespace)
		_ = RestartDeployment(ctx, t, client, cspHealthMonitorDeploymentName, NVSentinelNamespace)
	}

	return ctx
}

func updateCSPHealthMonitorConfig(
	ctx context.Context, client klient.Client, targetCSP CSPType, thresholdMinutes int,
) error {
	return UpdateConfigMapTOMLField(ctx, client, cspHealthMonitorConfigMapName, NVSentinelNamespace, "config.toml",
		func(cfg *cspHealthMonitorConfig) error {
			if cfg.ClusterName == "" {
				return fmt.Errorf("clusterName is empty in existing config")
			}

			cfg.MaintenanceEventPollIntervalSeconds = testMaintenanceEventPollIntervalSeconds
			cfg.PostMaintenanceHealthyDelayMinutes = testPostMaintenanceHealthyDelayMinutes
			cfg.TriggerQuarantineWorkflowTimeLimitMinutes = thresholdMinutes

			switch targetCSP {
			case CSPGCP:
				cfg.GCP.Enabled = true

				cfg.AWS.Enabled = false
				if cfg.GCP.TargetProjectID == "" {
					cfg.GCP.TargetProjectID = "test-project"
				}

				if cfg.GCP.LogFilter == "" {
					cfg.GCP.LogFilter = `logName="projects/test-project/logs/cloudaudit.googleapis.com%2Fsystem_event"`
				}

				cfg.GCP.APIPollingIntervalSeconds = testAPIPollingIntervalSeconds
				cfg.GCP.EndpointOverride = "csp-api-mock.nvsentinel.svc.cluster.local:50051"

			case CSPAWS:
				cfg.GCP.Enabled = false

				cfg.AWS.Enabled = true
				if cfg.AWS.AccountID == "" {
					cfg.AWS.AccountID = "123456789012"
				}

				if cfg.AWS.Region == "" {
					cfg.AWS.Region = "us-east-1"
				}

				cfg.AWS.PollingIntervalSeconds = testAPIPollingIntervalSeconds
				cfg.AWS.EndpointOverride = "http://csp-api-mock.nvsentinel.svc.cluster.local:8080/aws/health"

			default:
				return fmt.Errorf("unsupported CSP type: %s", targetCSP)
			}

			return nil
		})
}

func cleanupCSPTestNode(ctx context.Context, client klient.Client, nodeName string) {
	if nodeName == "" {
		return
	}

	node, err := GetNodeByName(ctx, client, nodeName)
	if err != nil {
		return
	}

	if strings.HasPrefix(node.Spec.ProviderID, "aws:///") {
		_ = client.Resources().Delete(ctx, node)
		return
	}

	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return err
		}

		if node.Annotations != nil {
			delete(node.Annotations, "container.googleapis.com/instance_id")
		}

		if node.Labels != nil {
			delete(node.Labels, "topology.kubernetes.io/zone")
		}

		node.Spec.Unschedulable = false

		return client.Resources().Update(ctx, node)
	})
}

// AddGCPInstanceIDAnnotation adds the GCP instance ID annotation and zone label to a node.
// Both are required for the csp-health-monitor to map GCP maintenance events to K8s nodes.
func AddGCPInstanceIDAnnotation(ctx context.Context, client klient.Client, nodeName, instanceID, zone string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return err
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations["container.googleapis.com/instance_id"] = instanceID

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		node.Labels["topology.kubernetes.io/zone"] = zone

		return client.Resources().Update(ctx, node)
	})
}

// AddAWSProviderID adds an AWS-style provider ID to a node's spec.
func AddAWSProviderID(ctx context.Context, client klient.Client, nodeName, instanceID, zone string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return err
		}

		// Set provider ID in AWS format: aws:///zone/instance-id
		node.Spec.ProviderID = fmt.Sprintf("aws:///%s/%s", zone, instanceID)

		return client.Resources().Update(ctx, node)
	})
}

// WaitForCSPMaintenanceCondition waits for a node to have (or not have) the CSPMaintenance condition.
func WaitForCSPMaintenanceCondition(
	ctx context.Context, t *testing.T, client klient.Client, nodeName string, expectCondition bool, expectCordoned bool,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			t.Logf("Error getting node %s: %v", nodeName, err)
			return false
		}

		if !checkCordonState(t, node, nodeName, expectCordoned) {
			return false
		}

		hasCondition := checkCSPMaintenanceCondition(t, node, nodeName)

		if !validateConditionExpectation(t, nodeName, hasCondition, expectCondition) {
			return false
		}

		logConditionResult(t, nodeName, expectCondition)

		return true
	}, EventuallyWaitTimeout, WaitInterval)
}

func checkCordonState(t *testing.T, node *v1.Node, nodeName string, expectCordoned bool) bool {
	if node.Spec.Unschedulable != expectCordoned {
		if expectCordoned {
			t.Logf("Node %s is not cordoned yet", nodeName)
		} else {
			t.Logf("Node %s is still cordoned, waiting for recovery...", nodeName)
		}

		return false
	}

	return true
}

func checkCSPMaintenanceCondition(t *testing.T, node *v1.Node, nodeName string) bool {
	for _, condition := range node.Status.Conditions {
		if string(condition.Type) == "CSPMaintenance" && condition.Status == v1.ConditionTrue {
			t.Logf("Node %s has CSPMaintenance condition: %s", nodeName, condition.Message)
			return true
		}
	}

	return false
}

func validateConditionExpectation(t *testing.T, nodeName string, hasCondition, expectCondition bool) bool {
	if expectCondition && !hasCondition {
		t.Logf("Node %s is cordoned but CSPMaintenance condition not set yet", nodeName)
		return false
	}

	if !expectCondition && hasCondition {
		t.Logf("CSPMaintenance condition still True, waiting for it to be cleared...")
		return false
	}

	return true
}

func logConditionResult(t *testing.T, nodeName string, expectCondition bool) {
	if expectCondition {
		t.Logf("Node %s is quarantined with CSPMaintenance condition", nodeName)
	} else {
		t.Logf("Node %s has been recovered (uncordoned, CSPMaintenance cleared)", nodeName)
	}
}

func waitForFirstPoll(t *testing.T, cspClient *CSPAPIMockClient, targetCSP CSPType) {
	t.Helper()
	waitForPollCountIncrease(t, cspClient, targetCSP, 0)
}

func waitForPollCountIncrease(t *testing.T, cspClient *CSPAPIMockClient, targetCSP CSPType, initialCount int64) {
	t.Helper()

	startTime := time.Now()

	require.Eventually(t, func() bool {
		pollCount, err := cspClient.GetPollCount(targetCSP)
		if err != nil {
			t.Logf("Error getting poll count for %s: %v", targetCSP, err)
			return false
		}

		if pollCount <= initialCount {
			elapsed := time.Since(startTime).Round(time.Second)
			t.Logf("Waiting for %s poll (count: %d, need > %d, elapsed: %v)", targetCSP, pollCount, initialCount, elapsed)

			return false
		}

		t.Logf("CSP health monitor poll detected (count: %d → %d)", initialCount, pollCount)

		return true
	}, EventuallyWaitTimeout, WaitInterval,
		fmt.Sprintf("CSP health monitor (%s) did not poll within timeout", targetCSP))
}

// WaitForCSPHealthMonitorPoll waits for the CSP health monitor to complete at least one
// poll cycle after this function is called. It resets the poll counter first to ensure
// we detect the NEW pod's first poll after a restart.
func WaitForCSPHealthMonitorPoll(t *testing.T, cspClient *CSPAPIMockClient, targetCSP CSPType) {
	t.Helper()

	require.NoError(t, cspClient.ResetPollCount(targetCSP), "failed to reset poll count")
	t.Logf("Waiting for %s health monitor to complete first poll cycle...", targetCSP)
	waitForFirstPoll(t, cspClient, targetCSP)
}

// WaitForNextPoll waits for the CSP health monitor to complete at least one more poll cycle
// since the function was called
func WaitForNextPoll(t *testing.T, cspClient *CSPAPIMockClient, targetCSP CSPType) {
	t.Helper()

	initialCount, err := cspClient.GetPollCount(targetCSP)
	require.NoError(t, err, "failed to get initial poll count")

	t.Logf("Waiting for next %s poll cycle (current count: %d)", targetCSP, initialCount)
	waitForPollCountIncrease(t, cspClient, targetCSP, initialCount)
}
