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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

type contextKey string

const (
	stubJournalHTTPPort            = 9091
	keyStopChan         contextKey = "stopChan"
	keySyslogPodName    contextKey = "syslogPodName"
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

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

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
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 79, pid=123456, name=test, GPU has fallen off the bus.",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 94, pid=789012, name=process, Contained ECC error.",
			"kernel: [103859.498995] NVRM: Xid (PCI:0002:00:00): 13, pid=2519562, name=python3, Graphics Exception: ChID 000c, Class 0000cbc0, Offset 00000000, Data 00000000",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, Graphics SM Warp Exception on (GPC 1, TPC 0, SM 0): Misaligned Address",
			"kernel: [16450076.435584] NVRM: Xid (PCI:0000:19:00): 62, 32260b5e 000154b0 00000000 2026da96 202b5626 202b5832 202b5872 202b58be",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:19:00): 45, pid=2864945, name=kit, Ch 0000000b",
		}

		expectedSequencePatterns := []string{
			`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,
			`ErrorCode:79 PCI:0001:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0001:00:00\): 79.*?Recommended Action=RESTART_BM`,
			`ErrorCode:62 PCI:0000:19:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:19:00\): 62.*?Recommended Action=COMPONENT_RESET`,
			`ErrorCode:45 PCI:0000:19:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:19:00\): 45.*?Recommended Action=CONTACT_SUPPORT`,
		}
		expectedEventPatterns := []string{
			`ErrorCode:94 PCI:0000:17:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:17:00\): 94.*?Recommended Action=NONE`,
			`ErrorCode:13 PCI:0002:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 13.*?Recommended Action=NONE`,
			`ErrorCode:31 PCI:0001:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0001:00:00\): 31.*?Recommended Action=NONE`,
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		t.Log("Verifying node condition contains XID sequence with GPU UUIDs using regex patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedSequencePatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should contain XID sequence with GPU UUIDs")

		t.Log("Verifying events contain expected XIDs with GPU UUIDs using regex patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyEventsMatchPatterns(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedEventPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Events should contain all XIDs with GPU UUIDs")

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
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestSyslogHealthMonitorXIDFloodAndTruncation tests two scenarios in sequence:
// 1. First, injects duplicate XID errors and verifies they are all captured in the node condition (within 1KB, not truncated)
// 2. Then, injects many more XIDs to exceed the 1KB limit and verifies truncation happens correctly
func TestSyslogHealthMonitorXIDFloodAndTruncation(t *testing.T) {
	feature := features.New("Syslog Health Monitor - XID Flood and Truncation").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "xid-flood-truncation")

	const (
		// maxConditionMessageLength must match the default value in Helm values.yaml
		// and the system's ConfigMap configuration for accurate truncation testing
		maxConditionMessageLength = 1024
		truncationSuffix          = "..."
	)

	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Wait for pod to be available and ready (handles race condition from previous test teardown)
		t.Log("Waiting for syslog-health-monitor pod to be ready...")
		require.Eventually(t, func() bool {
			var podErr error
			syslogPod, podErr = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
			if podErr != nil {
				t.Logf("Waiting for pod: %v", podErr)
				return false
			}
			// Verify pod is ready with all containers running
			for _, containerStatus := range syslogPod.Status.ContainerStatuses {
				if !containerStatus.Ready {
					t.Logf("Container %s not ready yet", containerStatus.Name)
					return false
				}
			}
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "syslog-health-monitor pod should be ready")

		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

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

	// Phase 1: Inject varied XIDs to fill ~80-90% of 1KB limit (no truncation)
	// Each message in node condition is ~180-250 bytes, so 5 messages should reach ~850-900 bytes
	feature.Assess("Phase 1: Inject varied XID errors to fill ~80-90% of 1KB limit", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		// Use varied XID formats (different error codes and message types) to fill ~75-85% of 1KB
		// All are fatal XIDs that will go to node condition
		// Each message in condition is ~200-220 bytes, so 4 messages = ~800-880 bytes (~80% of 1KB)
		xidMessages := []string{
			// XID 79 - GPU fallen off bus (~210 bytes in condition)
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 79, pid=123456, name=test, GPU has fallen off the bus.",
			// XID 62 - GPU memory errors with hex data (~230 bytes in condition)
			"kernel: [16450076.435584] NVRM: Xid (PCI:0000:19:00): 62, 32260b5e 000154b0 00000000 2026da96 202b5626 202b5832 202b5872 202b58be",
			// XID 45 - Channel errors with duplicate on same PCI (~210 bytes each in condition)
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:19:00): 45, pid=2864945, name=kit, Ch 0000000b",
			"kernel: [16450076.436857] NVRM: Xid (PCI:0000:19:00): 45, pid=2864945, name=kit, Ch 0000000c",
		}

		// Expected patterns for all 4 fatal XIDs in order
		expectedSequencePatterns := []string{
			`ErrorCode:79 PCI:0001:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0001:00:00\): 79.*?GPU has fallen off the bus.*?Recommended Action=RESTART_BM`,
			`ErrorCode:62 PCI:0000:19:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:19:00\): 62.*?Recommended Action=COMPONENT_RESET`,
			`ErrorCode:45 PCI:0000:19:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:19:00\): 45.*?Ch 0000000b.*?Recommended Action=CONTACT_SUPPORT`,
			`ErrorCode:45 PCI:0000:19:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:19:00\): 45.*?Ch 0000000c.*?Recommended Action=CONTACT_SUPPORT`,
		}

		t.Logf("Phase 1: Injecting %d varied XID messages to fill ~75-85%% of 1KB", len(xidMessages))
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		t.Log("Verifying node condition contains all 4 XID patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedSequencePatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should contain all 4 XIDs")

		// Verify message is within 1KB and NOT truncated
		t.Log("Verifying node condition message is within 1KB limit and NOT truncated")
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				return false
			}
			isNotTruncated := !strings.HasSuffix(condition.Message, truncationSuffix)
			isWithinLimit := len(condition.Message) <= maxConditionMessageLength
			percentUsed := float64(len(condition.Message)) / float64(maxConditionMessageLength) * 100
			t.Logf("Phase 1 - Message length: %d bytes (%.1f%% of 1KB), not truncated: %v",
				len(condition.Message), percentUsed, isNotTruncated)
			return isNotTruncated && isWithinLimit
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition message should be within 1KB and NOT truncated")

		t.Log("Phase 1 PASSED: All varied XIDs captured within 1KB without truncation")
		return ctx
	})

	// Phase 2: Add 1-2 more XIDs to exceed 1KB and trigger truncation
	// The previous XIDs from Phase 1 are still in the node condition, so we just need to add enough to exceed 1KB
	feature.Assess("Phase 2: Add more XIDs to exceed 1KB and verify truncation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		// Add 1 more fatal XID with a longer message to push over 1KB
		// Phase 1 has ~850 bytes, this XID 119 adds ~250 bytes → total ~1100 bytes → triggers truncation
		additionalXidMessages := []string{
			// XID 119 - Longer GSP RPC timeout message (~250 bytes in condition)
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
		}

		t.Logf("Phase 2: Injecting %d additional XID message to exceed 1KB (added to existing 4 from Phase 1)", len(additionalXidMessages))
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, additionalXidMessages)

		t.Log("Verifying node condition message is truncated to 1KB limit")
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				t.Logf("Condition not found yet: %v", err)
				return false
			}

			messageLen := len(condition.Message)

			// Check 1: Message must not exceed 1KB
			if messageLen > maxConditionMessageLength {
				t.Logf("FAIL: Message length %d exceeds max %d", messageLen, maxConditionMessageLength)
				return false
			}

			// Check 2: Must have truncation suffix (indicating truncation occurred)
			if !strings.HasSuffix(condition.Message, truncationSuffix) {
				t.Logf("FAIL: Message should end with truncation suffix '%s'", truncationSuffix)
				return false
			}

			// Check 3: Should contain XIDs from Phase 1 (at least the first ones)
			if !strings.Contains(condition.Message, "ErrorCode:79") {
				t.Logf("FAIL: Message should contain ErrorCode:79 from Phase 1")
				return false
			}

			t.Logf("Phase 2 - Message length: %d bytes (exactly at or near 1KB limit), truncated with suffix '%s'",
				messageLen, truncationSuffix)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node condition message should be truncated to 1KB with truncation suffix")

		t.Log("Phase 2 PASSED: Message correctly truncated to 1KB limit while preserving earlier XIDs")
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
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestSyslogHealthMonitorXIDWithoutMetadata tests XID detection works without metadata file
func TestSyslogHealthMonitorXIDWithoutMetadata(t *testing.T) {
	feature := features.New("Syslog Health Monitor - XID Without Metadata").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "xid-graceful-degradation")

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

	feature.Assess("Verify XID detection works without metadata", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		xidMessage := "kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 79, pid=123456, name=test, GPU has fallen off the bus."

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, []string{xidMessage})

		t.Log("Verifying node condition is created without GPU UUID (metadata not available)")
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				t.Logf("Condition not found yet: %v", err)
				return false
			}

			if !strings.Contains(condition.Message, "ErrorCode:79") {
				t.Logf("Condition found but missing error code 79: %s", condition.Message)
				return false
			}

			if !strings.Contains(condition.Message, "PCI:0000:17:00") {
				t.Logf("Condition found but missing PCI address: %s", condition.Message)
				return false
			}

			if strings.Contains(condition.Message, "GPU-") {
				t.Logf("Condition should NOT contain GPU UUID but does: %s", condition.Message)
				return false
			}

			t.Logf("Found condition without GPU UUID (expected): %s", condition.Message)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should be created without GPU UUID")

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

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestSyslogHealthMonitorSXIDDetection tests SXID error detection with NVSwitch topology
func TestSyslogHealthMonitorSXIDDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - SXID Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "sxid-detection")

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

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

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

	feature.Assess("Inject SXID errors and verify GPU lookup via NVSwitch topology", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		sxidMessages := []string{
			"kernel: [123.456789] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 20009, Non-fatal, Link 28 RX Short Error Rate",
			"kernel: [123.456790] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 28006, Non-fatal, Link 29 MC TS crumbstore MCTO",
			"kernel: [123.456791] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 24007, Fatal, Link 30 sourcetrack timeout error",
		}

		expectedEventPatterns := []string{
			`ErrorCode:20009 NVSWITCH:0 PCI:0000:c3:00\.0 NVLINK:28 GPU:0 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?nvidia-nvswitch0: SXid \(PCI:0000:c3:00\.0\): 20009.*?Recommended Action=NONE`,
			`ErrorCode:28006 NVSWITCH:0 PCI:0000:c3:00\.0 NVLINK:29 GPU:0 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?nvidia-nvswitch0: SXid \(PCI:0000:c3:00\.0\): 28006.*?Recommended Action=NONE`,
		}

		expectedConditionPattern := []string{
			`ErrorCode:24007 NVSWITCH:0 PCI:0000:c3:00\.0 NVLINK:30 GPU:1 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?nvidia-nvswitch0: SXid \(PCI:0000:c3:00\.0\): 24007, Fatal, Link 30.*?Recommended Action=CONTACT_SUPPORT`,
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, sxidMessages)

		t.Log("Verifying we got 2 non-fatal SXID Kubernetes Events with GPU UUIDs using regex patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyEventsMatchPatterns(t, ctx, client, nodeName, "SysLogsSXIDError",
				"SysLogsSXIDErrorIsNotHealthy", expectedEventPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Should have 2 non-fatal SXID events with GPU UUIDs")

		t.Log("Verifying node condition contains fatal SXID error code 24007 with full message structure")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName, "SysLogsSXIDError",
				"SysLogsSXIDErrorIsNotHealthy", expectedConditionPattern)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should contain SXID 24007 with full message")

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
			t.Log("Skipping teardown: nodeName not set")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsSXIDError", "SysLogsSXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		err = helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName)
		if err != nil {
			t.Logf("Warning: failed to remove label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
