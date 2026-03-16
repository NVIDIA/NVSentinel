//go:build amd64_group
// +build amd64_group

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
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type contextKey string

const (
	keySyslogNodeName contextKey = "nodeName"
	keyStopChan       contextKey = "stopChan"
	keySyslogPodName  contextKey = "syslogPodName"
	keyOriginalArgs   contextKey = "originalArgs"
)

// TestSyslogHealthMonitorXIDDetection tests burst XID injection and aggregation
func TestSyslogHealthMonitorXIDDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - Burst XID Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "xid-detection")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, false)

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		ctx = context.WithValue(ctx, keyOriginalArgs, originalArgs)

		return ctx
	})

	// We require that the "Inject XID error requiring GPU reset" and "Inject GPU reset event" tests run first since
	// these 2 tests will ensure that the SysLogsXIDError is added in response to an XID error and cleared in response
	// to a GPU reset event. This ensures the remaining tests run against the same syslog-health-monitor pod and node,
	// with no unhealthy node status conditions, without needing to recycle the pod during the test execution.
	feature.Assess("Inject XID error requiring GPU reset", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
		}
		expectedSequencePatterns := []string{
			`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, xidMessages)

		t.Log("Verifying node condition contains XID error with GPU UUID using regex pattern")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedSequencePatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should contain XID error with GPU UUID")

		return ctx
	})

	feature.Assess("Inject GPU reset event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		gpuResetMessages := []string{
			"kernel: [16450076.435595] GPU reset executed: GPU-22222222-2222-2222-2222-222222222222",
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, gpuResetMessages)

		t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
			if err != nil {
				return false
			}
			return condition != nil && condition.Status == v1.ConditionFalse
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")

		return ctx
	})

	feature.Assess("Inject burst XID errors and verify aggregation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 94, pid=789012, name=process, Contained ECC error.",
			"kernel: [103859.498995] NVRM: Xid (PCI:0002:00:00): 13, pid=2519562, name=python3, Graphics Exception: ChID 000c, Class 0000cbc0, Offset 00000000, Data 00000000",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, Graphics SM Warp Exception on (GPC 1, TPC 0, SM 0): Misaligned Address",

			// Driver version in metadata file is R570, so this XID's intrInfo is expected to match V1 pattern with recommended action COMPONENT_RESET
			"kernel: [16450076.435584] NVRM: Xid (PCI:0001:00:00): 145, RLW_REMAP Fatal XC1 i0 Link 10 (0x00000082 0x00000040 0x00000000 0x00000000 0x00000000 0x00000000)",

			// Driver version in metadata file is R570, so this XID's intrInfo is not expected to match V2 pattern and recommended action should be NONE
			"kernel: [16450076.435584] NVRM: Xid (PCI:0002:00:00): 145, RLW_REMAP Nonfatal XC1 i0 Link 10 (0x00000004 0x00000040 0x00000000 0x00000000 0x00000000 0x00000000)",
		}

		expectedSequencePatterns := []string{
			`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,

			// XID 94 has been fatal from override rule defined in values-tilt.yaml
			`ErrorCode:94 PCI:0000:17:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0000:17:00\): 94.*?Recommended Action=CONTACT_SUPPORT`,
			`ErrorCode:145.RLW_REMAP PCI:0001:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0001:00:00\): 145.*?Recommended Action=COMPONENT_RESET`,
		}
		expectedEventPatterns := []string{
			`ErrorCode:13 PCI:0002:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 13.*?Recommended Action=NONE`,
			`ErrorCode:31 PCI:0001:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0001:00:00\): 31.*?Recommended Action=NONE`,
			`ErrorCode:145.RLW_REMAP PCI:0002:00:00 GPU_UUID:GPU-[0-9a-fA-F-]+ kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 145.*?Recommended Action=NONE`,
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, xidMessages)

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

	feature.Assess("Inject more XID errors to exceed 1KB and verify truncation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		const (
			maxConditionMessageLength = 1024
			truncationSuffix          = "..."
		)

		// We need 10+ unique node condition entries to exceed 1024 bytes after per-message
		// compaction (each compacted message is ~110 bytes). The burst test already created 3
		// entries (XIDs 119, 94, 145), so we inject 7 more across all 4 GPU PCI addresses.
		// The last message is a duplicate of XID 119 on PCI:0002:00:00 (already present from
		// the burst test) to verify identity-based dedup replaces rather than duplicates.
		additionalXidMessages := []string{
			// XID 119 on remaining GPUs (0002:00:00 already exists from burst test)
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 119, pid=15822501, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 119, pid=15822502, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU0 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:19:00): 119, pid=15822503, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU3 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			// XID 79 on all GPUs
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 79, pid=123457, name=test-again, GPU has fallen off the bus.",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 79, pid=123458, name=test-again, GPU has fallen off the bus.",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 79, pid=123459, name=test-again, GPU has fallen off the bus.",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0000:19:00): 79, pid=123460, name=test-again, GPU has fallen off the bus.",
			// Duplicate: XID 119 on PCI:0002:00:00 already exists from burst test
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=9999999, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
		}

		t.Logf("Injecting %d additional XID messages (includes 1 duplicate) to exceed 1KB limit", len(additionalXidMessages))
		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, additionalXidMessages)

		t.Log("Verifying compaction and dedup: message <= 1KB, no '...' suffix, no duplicate entries")
		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				t.Logf("Condition not found yet: %v", err)
				return false
			}

			messageLen := len(condition.Message)

			if !strings.Contains(condition.Message, "ErrorCode:79") {
				t.Logf("Waiting for all messages to be processed (%d bytes so far)", messageLen)
				return false
			}

			if messageLen > maxConditionMessageLength {
				t.Logf("FAIL: Message length %d exceeds max %d", messageLen, maxConditionMessageLength)
				return false
			}

			if strings.HasSuffix(condition.Message, truncationSuffix) {
				t.Logf("Waiting for dedup to settle (%d bytes, still has '%s')", messageLen, truncationSuffix)
				return false
			}

			// Verify dedup: ErrorCode:119 + PCI:0002:00:00 should appear exactly once
			parts := strings.Split(condition.Message, ";")
			count := 0

			for _, part := range parts {
				if strings.Contains(part, "ErrorCode:119") && strings.Contains(part, "PCI:0002:00:00") {
					count++
				}
			}

			if count != 1 {
				t.Logf("FAIL: Expected exactly 1 entry for ErrorCode:119 PCI:0002:00:00, found %d", count)
				return false
			}

			t.Logf("PASS: %d bytes, compacted without hard truncation, dedup verified", messageLen)
			return true
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"10 compacted entries should fit within 1KB without hard truncation")

		return ctx
	})

	feature.Assess("Inject one more XID to trigger hard truncation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 94, pid=789101, name=process, Contained ECC error.",
		})

		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				return false
			}

			return len(condition.Message) <= 1024 && strings.HasSuffix(condition.Message, "...")
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"11th entry should trigger hard truncation with '...' suffix")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		syslogPod := ctx.Value(keySyslogPodName).(string)
		stopChan := ctx.Value(keyStopChan).(chan struct{})
		originalArgs, _ := ctx.Value(keyOriginalArgs).([]string)

		helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, syslogPod)

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

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, helpers.StubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			helpers.StubJournalHTTPPort,
			helpers.StubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		t.Logf("Setting ManagedByNVSentinel=false on node %s", testNodeName)
		err = helpers.SetNodeManagedByNVSentinel(ctx, client, testNodeName, false)
		require.NoError(t, err, "failed to set ManagedByNVSentinel label")

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		return ctx
	})

	feature.Assess("Verify XID detection works without metadata", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		xidMessage := "kernel: [16450076.435595] NVRM: Xid (PCI:0000:17:00): 79, pid=123456, name=test, GPU has fallen off the bus."

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{xidMessage})

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

		nodeNameVal := ctx.Value(keySyslogNodeName)
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

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, false)

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		ctx = context.WithValue(ctx, keyOriginalArgs, originalArgs)
		return ctx
	})

	feature.Assess("Inject SXID errors and verify GPU lookup via NVSwitch topology", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

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

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, sxidMessages)

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
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		stopChan := ctx.Value(keyStopChan).(chan struct{})
		syslogPod := ctx.Value(keySyslogPodName).(string)
		originalArgs, _ := ctx.Value(keyOriginalArgs).([]string)

		helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, syslogPod)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestSyslogHealthMonitorStoreOnlyStrategy tests the STORE_ONLY event handling strategy
