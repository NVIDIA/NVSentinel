//go:build amd64_group
// +build amd64_group

// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	keyCancellationsCMBackup  contextKey = "cancellationsConfigMapBackup"
	keyCancellationsTestNode  contextKey = "cancellationsTestNode"
	keyCancellationsSyslogPod contextKey = "cancellationsSyslogPod"
	keyCancellationsStopChan  contextKey = "cancellationsStopChan"
	keyCancellationsOrigArgs  contextKey = "cancellationsOriginalArgs"
)

// TestSyslogHealthMonitorCancellationRules: rule "79 cancels 119" — XID 79
// must clear XID 119 from the node condition without touching unrelated
// XID 94 on the same GPU.
func TestSyslogHealthMonitorCancellationRules(t *testing.T) {
	feature := features.New("Syslog Health Monitor - Cancellation Rules").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "cancellation-rules")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		backup, err := helpers.BackupConfigMap(ctx, client,
			helpers.SyslogConfigMapName, helpers.NVSentinelNamespace)
		require.NoError(t, err, "failed to back up syslog-health-monitor ConfigMap")

		ctx = context.WithValue(ctx, keyCancellationsCMBackup, backup)

		require.NoError(t, helpers.SetSyslogCancellationRules(ctx, client,
			[]helpers.SyslogCheckCancellations{
				{
					Name:    "SysLogsXIDError",
					Enabled: true,
					Rules: []helpers.SyslogCancellationRule{
						{OnErrorCode: "79", CancelErrorCodes: []string{"119"}},
					},
				},
			},
		), "failed to update cancellations ConfigMap")

		// Pod restart inside SetUpSyslogHealthMonitor picks up the patched config.
		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, false)

		ctx = context.WithValue(ctx, keyCancellationsTestNode, testNodeName)
		ctx = context.WithValue(ctx, keyCancellationsSyslogPod, syslogPod.Name)
		ctx = context.WithValue(ctx, keyCancellationsStopChan, stopChan)
		ctx = context.WithValue(ctx, keyCancellationsOrigArgs, originalArgs)

		return ctx
	})

	feature.Assess("Inject XID 119 and XID 94 — both appear in node condition", func(
		ctx context.Context, t *testing.T, c *envconf.Config,
	) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyCancellationsTestNode).(string)

		// XID 119 → COMPONENT_RESET. XID 94 → CONTACT_SUPPORT (overridden in
		// values-tilt.yaml). Both fatal, both on GPU-2 (PCI:0002:00:00).
		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 94, pid=789012, name=process, Contained ECC error.",
		})

		expectedSequencePatterns := []string{
			`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,
			`ErrorCode:94 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 94.*?Recommended Action=CONTACT_SUPPORT`,
		}

		t.Log("Verifying node condition contains both XID 119 and XID 94 on GPU-2")
		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", expectedSequencePatterns)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node condition should contain both XID 119 and XID 94 entries on GPU-2")

		return ctx
	})

	feature.Assess("Inject XID 79 — synthetic cancellation clears XID 119 only", func(
		ctx context.Context, t *testing.T, c *envconf.Config,
	) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyCancellationsTestNode).(string)

		// XID 79 is itself fatal so it surfaces as a new entry; the synthetic
		// healthy XID 119 event fans out alongside it and clears XID 119.
		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 79, pid=123456, name=test-cancel, GPU has fallen off the bus.",
		})

		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy")
			if err != nil || condition == nil {
				return false
			}

			msg := condition.Message

			return strings.Contains(msg, "ErrorCode:79") &&
				strings.Contains(msg, "ErrorCode:94") &&
				entryCount(msg, "ErrorCode:119") == 0
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"XID 119 should be cleared, XID 94 preserved, XID 79 surfaced")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyCancellationsTestNode).(string)
		syslogPod := ctx.Value(keyCancellationsSyslogPod).(string)
		stopChan := ctx.Value(keyCancellationsStopChan).(chan struct{})
		originalArgs, _ := ctx.Value(keyCancellationsOrigArgs).([]string)

		// Restore the ConfigMap; t.Errorf (not t.Logf) — a leaked rule would
		// poison every subsequent test in the same run.
		if backup, ok := ctx.Value(keyCancellationsCMBackup).([]byte); ok && backup != nil {
			if restoreErr := helpers.ReplaceConfigMapFromBackup(
				ctx, client, backup,
				helpers.SyslogConfigMapName, helpers.NVSentinelNamespace,
			); restoreErr != nil {
				t.Errorf("failed to restore syslog-health-monitor ConfigMap: %v", restoreErr)
			}
		}

		helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, syslogPod)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// entryCount counts ";"-separated message entries containing substr.
func entryCount(message, substr string) int {
	count := 0

	for _, part := range strings.Split(message, ";") {
		if strings.Contains(part, substr) {
			count++
		}
	}

	return count
}

// TestSyslogHealthMonitorCancellationUncordonsNode: rule "13 cancels 119" —
// fatal XID 119 cordons the node; non-fatal XID 13 emits a synthetic healthy
// XID 119 that uncordons it. XID 13 (RecommendedAction=NONE) is itself
// non-fatal so it doesn't satisfy fault-quarantine's cordon CEL on its own.
func TestSyslogHealthMonitorCancellationUncordonsNode(t *testing.T) {
	feature := features.New("Syslog Health Monitor - Cancellation Uncordons Node").
		WithLabel("suite", "syslog-health-monitor").
		WithLabel("component", "cancellation-uncordon")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Apply a quarantine rule that cordons fatal syslog-health-monitor
		// GPU events. The default test-cluster fault-quarantine ConfigMap
		// only matches gpu-health-monitor events, so without this our
		// XID 119 syslog event would never trigger a cordon. The setup
		// helper backs up the existing config and the matching teardown
		// (TeardownQuarantineTest) restores it.
		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/syslog-xid-cordon-configmap.yaml")

		backup, err := helpers.BackupConfigMap(ctx, client,
			helpers.SyslogConfigMapName, helpers.NVSentinelNamespace)
		require.NoError(t, err, "failed to back up syslog-health-monitor ConfigMap")

		ctx = context.WithValue(ctx, keyCancellationsCMBackup, backup)

		require.NoError(t, helpers.SetSyslogCancellationRules(ctx, client,
			[]helpers.SyslogCheckCancellations{
				{
					Name:    "SysLogsXIDError",
					Enabled: true,
					Rules: []helpers.SyslogCancellationRule{
						{OnErrorCode: "13", CancelErrorCodes: []string{"119"}},
					},
				},
			},
		), "failed to update cancellations ConfigMap")

		// setManagedByNVSentinel=true is required for fault-quarantine's
		// syslog cordon CEL to match this node.
		testNodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, true)

		ctx = context.WithValue(ctx, keyCancellationsTestNode, testNodeName)
		ctx = context.WithValue(ctx, keyCancellationsSyslogPod, syslogPod.Name)
		ctx = context.WithValue(ctx, keyCancellationsStopChan, stopChan)
		ctx = context.WithValue(ctx, keyCancellationsOrigArgs, originalArgs)

		return ctx
	})

	feature.Assess("fatal XID 119 cordones the node and writes the FQ annotation", func(
		ctx context.Context, t *testing.T, c *envconf.Config,
	) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyCancellationsTestNode).(string)

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
		})

		require.Eventually(t, func() bool {
			return helpers.VerifyNodeConditionMatchesSequence(t, ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsNotHealthy", []string{
					`ErrorCode:119 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 kernel:.*?NVRM: Xid \(PCI:0002:00:00\): 119.*?Recommended Action=COMPONENT_RESET`,
				})
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"Node condition should contain the XID 119 fault entry")

		helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
			ExpectCordoned: true,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: true},
			},
		})

		return ctx
	})

	feature.Assess("non-fatal XID 13 cancellation triggers full uncordon", func(
		ctx context.Context, t *testing.T, c *envconf.Config,
	) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyCancellationsTestNode).(string)

		helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, []string{
			"kernel: [103859.498995] NVRM: Xid (PCI:0002:00:00): 13, pid=2519562, name=python3, Graphics Exception: ChID 000c, Class 0000cbc0, Offset 00000000, Data 00000000",
		})

		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
				"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
			if err != nil {
				return false
			}

			// Condition either gone or flipped to False with no XID 119 entry.
			if condition == nil {
				return true
			}

			return condition.Status == v1.ConditionFalse &&
				entryCount(condition.Message, "ErrorCode:119") == 0
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"XID 119 should be cleared from the SysLogsXIDError condition")

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

		nodeName := ctx.Value(keyCancellationsTestNode).(string)
		syslogPod := ctx.Value(keyCancellationsSyslogPod).(string)
		stopChan := ctx.Value(keyCancellationsStopChan).(chan struct{})
		originalArgs, _ := ctx.Value(keyCancellationsOrigArgs).([]string)

		if backup, ok := ctx.Value(keyCancellationsCMBackup).([]byte); ok && backup != nil {
			if restoreErr := helpers.ReplaceConfigMapFromBackup(
				ctx, client, backup,
				helpers.SyslogConfigMapName, helpers.NVSentinelNamespace,
			); restoreErr != nil {
				t.Errorf("failed to restore syslog-health-monitor ConfigMap: %v", restoreErr)
			}
		}

		helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, syslogPod)

		// Restore the original fault-quarantine ConfigMap so subsequent
		// tests in the same run see the chart-deployed default rules.
		helpers.RestoreQuarantineConfig(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
