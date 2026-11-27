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

// Package publisher sends health events to the platform connector through the
// shared healthpub publisher (commons/pkg/healthpub).
package publisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/metrics"
)

// AgentName is the agent on every event.
const AgentName = "nvhv-health-monitor"

// Publisher publishes batches of health events and counts the outcome.
type Publisher struct {
	pub      *healthpub.Publisher
	nodeName string
}

// New creates a Publisher. target is the platform connector socket target
// (typically "unix:///var/run/nvsentinel.sock"); opts are forwarded to
// healthpub.New, the initializer passing the option healthpub.DialFromEnvOr
// returns.
func New(client pb.PlatformConnectorClient, target, nodeName string, opts ...healthpub.Option) *Publisher {
	return &Publisher{
		pub:      healthpub.New(client, target, AgentName, opts...),
		nodeName: nodeName,
	}
}

// Close shuts down the publisher and the connection it owns.
func (p *Publisher) Close() {
	p.pub.CloseOrWarn()
}

// WaitingOnServer reports that a publish is waiting for the platform
// connector, which is the server being away, not the caller hanging.
func (p *Publisher) WaitingOnServer() bool {
	return p.pub.WaitingOnServer()
}

// Publish sends the events as one batch. It returns nil once the batch is
// delivered, or when the platform connector refuses it for good, because
// retrying would get the same answer; any other error is to be retried.
func (p *Publisher) Publish(ctx context.Context, events []*pb.HealthEvent) error {
	if err := p.pub.Publish(ctx, &pb.HealthEvents{Version: 1, Events: events}); err != nil {
		metrics.PublishFailures.WithLabelValues(p.nodeName).Inc()

		if errors.Is(err, healthpub.ErrPublishRejected) {
			slog.Error("Platform connector rejected the health events for good; not retrying them",
				"events", len(events), "error", err)

			return nil
		}

		return fmt.Errorf("failed to publish %d health events to the platform connector: %w", len(events), err)
	}

	for _, e := range events {
		metrics.EventsPublished.WithLabelValues(p.nodeName, e.GetCheckName(), strconv.FormatBool(e.GetIsFatal())).Inc()
	}

	return nil
}