func TestSyslogHealthMonitorStoreOnlyStrategy(t *testing.T) {
	feature := features.New("Syslog Health Monitor - Store Only Strategy").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "store-only-strategy")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, map[string]string{
			"--processing-strategy": "STORE_ONLY",
		}, true)

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyOriginalArgs, originalArgs)
		return ctx
	})

	feature.Assess("Inject XID errors and verify no node condition is created when running in STORE_ONLY strategy", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		xidMessages := []string{
			"kernel: [123.456789] NVRM: Xid (PCI:0000:17:00): 79, pid=123456, name=test, GPU has fallen off the bus.",
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, xidMessages)

		// The syslog monitor processes journal messages at 15sec interval. The timeout is set to
		// 20 seconds to ensure the syslog monitor has sufficient time to process messages and insert
		// events into the database before verifying that the node condition is not present.
		require.Never(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				t.Logf("failed to get node %s: %v", nodeName, err)
				return false
			}

			for _, condition := range node.Status.Conditions {
				if condition.Status == v1.ConditionTrue && condition.Reason == "SysLogsXIDErrorIsNotHealthy" {
					t.Logf("ERROR: Found unexpected node condition: Type=%s, Reason=%s, Status=%s, Message=%s",
						condition.Type, condition.Reason, condition.Status, condition.Message)

					return true
				}
			}

			t.Logf("Node %s correctly does not have a condition with check name '%s'", nodeName, "SysLogsXIDError")

			return false
		}, 20*time.Second, helpers.WaitInterval, "node %s should NOT have a condition with check name %s", nodeName, "SysLogsXIDError")

		t.Log("Verifying node was not cordoned")
		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		stopChan := ctx.Value(keyStopChan).(chan struct{})
		syslogPod := ctx.Value(keySyslogPodName).(string)
		originalArgs, _ := ctx.Value(keyOriginalArgs).([]string)

		helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, syslogPod)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
