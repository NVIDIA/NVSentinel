/*
* Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package tests

import (
	"context"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/nodehealtheventsudsconnectorserver"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/utils/strings/slices"
	"time"

	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	nodeHealthEventsPluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"
)

const testSocketPath = "/tmp/nodehealthevents.sock"

func TestHealthEventStreamV1(t *testing.T) {
	healthEvents := []*pb.HealthEvent{

		&pb.HealthEvent{
			CheckName:          "GpuPcieWatch",
			IsHealthy:          true,
			EntitiesImpacted:   []*pb.Entity{},
			ErrorCode:          []string{},
			IsFatal:            false,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_UNKNOWN,
			Message:            "Pcie watch error on GPU 0",
		},

		&pb.HealthEvent{
			CheckName:          "GpuXidError",
			IsHealthy:          true,
			Message:            "",
			EntitiesImpacted:   []*pb.Entity{},
			ErrorCode:          []string{},
			IsFatal:            false,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_NONE,
		},
		&pb.HealthEvent{
			CheckName:          "GpuPcieWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*pb.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_UNKNOWN,
			Message:            "Pcie error on GPU 0",
		},

		&pb.HealthEvent{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			Message:            "",
			EntitiesImpacted:   []*pb.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_REPORT_ISSUE,
		},

		&pb.HealthEvent{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			Message:            "",
			EntitiesImpacted:   []*pb.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"45"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_NONE,
		},
		&pb.HealthEvent{
			CheckName:          "GpuThermalWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*pb.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  pb.RecommenedAction_UNKNOWN,
			Message:            "Thermal watch error on GPU 0",
		},
	}
	nodeHealthEventsSocket := testSocketPath
	var nodeHealthEventsListener net.Listener
	nodeHealthEventsListener = nil
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// Setup the server
	nodeHealthEventsRingBuffer := ringbuffer.NewRingBuffer("nodeHealthEventsBuffer", ctx)
	nodeHealthEventsUDSConnector := nodehealtheventsudsconnectorserver.CreateAndStartNodeHealthEventsUDSServer(&nodeHealthEventsSocket, &nodeHealthEventsListener, nodeHealthEventsRingBuffer, "node", stopCh, ctx)
	opts := grpc.WithTransportCredentials(insecure.NewCredentials())

	conn, err := grpc.NewClient("unix://"+nodeHealthEventsSocket, opts)
	if err != nil {
		t.Errorf("Failed to create client: %v", err)
	}
	defer conn.Close()

	// Setup the client
	client := nodeHealthEventsPluginPb.NewNodeHealthEventsUDSConnectorClient(conn)

	stream, err := client.HealthEventStreamV1(ctx, &emptypb.Empty{})

	if err != nil {
		t.Errorf("nodehealtheventsclient Error calling ReceiveMessages: %v", err)
	}

	go func() {
		for _, healthEvent := range healthEvents {
			nodeHealthEventsUDSConnector.ProcessHealthEvents(ctx, healthEvent)
		}
	}()
	for index, healthEvent := range healthEvents {

		receivedHealthEvent, err := stream.Recv()

		if err != nil {
			t.Errorf("Error receiving message: %v", err)
		}
		assert.Equal(t, receivedHealthEvent.Message, healthEvent.Message)
		assert.Equal(t, receivedHealthEvent.CheckName, healthEvent.CheckName)
		assert.Equal(t, receivedHealthEvent.RecommendedAction.String(), healthEvent.RecommendedAction.String())
		if len(receivedHealthEvent.ImpactedIndices) != len(healthEvent.EntitiesImpacted) {
			t.Errorf(" testcase %d receivedHealthEvent.ImpactedIndices length %d is not equal to healthEvent.EntitiesImpacted length %d", index, len(receivedHealthEvent.ImpactedIndices), len(healthEvent.EntitiesImpacted))
		}

		for i, recievedEntity := range receivedHealthEvent.ImpactedIndices {
			healthEntity := healthEvent.EntitiesImpacted[i]
			if recievedEntity.EntityType != healthEntity.EntityType {
				t.Errorf("testcase %d  recievedEntity.EntityType %v not matching with healthEntity.EntityType %v", index, recievedEntity.EntityType, healthEntity.EntityType)
			}
			if recievedEntity.EntityValue != healthEntity.EntityValue {
				t.Errorf("testcase %d  recievedEntity.EntityValue %v not matching with healthEntity.EntityValue %v", index, recievedEntity.EntityValue, healthEntity.EntityValue)
			}
		}
		assert.Equal(t, receivedHealthEvent.IsFatal, healthEvent.IsFatal)
		if !slices.Equal(receivedHealthEvent.ErrorCode, healthEvent.ErrorCode) {
			t.Errorf("index %d deepak. receivedHealthEvent.ErrorCode is %v and healthEvent.ErrorCode is %v", index, receivedHealthEvent.ErrorCode, healthEvent.ErrorCode)
		}
		assert.Equal(t, receivedHealthEvent.IsHealthy, healthEvent.IsHealthy)
	}
	// Assert the received event
	cancel()
}
