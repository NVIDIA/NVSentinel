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

package server

import (
	"context"
	"log/slog"

	"github.com/golang/protobuf/ptypes/empty"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

/*
In the code coverage report, this file is contributing 0%. Reason is since the healthEvents message send
by the gpu health monitor is received by function HealthEventOccurredV1 and in order to test the functionality
completely, we need simulate the queue enqueue and dequeue operations along with initializing the
PlatformConnectorServer. it will get really complex.Hence, ignoring this file as part of unit testing for now.
*/

var ringBufferQueue []*ringbuffer.RingBuffer

// prometheus metrics
var (
	healthEventsReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "platform_connector_health_events_received_total",
		Help: "The total number of health events that the platform connector has received",
	})
)

type PlatformConnectorServer struct {
	pb.UnimplementedPlatformConnectorServer
}

func (p *PlatformConnectorServer) HealthEventOccurredV1(ctx context.Context,
	he *pb.HealthEvents) (*empty.Empty, error) {
	slog.Info("Health events received", "events", he)

	healthEventsReceived.Add(float64(len(he.Events)))

	for _, buffer := range ringBufferQueue {
		buffer.Enqueue(he)
	}

	return nil, nil
}

func InitializeAndAttachRingBufferForConnectors(buffer *ringbuffer.RingBuffer) {
	ringBufferQueue = append(ringBufferQueue, buffer)
}
