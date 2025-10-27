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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	syslogHealthMonitorNamespace = "nvsentinel"
	stubJournalHTTPPort          = 9091
	keySyslogPod                 = "syslogPod"
)

// TestSyslogHealthMonitorXIDDetection tests both fatal and non-fatal XID error detection
func TestSyslogHealthMonitorXIDDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - XID Error Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "xid-detection")

	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, syslogHealthMonitorNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPod, syslogPod)
		return ctx
	})

	feature.Assess("Inject fatal XID error and verify node condition", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)
		pod := ctx.Value(keySyslogPod).(*v1.Pod)
		restConfig := client.RESTConfig()

		xidMessage := "NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8)."

		t.Logf("Step 1: Injecting fatal XID 119 error on pod %s", pod.Name)
		command := []string{
			"/bin/sh", "-c",
			fmt.Sprintf("curl -X POST http://localhost:%d/add -d '%s'", stubJournalHTTPPort, xidMessage),
		}

		stdout, stderr, err := helpers.ExecInPod(ctx, restConfig, pod.Namespace, pod.Name, "syslog-health-monitor", command)
		require.NoError(t, err, "failed to inject fatal XID error: %s", stderr)
		t.Logf("Injection successful: %s", stdout)

		t.Logf("Step 2: Verifying node condition on %s", nodeName)
		require.Eventually(t, func() bool {
			found, condition := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				v1.NodeConditionType("SysLogsXIDError"), "SysLogsXIDErrorIsNotHealthy")
			if found {
				t.Logf("Found condition: %s - %s", condition.Type, condition.Message)
			}
			return found
		}, 2*time.Minute, 10*time.Second, "Fatal XID should create SysLogsXIDError node condition")

		return ctx
	})

	feature.Assess("Inject non-fatal XID error and verify event creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)
		pod := ctx.Value(keySyslogPod).(*v1.Pod)
		restConfig := client.RESTConfig()

		xidMessage := "NVRM: Xid (PCI:0000:02:00): 13, pid=456, name=xid_trigger, Graphics Exception on (GPC 1, TPC 0)"

		t.Logf("Step 3: Injecting non-fatal XID 13 error on pod %s", pod.Name)
		command := []string{
			"/bin/sh", "-c",
			fmt.Sprintf("curl -X POST http://localhost:%d/add -d '%s'", stubJournalHTTPPort, xidMessage),
		}

		stdout, stderr, err := helpers.ExecInPod(ctx, restConfig, pod.Namespace, pod.Name, "syslog-health-monitor", command)
		require.NoError(t, err, "failed to inject non-fatal XID error: %s", stderr)
		t.Logf("Injection successful: %s", stdout)

		t.Logf("Step 4: Verifying event (not node condition) on %s", nodeName)
		require.Eventually(t, func() bool {
			found, event := helpers.CheckNodeEventExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if found {
				t.Logf("Found event: %s - %s", event.Type, event.Message)
			}
			return found
		}, 2*time.Minute, 10*time.Second, "Non-fatal XID should create SysLogsXIDError event")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeName := ctx.Value(keyNodeName).(string)

		t.Logf("Removing SysLogsXIDError condition from node %s", nodeName)
		err = helpers.RemoveNodeCondition(ctx, client, nodeName, v1.NodeConditionType("SysLogsXIDError"))
		if err != nil {
			t.Logf("Warning: failed to remove SysLogsXIDError condition: %v", err)
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
