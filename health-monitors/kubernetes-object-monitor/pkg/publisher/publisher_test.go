// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

type fakePlatformConnectorClient struct {
	events *pb.HealthEvents
}

func (f *fakePlatformConnectorClient) HealthEventOccurredV1(
	_ context.Context, events *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.events = events

	return &emptypb.Empty{}, nil
}

func TestPublishHealthEventIncludesBehaviourOverrides(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := New(client, "passthrough:///platform-connector", pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	policy := &config.Policy{
		Name: "operator-pod-unhealthy",
		HealthEvent: config.HealthEventSpec{
			ComponentClass:    "Software",
			IsFatal:           true,
			Message:           "operator pod is unhealthy",
			RecommendedAction: "CONTACT_SUPPORT",
			ErrorCode:         []string{"OPERATOR_POD_UNHEALTHY"},
			QuarantineOverrides: &config.BehaviourOverridesSpec{
				Force: true,
			},
			DrainOverrides: &config.BehaviourOverridesSpec{
				Skip: true,
			},
		},
	}

	err := pub.PublishHealthEvent(context.Background(), policy, "node-1", false, nil)
	require.NoError(t, err)
	require.NotNil(t, client.events)
	require.Len(t, client.events.Events, 1)

	event := client.events.Events[0]
	require.NotNil(t, event.QuarantineOverrides)
	require.True(t, event.QuarantineOverrides.Force)
	require.False(t, event.QuarantineOverrides.Skip)
	require.NotNil(t, event.DrainOverrides)
	require.False(t, event.DrainOverrides.Force)
	require.True(t, event.DrainOverrides.Skip)
}
