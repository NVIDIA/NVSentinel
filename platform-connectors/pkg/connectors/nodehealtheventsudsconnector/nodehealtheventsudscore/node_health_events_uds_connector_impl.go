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

package nodehealtheventsudscore

import (
	"context"
	"net"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
)

type NodeHealthEventsUDSConnector struct {
	// ringBuffer are client for pushing data to the resource count sink
	ringBuffer      *ringbuffer.RingBuffer
	stopCh          <-chan struct{}
	ctx             context.Context
	listener        net.Listener
	healthEventChan chan *pb.HealthEvents
}

func NewNodeHealthEventsUDSConnector(
	ringBuffer *ringbuffer.RingBuffer,
	listener net.Listener,
	stopCh <-chan struct{}, ctx context.Context, healthEventChan chan *pb.HealthEvents) *NodeHealthEventsUDSConnector {
	return &NodeHealthEventsUDSConnector{
		ringBuffer:      ringBuffer,
		stopCh:          stopCh,
		ctx:             ctx,
		listener:        listener,
		healthEventChan: healthEventChan,
	}
}

func InitializeNodeHealthEventsUDSConnector(ringbuffer *ringbuffer.RingBuffer, listener net.Listener,
	stopCh <-chan struct{}, ctx context.Context, healthEventChan chan *pb.HealthEvents) *NodeHealthEventsUDSConnector {
	nodeHealthEventsUDSConnector := NewNodeHealthEventsUDSConnector(ringbuffer, listener,
		stopCh, ctx, healthEventChan)
	return nodeHealthEventsUDSConnector
}

func (r *NodeHealthEventsUDSConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-r.stopCh:
			klog.Infof("k8sConnector queue received stop signal")
			return
		default:
			healthEvents := r.ringBuffer.Dequeue()
			r.ProcessHealthEvents(ctx, healthEvents)
			r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
		}
	}
}
