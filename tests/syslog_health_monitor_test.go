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

		healthyEvent := helpers.NewHealthEvent(testNodeName).
			WithAgent("syslog-health-monitor").
			WithCheckName("SysLogsNICDriverError").
			WithComponentClass("NIC").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithEntitiesImpacted([]helpers.EntityImpacted{})
		helpers.SendHealthEvent(ctx, t, healthyEvent)

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

	feature.Assess("Inject successful GPU reset event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		gpuResetMessages := []string{
			"kernel: [16450076.435595] GPU reset executed: GPU-22222222-2222-2222-2222-222222222222, success: true",
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

	feature.Assess("Inject failed GPU reset event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		gpuResetMessages := []string{
			"kernel: [16450076.435595] GPU reset executed: GPU-22222222-2222-2222-2222-222222222222, success: false",
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, gpuResetMessages)

		expectedSequencePatterns := []string{
			`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,
			"ErrorCode:GPU_RESET_FAILURE PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 GPU reset failed, proceeding with a node reboot.*?Recommended Action=RESTART_VM",
		}

		t.Log("Verifying node condition contains 2 health events for the original XID error and a failed GPU reset")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedSequencePatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "Node condition should contain XID error and failed GPU reset condition")

		return ctx
	})

	feature.Assess("Simulating healthy event from SysLogsXIDError post reboot", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		t.Logf("Sending healthy SysLogsXIDError event to clear all errors")
		healthyEvent := helpers.NewHealthEvent(nodeName).
			WithCheckName("SysLogsXIDError").
			WithAgent("syslog-health-monitor").
			WithComponentClass("GPU").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithEntitiesImpacted([]helpers.EntityImpacted{}) // Empty entities clears all

		helpers.SendHealthEvent(ctx, t, healthyEvent)

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

// TestSyslogHealthMonitorNICDriverDetection verifies the SysLogsNICDriverError
// check emits fatal node conditions and non-fatal Kubernetes events from
// kernel log lines.
func TestSyslogHealthMonitorNICDriverDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - NIC Driver Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "nic-driver-detection")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// ManagedByNVSentinel=true is required for the fault-quarantine
		// syslog ruleset to cordon this test node.
		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, true)

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		ctx = context.WithValue(ctx, keyOriginalArgs, originalArgs)

		return ctx
	})

	feature.Assess("Inject all fatal NIC driver patterns", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		messages := []string{
			"kernel: [   66.816635] mlx5_core 0000:03:00.0: wait_func:964:(pid 112): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource",
			"kernel: [   17.982092] mlx5_core 0000:41:00.0: poll_health:852:(pid 0): device's health compromised - reached miss count",
			"kernel: [   17.982217] mlx5_core 0000:41:00.0: print_health_info:458:(pid 0): synd 0x8: unrecoverable hardware error",
		}
		expectedPatterns := []string{
			`ErrorCode:cmd_exec_timeout kernel:.*?mlx5_core 0000:03:00\.0: wait_func:964:\(pid 112\): ENABLE_HCA\(0x104\) timeout\. Will cause a leak of a command resource.*?Recommended Action=REPLACE_VM`,
			`ErrorCode:health_poll_failed kernel:.*?mlx5_core 0000:41:00\.0: poll_health:852:\(pid 0\): device's health compromised - reached miss count.*?Recommended Action=REPLACE_VM`,
			`ErrorCode:unrecoverable_err kernel:.*?mlx5_core 0000:41:00\.0: print_health_info:458:\(pid 0\): synd 0x8: unrecoverable hardware error.*?Recommended Action=REPLACE_VM`,
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, messages)

		t.Log("Verifying SysLogsNICDriverError condition contains all fatal NIC driver patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsNotHealthy", expectedPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node condition should contain all fatal NIC driver patterns")

		t.Log("Verifying fatal NIC driver syslog event cordons the node")
		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{
					Key:         helpers.QuarantineHealthEventAnnotationKey,
					Pattern:     "cmd_exec_timeout",
					ShouldExist: true,
				},
			},
		})

		return ctx
	})

	feature.Assess("Inject all non-fatal NIC driver patterns", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)
		messages := []string{
			"kernel: [123.456792] NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue timed out",
			"kernel: [123.456793] mlx5_core 0000:65:00.0 ens15np0: NETDEV WATCHDOG: CPU: 94: transmit queue 3 timed out 5032 ms",
			"kernel: Jan 21 11:18:36 kernel: mlx5_core 0000:01:00.0: mlx5_pcie_event:326:(pid 165): Detected insufficient power on the PCIe slot (27W).",
			"kernel: [123.456794] mlx5_core 0000:5c:00.0: Port module event[error]: module 0, Cable error, High Temperature",
			"kernel: [123.456795] mlx5_core 0000:d2:00.0: mlx5_cmd_out_err:838:(pid 1441871): ACCESS_REG(0x805) op_mod(0x1) failed, status bad operation(0x2), syndrome (0x305684), err(-22)",
			"kernel: [    8.951695] mlx5_core 0000:63:00.1: Port module event: module 1, Cable unplugged",
			"kernel: [123.456796] mlx5_core 0000:65:00.0 ens15np0: RX timeout on channel: 20, ICOSQ: 0x1ee0, RQ: 0x1e43, CQ: 0x3ea6",
		}
		expectedEventPatterns := []string{
			`ErrorCode:netdev_watchdog kernel:.*NETDEV WATCHDOG: eth0 \(mlx5_core\): transmit queue timed out.*Recommended Action=NONE`,
			`ErrorCode:netdev_watchdog kernel:.*mlx5_core 0000:65:00\.0 ens15np0: NETDEV WATCHDOG: CPU: 94: transmit queue 3 timed out 5032 ms.*Recommended Action=NONE`,
			`ErrorCode:pci_power_insufficient kernel:.*mlx5_core 0000:01:00\.0: mlx5_pcie_event:326:\(pid 165\): Detected insufficient power on the PCIe slot \(27W\).*Recommended Action=NONE`,
			`ErrorCode:port_module_high_temp kernel:.*mlx5_core 0000:5c:00\.0: Port module event\[error\]: module 0, Cable error, High Temperature.*Recommended Action=NONE`,
			`ErrorCode:access_reg_failed kernel:.*mlx5_core 0000:d2:00\.0: mlx5_cmd_out_err:838:\(pid 1441871\): ACCESS_REG\(0x805\) op_mod\(0x1\) failed.*Recommended Action=NONE`,
			`ErrorCode:module_unplugged kernel:.*mlx5_core 0000:63:00\.1: Port module event: module 1, Cable unplugged.*Recommended Action=NONE`,
			`ErrorCode:mlx5_rx_timeout_detected kernel:.*mlx5_core 0000:65:00\.0 ens15np0: RX timeout on channel: 20.*Recommended Action=NONE`,
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, messages)

		t.Log("Verifying SysLogsNICDriverError Kubernetes events contain all non-fatal NIC driver patterns")
		require.Eventually(t, func() bool {
			return helpers.VerifyEventsMatchPatterns(t, ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsNotHealthy", expectedEventPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node events should contain all non-fatal NIC driver patterns")

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

		if originalArgs, ok := ctx.Value(keyOriginalArgs).([]string); ok && originalArgs != nil {
			helpers.RestoreDaemonSetArgs(ctx, t, client, helpers.SyslogDaemonSetName,
				helpers.SyslogContainerName, originalArgs)
		}

		t.Logf("Sending healthy SysLogsNICDriverError event to clear condition on node %s", nodeName)
		healthyEvent := helpers.NewHealthEvent(nodeName).
			WithAgent("syslog-health-monitor").
			WithCheckName("SysLogsNICDriverError").
			WithComponentClass("NIC").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithEntitiesImpacted([]helpers.EntityImpacted{})
		helpers.SendHealthEvent(ctx, t, healthyEvent)

		require.Eventually(t, func() bool {
			condition, conditionErr := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsHealthy")
			if conditionErr != nil {
				t.Logf("Failed to check node condition: %v", conditionErr)
				return false
			}
			return condition != nil && condition.Status == v1.ConditionFalse
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"SysLogsNICDriverError condition should be cleared")

		t.Logf("Verifying node %s is uncordoned after SysLogsNICDriverError recovery", nodeName)
		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		if err := helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName); err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestSyslogHealthMonitorNAPISoftLockupDetection verifies the stateful
// multi-line mlx5 NAPI soft-lockup detection end to end: a "BUG: soft lockup"
// header followed — as separate journal entries — by an mlx5e stack frame
// must emit a fatal mlx5_napi_soft_lockup event that cordons the node, while
// the driver's own TX timeout line emits a non-fatal corroborating event.
func TestSyslogHealthMonitorNAPISoftLockupDetection(t *testing.T) {
	feature := features.New("Syslog Health Monitor - mlx5 NAPI Soft Lockup Detection").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "nic-driver-detection")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// ManagedByNVSentinel=true is required for the fault-quarantine
		// syslog ruleset to cordon this test node.
		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, true)

		ctx = context.WithValue(ctx, keySyslogNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)
		ctx = context.WithValue(ctx, keyOriginalArgs, originalArgs)

		return ctx
	})

	feature.Assess("Inject TX timeout and multi-line soft lockup dump", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keySyslogNodeName).(string)

		// Log shape of the issue #1334 incident: the driver's TX timeout
		// line, then the watchdog dump where the header and the mlx5 stack
		// evidence arrive as separate journal entries. No single entry
		// contains both the lockup header and the mlx5 attribution.
		messages := []string{
			"kernel: [249067.323011] mlx5_core 0000:65:00.0 ens15np0: TX timeout detected",
			"kernel: [249078.442331] watchdog: BUG: soft lockup - CPU#94 stuck for 226906s! [swapper/94:0]",
			"kernel: [249078.442332] Modules linked in: xt_conntrack nf_conntrack mlx5_ib mlx5_core",
			"kernel: [249078.442333] CPU: 94 PID: 0 Comm: swapper/94 Tainted: G             L 5.15.0-1053-azure #61-Ubuntu",
			"kernel: [249078.442334] RIP: 0010:mlx5e_poll_ico_cq+0x8b/0x1a0 [mlx5_core]",
			"kernel: [249078.442335]  mlx5e_napi_poll+0x142/0x680 [mlx5_core]",
			"kernel: [249078.442336]  net_rx_action+0x142/0x2a0",
		}

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, messages)

		t.Log("Verifying fatal mlx5_napi_soft_lockup node condition with parsed header fields")
		expectedConditionPatterns := []string{
			`ErrorCode:mlx5_napi_soft_lockup.*CPU#94 stuck for 226906s in mlx5 NAPI poll loop.*mlx5e_poll_ico_cq\+0x8b/0x1a0.*Recommended Action=REPLACE_VM`,
		}
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsNotHealthy", expectedConditionPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node condition should contain the mlx5 NAPI soft lockup pattern")

		t.Log("Verifying the non-fatal TX timeout corroborating event")
		expectedEventPatterns := []string{
			`ErrorCode:mlx5_tx_timeout_detected kernel:.*mlx5_core 0000:65:00\.0 ens15np0: TX timeout detected.*Recommended Action=NONE`,
		}
		require.Eventually(t, func() bool {
			return helpers.VerifyEventsMatchPatterns(t, ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsNotHealthy", expectedEventPatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node events should contain the non-fatal TX timeout pattern")

		t.Log("Verifying the fatal soft lockup event cordons the node")
		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{
					Key:         helpers.QuarantineHealthEventAnnotationKey,
					Pattern:     "mlx5_napi_soft_lockup",
					ShouldExist: true,
				},
			},
		})

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

		if originalArgs, ok := ctx.Value(keyOriginalArgs).([]string); ok && originalArgs != nil {
			helpers.RestoreDaemonSetArgs(ctx, t, client, helpers.SyslogDaemonSetName,
				helpers.SyslogContainerName, originalArgs)
		}

		t.Logf("Sending healthy SysLogsNICDriverError event to clear condition on node %s", nodeName)
		healthyEvent := helpers.NewHealthEvent(nodeName).
			WithAgent("syslog-health-monitor").
			WithCheckName("SysLogsNICDriverError").
			WithComponentClass("NIC").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithEntitiesImpacted([]helpers.EntityImpacted{})
		helpers.SendHealthEvent(ctx, t, healthyEvent)

		require.Eventually(t, func() bool {
			condition, conditionErr := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsNICDriverError", "SysLogsNICDriverErrorIsHealthy")
			if conditionErr != nil {
				t.Logf("Failed to check node condition: %v", conditionErr)
				return false
			}
			return condition != nil && condition.Status == v1.ConditionFalse
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"SysLogsNICDriverError condition should be cleared")

		t.Logf("Verifying node %s is uncordoned after soft lockup recovery", nodeName)
		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", nodeName)
		if err := helpers.RemoveNodeManagedByNVSentinelLabel(ctx, client, nodeName); err != nil {
			t.Logf("Warning: failed to remove ManagedByNVSentinel label: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
