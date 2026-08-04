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
	"testing"

	"github.com/google/uuid"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

type platformConnectorDedupContextKey string

const (
	keyPlatformConnectorDedupNodeName platformConnectorDedupContextKey = "platformConnectorDedupNodeName"
	platformConnectorDedupCheckName   string                           = "PlatformConnectorDedupTestXIDError"
)

func TestPlatformConnectorDeduplicatesRepeatedHealthEvents(t *testing.T) {
	feature := features.New("Platform Connector - Health Event Deduplication").
		WithLabel("suite", "platform-connector").
		WithLabel("component", "dedup")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/syslog-xid-cordon-configmap.yaml")

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		ctx = context.WithValue(ctx, keyPlatformConnectorDedupNodeName, nodeName)

		return ctx
	})

	feature.Assess("Repeated health events trigger quarantine only for unique faults",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			nodeName, ok := ctx.Value(keyPlatformConnectorDedupNodeName).(string)
			if !ok || nodeName == "" {
				t.Fatalf("missing %s in test context", keyPlatformConnectorDedupNodeName)
				return ctx
			}

			events := make([]*helpers.HealthEventTemplate, 0, 26)
			baseGPUUUID := "GPU-" + uuid.NewString()
			baseXID := helpers.SyslogXIDEventOptions{
				NodeName:          nodeName,
				XID:               "95",
				PCI:               "0002:00:00",
				GPUUUID:           baseGPUUUID,
				RecommendedAction: pb.RecommendedAction_RESTART_VM,
			}
			for i := 0; i < 10; i++ {
				event := baseXID
				event.KernelTimestamp = 16450076 + i
				event.PID = 123456 + i
				event.ProcessName = "nvidia-smi"
				event.Detail = "GPU recovery action required."
				events = append(events, helpers.NewSyslogXIDEvent(event))
			}

			distinctPCIs := []string{"0003:00:00", "0004:00:00", "0005:00:00", "0006:00:00", "0007:00:00"}
			for i, pci := range distinctPCIs {
				gpuUUID := "GPU-" + uuid.NewString()
				xid := helpers.SyslogXIDEventOptions{
					NodeName:          nodeName,
					XID:               "95",
					PCI:               pci,
					GPUUUID:           gpuUUID,
					RecommendedAction: pb.RecommendedAction_RESTART_VM,
				}

				first := xid
				first.KernelTimestamp = 1645010 + i
				first.PID = 223456 + i
				first.ProcessName = "cuda-app"
				first.Detail = "GPU recovery action required."

				second := xid
				second.KernelTimestamp = 1645020 + i
				second.PID = 323456 + i
				second.ProcessName = "cuda-app-retry"
				second.Detail = "GPU recovery action required."
				second.ReverseEntityOrder = true

				events = append(events,
					helpers.NewSyslogXIDEvent(first),
					helpers.NewSyslogXIDEvent(second),
				)
			}

			xid119GPUUUID := "GPU-" + uuid.NewString()
			xid119 := helpers.SyslogXIDEventOptions{
				NodeName:          nodeName,
				XID:               "119",
				PCI:               "0008:00:00",
				GPUUUID:           xid119GPUUUID,
				RecommendedAction: pb.RecommendedAction_COMPONENT_RESET,
			}
			for i := 0; i < 4; i++ {
				event := xid119
				event.KernelTimestamp = 1645030 + i
				event.PID = 423456 + i
				event.ProcessName = "nvc:[driver]"
				event.Detail = "Timeout after 6s of waiting for RPC response from GPU GSP."
				events = append(events, helpers.NewSyslogXIDEvent(event))
			}

			otherXid119GPUUUID := "GPU-" + uuid.NewString()
			otherXID119 := helpers.SyslogXIDEventOptions{
				NodeName:          nodeName,
				XID:               "119",
				PCI:               "0009:00:00",
				GPUUUID:           otherXid119GPUUUID,
				RecommendedAction: pb.RecommendedAction_COMPONENT_RESET,
			}

			first119 := otherXID119
			first119.KernelTimestamp = 1645040
			first119.PID = 523456
			first119.ProcessName = "nvc:[driver]"
			first119.Detail = "Timeout after 6s of waiting for RPC response from GPU GSP."

			second119 := otherXID119
			second119.KernelTimestamp = 1645041
			second119.PID = 523457
			second119.ProcessName = "nvc:[driver-retry]"
			second119.Detail = "Timeout after 6s of waiting for RPC response from GPU GSP."
			second119.ReverseEntityOrder = true

			events = append(events,
				helpers.NewSyslogXIDEvent(first119),
				helpers.NewSyslogXIDEvent(second119),
			)

			for _, event := range events {
				event.WithCheckName(platformConnectorDedupCheckName)
				helpers.SendHealthEvent(ctx, t, event)
			}

			t.Logf("Waiting for fault-quarantine annotation to show only unique faults: XID 95 count=6, XID 119 count=2")
			require.Eventually(t, func() bool {
				summary, err := helpers.SummarizeQuarantineAnnotation(ctx, client, nodeName)
				if err != nil {
					t.Logf("failed to count quarantine annotation events: %v", err)
					return false
				}
				t.Logf("Current quarantine annotation summary: total=%d XID95=%d XID119=%d",
					summary.Total, summary.ByErrorCode["95"], summary.ByErrorCode["119"])

				return summary.Total == 8 &&
					summary.ByErrorCode["95"] == 6 &&
					summary.ByErrorCode["119"] == 2
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
				"quarantine annotation should contain exactly the unique faults that remained EXECUTE_REMEDIATION")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		if nodeName, ok := ctx.Value(keyPlatformConnectorDedupNodeName).(string); ok && nodeName != "" {
			t.Logf("Sending check-level healthy %s event to clear quarantine annotation", platformConnectorDedupCheckName)
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(nodeName).
				WithAgent("syslog-health-monitor").
				WithCheckName(platformConnectorDedupCheckName).
				WithComponentClass("GPU").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("No Health Failures").
				WithEntitiesImpacted([]helpers.EntityImpacted{}).
				WithProcessingStrategy(int(pb.ProcessingStrategy_EXECUTE_REMEDIATION)))

			assert.Eventually(t, func() bool {
				summary, err := helpers.SummarizeQuarantineAnnotation(ctx, client, nodeName)
				if err != nil {
					t.Logf("failed to count quarantine annotation events during cleanup: %v", err)
					return false
				}

				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("failed to get node during cleanup: %v", err)
					return false
				}

				t.Logf("Waiting for cleanup: current quarantine annotation total=%d XID95=%d XID119=%d cordoned=%v",
					summary.Total, summary.ByErrorCode["95"], summary.ByErrorCode["119"], node.Spec.Unschedulable)

				return summary.Total == 0 && !node.Spec.Unschedulable
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
				"quarantine annotation should be cleared and node should be uncordoned")
		}

		helpers.RestoreQuarantineConfig(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
