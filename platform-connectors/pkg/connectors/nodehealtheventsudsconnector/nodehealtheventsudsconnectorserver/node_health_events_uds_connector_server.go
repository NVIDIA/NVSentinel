// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package nodehealtheventsudsconnectorserver

import (
	"context"
	"log"
	"net"
	"os"

	nodeHealthEventsPluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/nodehealtheventsudscore"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/klog/v2"
)

type NodeHealthEventsUDSConnectorServer struct {
	nodeHealthEventsPluginPb.UnimplementedNodeHealthEventsUDSConnectorServer
	healthEventChan chan *nodeHealthEventsPluginPb.HealthEvents
}

// HealthEventStreamV1 streams HealthEvents to the client
func (s *NodeHealthEventsUDSConnectorServer) HealthEventStreamV1(empty *emptypb.Empty,
	stream nodeHealthEventsPluginPb.NodeHealthEventsUDSConnector_HealthEventStreamV1Server) error {
	// Signal that the client has connected
	klog.Infof("Client connected, starting to stream events...")

	for {
		select {
		case event := <-s.healthEventChan:
			if err := stream.Send(event); err != nil {
				klog.Errorf("Error sending event: %v", err)
				return err
			}
		case <-stream.Context().Done():
			klog.Infof("Client disconnected")
			return nil
		}
	}
}

func NewNodeHealthEventsUDSConnectorServer(healthEventChan chan *nodeHealthEventsPluginPb.HealthEvents) *NodeHealthEventsUDSConnectorServer { //nolint:lll
	return &NodeHealthEventsUDSConnectorServer{
		healthEventChan: healthEventChan,
	}
}

func serveNodeHealthEventsOverUDS(s *grpc.Server, nodeHealthEventsListener net.Listener) {
	if err := s.Serve(nodeHealthEventsListener); err != nil {
		log.Fatalf("nodeHealthEventsServer Failed to serve: %v", err)
	}
}

func CreateAndStartNodeHealthEventsUDSServer(nodeHealthEventsSocket *string, nodeHealthEventsListener *net.Listener,
	nodeHealthEventsRingBuffer *ringbuffer.RingBuffer, stopCh chan struct{},
	ctx context.Context) *nodehealtheventsudscore.NodeHealthEventsUDSConnector {
	os.Remove(*nodeHealthEventsSocket)

	var err error
	// Create Device plugin Socket
	*nodeHealthEventsListener, err = net.Listen("unix", *nodeHealthEventsSocket)

	if err != nil {
		klog.Fatalf("Error creating nodeHealthEventsSocket %s", err)
		return nil
	}

	server.InitializeAndAttachRingBufferForConnectors(nodeHealthEventsRingBuffer)

	healthEventChan := make(chan *nodeHealthEventsPluginPb.HealthEvents, 1000)
	nodeHealthEventsUDSConnector := nodehealtheventsudscore.InitializeNodeHealthEventsUDSConnector(
		nodeHealthEventsRingBuffer, *nodeHealthEventsListener, stopCh, ctx, healthEventChan)

	var opts []grpc.ServerOption
	s := grpc.NewServer(opts...)
	nodeHealthEventsServer := NewNodeHealthEventsUDSConnectorServer(healthEventChan)
	nodeHealthEventsPluginPb.RegisterNodeHealthEventsUDSConnectorServer(s, nodeHealthEventsServer)
	klog.Infof("Server is listening on %s", *nodeHealthEventsSocket)

	go serveNodeHealthEventsOverUDS(s, *nodeHealthEventsListener)
	go nodeHealthEventsUDSConnector.FetchAndProcessHealthMetric(ctx)

	return nodeHealthEventsUDSConnector
}
