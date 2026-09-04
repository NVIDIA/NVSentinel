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
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// capturingClient records the HealthEvents sent through HealthEventOccurredV1.
type capturingClient struct {
	sent []*pb.HealthEvents
}

func (c *capturingClient) HealthEventOccurredV1(
	_ context.Context, in *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.sent = append(c.sent, in)

	return &emptypb.Empty{}, nil
}

func TestPublishHealthEvent_EmptyMessageUsesFallback(t *testing.T) {
	client := &capturingClient{}
	p := New(client, pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	err := p.PublishHealthEvent(context.Background(), "node-a", false, "", "nccl-all-gather/WorkloadFailed")
	require.NoError(t, err)
	require.Len(t, client.sent, 1)
	require.Len(t, client.sent[0].Events, 1)

	ev := client.sent[0].Events[0]
	require.Equal(t, defaultMessage, ev.Message)
	require.Equal(t, agentName, ev.Agent)
	require.Equal(t, CheckNameNVCRECertFailed, ev.CheckName)
	require.Equal(t, componentClassNode, ev.ComponentClass)
	require.Equal(t, "node-a", ev.NodeName)
	require.True(t, ev.IsFatal)
	require.False(t, ev.IsHealthy)
	require.Equal(t, []string{"nccl-all-gather/WorkloadFailed"}, ev.ErrorCode)
	require.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, ev.RecommendedAction)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, ev.ProcessingStrategy)
	require.Len(t, ev.EntitiesImpacted, 1)
	require.Equal(t, nodeEntityType, ev.EntitiesImpacted[0].EntityType)
	require.Equal(t, "node-a", ev.EntitiesImpacted[0].EntityValue)
}

func TestPublishHealthEvent_NonEmptyMessagePreserved(t *testing.T) {
	client := &capturingClient{}
	p := New(client, pb.ProcessingStrategy_STORE_ONLY)

	msg := `Threshold "busBandwidthGBps" violated: measured 12.34, expression: value >= 400`

	err := p.PublishHealthEvent(context.Background(), "node-b", false, msg, "nccl-all-reduce/ThresholdViolation")
	require.NoError(t, err)
	require.Len(t, client.sent, 1)

	ev := client.sent[0].Events[0]
	require.Equal(t, msg, ev.Message)
	require.Equal(t, pb.ProcessingStrategy_STORE_ONLY, ev.ProcessingStrategy)
}

func TestPublishHealthEvent_HealthyEventIsNotFatal(t *testing.T) {
	client := &capturingClient{}
	p := New(client, pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	err := p.PublishHealthEvent(context.Background(), "node-c", true, "", "nccl-all-gather/WorkloadFailed")
	require.NoError(t, err)
	require.Len(t, client.sent, 1)

	ev := client.sent[0].Events[0]
	require.True(t, ev.IsHealthy)
	require.False(t, ev.IsFatal)
	require.Equal(t, defaultMessage, ev.Message)
}
