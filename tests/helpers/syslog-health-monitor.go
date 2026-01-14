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
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	StubJournalHTTPPort = 9091
	SyslogContainerName = "syslog-health-monitor"
	SyslogDaemonSetName = "syslog-health-monitor-regular"
)

// helper function to set up syslog health monitor and port forward to it.
// Returns the node name, pod, stop channel, and original args (for restoration during teardown).
func SetUpSyslogHealthMonitor(ctx context.Context, t *testing.T,
	client klient.Client, args map[string]string) (string, *v1.Pod, chan struct{}, []string) {
	var err error

	var syslogPod *v1.Pod

	var originalArgs []string

	if args != nil {
		originalArgs, err = UpdateDaemonSetArgs(ctx, t, client, SyslogDaemonSetName, SyslogContainerName, args)
		require.NoError(t, err, "failed to update syslog health monitor processing strategy")
	}

	syslogPod, err = GetDaemonSetPodOnWorkerNode(ctx, t, client, SyslogDaemonSetName, "syslog-health-monitor-regular")
	require.NoError(t, err, "failed to get syslog health monitor pod on worker node")
	require.NotNil(t, syslogPod, "syslog health monitor pod should exist on worker node")

	testNodeName := syslogPod.Spec.NodeName
	t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

	metadata := CreateTestMetadata(testNodeName)
	InjectMetadata(t, ctx, client, NVSentinelNamespace, testNodeName, metadata)

	t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, StubJournalHTTPPort)
	stopChan, readyChan := PortForwardPod(
		ctx,
		client.RESTConfig(),
		syslogPod.Namespace,
		syslogPod.Name,
		StubJournalHTTPPort,
		StubJournalHTTPPort,
	)
	<-readyChan
	t.Log("Port-forward ready")

	t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
	err = SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
	require.NoError(t, err, "failed to set ManagedByNVSentinel label")

	return testNodeName, syslogPod, stopChan, originalArgs
}

// helper function to roll back syslog health monitor daemonset and stop the port forward.
// Pass the originalArgs returned by SetUpSyslogHealthMonitor to restore the daemonset to its original state.
func TearDownSyslogHealthMonitor(ctx context.Context, t *testing.T, client klient.Client,
	nodeName string, stopChan chan struct{},
	originalArgs []string, podName string) {
	t.Log("Stopping port-forward")
	close(stopChan)

	if originalArgs != nil {
		RestoreDaemonSetArgs(ctx, t, client, SyslogDaemonSetName, SyslogContainerName, originalArgs)
	}

	t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)

	err := DeletePod(ctx, t, client, NVSentinelNamespace, podName, true)
	if err != nil {
		t.Logf("Warning: failed to delete pod: %v", err)
	} else {
		t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
		require.Eventually(t, func() bool {
			condition, err := CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
			if err != nil {
				t.Logf("Failed to check node condition: %v", err)
				return false
			}

			return condition != nil && condition.Status == v1.ConditionFalse
		}, EventuallyWaitTimeout, WaitInterval, "SysLogsXIDError condition should be cleared")
	}

	t.Logf("Cleaning up metadata from node %s", nodeName)
	DeleteMetadata(t, ctx, client, NVSentinelNamespace, nodeName)

	t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)

	err = RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
	if err != nil {
		t.Logf("Warning: failed to remove label: %v", err)
	}
}
