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

package publisher

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

type capturePlatformConnector struct {
	events *protos.HealthEvents
}

func (c *capturePlatformConnector) HealthEventOccurredV1(
	_ context.Context,
	events *protos.HealthEvents,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.events = proto.Clone(events).(*protos.HealthEvents)
	return &emptypb.Empty{}, nil
}

func TestPublishRecovery(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	source := &protos.HealthEvent{
		Version:                 1,
		Agent:                   "syslog-health-monitor",
		ComponentClass:          "GPU",
		CheckName:               "SysLogsXIDError",
		IsFatal:                 true,
		IsHealthy:               true,
		RecommendedAction:       protos.RecommendedAction_RESTART_BM,
		CustomRecommendedAction: "old-custom-action",
		ErrorCode:               []string{"94"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-123"},
		},
		Metadata:            map[string]string{"source": "reboot-check"},
		GeneratedTimestamp:  timestamppb.Now(),
		NodeName:            "node-a",
		QuarantineOverrides: &protos.BehaviourOverrides{Force: true},
		DrainOverrides:      &protos.BehaviourOverrides{Skip: true},
		ProcessingStrategy:  protos.ProcessingStrategy_STORE_AND_ANALYSE,
	}
	original := proto.Clone(source).(*protos.HealthEvent)
	rule := &config.HealthEventsAnalyzerRule{
		Name:               "RepeatedXID94OnSameGPU",
		ProcessingStrategy: "EXECUTE_REMEDIATION",
	}

	err := pub.PublishRecovery(context.Background(), source, rule.Name,
		[]*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-123"}}, rule)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, source), "publishing must not mutate the source event")
	require.NotNil(t, client.events)
	require.Len(t, client.events.Events, 1)

	recovery := client.events.Events[0]
	require.Equal(t, "health-events-analyzer", recovery.Agent)
	require.Equal(t, rule.Name, recovery.CheckName)
	require.True(t, recovery.IsHealthy)
	require.False(t, recovery.IsFatal)
	require.Equal(t, protos.RecommendedAction_NONE, recovery.RecommendedAction)
	require.Empty(t, recovery.CustomRecommendedAction)
	require.Empty(t, recovery.ErrorCode)
	require.Equal(t, []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-123"}}, recovery.EntitiesImpacted)
	require.Nil(t, recovery.QuarantineOverrides)
	require.Nil(t, recovery.DrainOverrides)
	require.Equal(t, protos.ProcessingStrategy_EXECUTE_REMEDIATION, recovery.ProcessingStrategy)
	require.Equal(t, source.Metadata, recovery.Metadata)
	require.True(t, proto.Equal(source.GeneratedTimestamp, recovery.GeneratedTimestamp))
	require.Equal(t, source.NodeName, recovery.NodeName)
}

func TestPublishRecoveryNodeScopeHasNoEntities(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	err := pub.PublishRecovery(context.Background(), &protos.HealthEvent{
		NodeName: "node-a",
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU_UUID", EntityValue: "GPU-123"},
		},
	}, "NodeDerivedCondition", nil, nil)
	require.NoError(t, err)
	require.Empty(t, client.events.Events[0].EntitiesImpacted)
}
