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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	stubJournalHTTPPort = 9091
	keyStopChan         = "stopChan"
	keySyslogPodName    = "syslogPodName"
)

// TestSyslogHealthMonitorXIDDetection tests burst XID injection and aggregation
func TestSyslogHealthMonitorXIDDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - Burst XID Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "xid-detection")

	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		return ctx
	})

	feature.Assess("Inject burst XID errors and verify aggregation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		xidMessages := []string{
			"NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"NVRM: Xid (PCI:0001:00:00): 79, pid=123456, name=test, GPU has fallen off the bus.",
			"NVRM: Xid (PCI:0003:00:00): 94, pid=789012, name=process, Contained ECC error.",
			"[103859.498995] NVRM: Xid (PCI:0000:97:00): 13, pid=2519562, name=python3, Graphics Exception: ChID 000c, Class 0000cbc0, Offset 00000000, Data 00000000",
			"NVRM: Xid (PCI:0000:02:00): 31, Graphics SM Warp Exception on (GPC 1, TPC 0, SM 0): Misaligned Address",
		}

		expectedInCondition := []string{"119", "79"}
		expectedInEvents := []string{"94", "13", "31"}

		httpClient := &http.Client{Timeout: 10 * time.Second}

		t.Logf("Injecting %d XID messages in burst via port-forward", len(xidMessages))
		for i, msg := range xidMessages {
			t.Logf("  [%d/%d] Injecting XID message", i+1, len(xidMessages))

			resp, err := httpClient.Post(
				fmt.Sprintf("http://localhost:%d/add", stubJournalHTTPPort),
				"text/plain",
				strings.NewReader(msg),
			)
			require.NoError(t, err, "failed to inject XID message %d", i+1)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, "stub journal should return 200 OK for message %d", i+1)
		}
		t.Logf("All %d XID messages injected successfully", len(xidMessages))

		t.Logf("Verifying node condition on %s contains all %d XID codes", nodeName, len(expectedInCondition))
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil {
				t.Logf("Error checking condition: %v", err)
				return false
			}
			if condition == nil {
				t.Log("Condition not found yet")
				return false
			}

			t.Logf("Found condition message: %s", condition.Message)

			for _, expected := range expectedInCondition {
				if !strings.Contains(condition.Message, expected) {
					t.Logf("Missing XID %s in condition message", expected)
					return false
				}
			}

			t.Logf("All %d expected XID codes found in condition message", len(expectedInCondition))
			return true
		}, helpers.WaitTimeout, helpers.WaitInterval, "Node condition should contain all %d XID codes", len(expectedInCondition))

		t.Logf("Verifying %d non-fatal XID codes created events on %s", len(expectedInEvents), nodeName)
		require.Eventually(t, func() bool {
			eventList, err := helpers.GetNodeEvents(ctx, client, nodeName, "SysLogsXIDError")
			if err != nil {
				t.Logf("Error listing events: %v", err)
				return false
			}

			var allMessages []string
			for _, event := range eventList.Items {
				if event.Reason == "SysLogsXIDErrorIsNotHealthy" {
					allMessages = append(allMessages, event.Message)
				}
			}

			if len(allMessages) == 0 {
				t.Log("No events found yet")
				return false
			}

			foundXids := make(map[string]bool)
			for _, expectedXid := range expectedInEvents {
				for _, msg := range allMessages {
					if strings.Contains(msg, expectedXid) {
						foundXids[expectedXid] = true
						break
					}
				}
			}

			for _, expectedXid := range expectedInEvents {
				if foundXids[expectedXid] {
					t.Logf("  Found XID %s in events", expectedXid)
				} else {
					t.Logf("  Missing XID %s in events", expectedXid)
				}
			}

			return len(foundXids) == len(expectedInEvents)
		}, helpers.WaitTimeout, helpers.WaitInterval, "All non-fatal XIDs should appear in events")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.WaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
