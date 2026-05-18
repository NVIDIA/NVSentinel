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
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
)

const (
	agentName = "slurm-drain-monitor"
)

// Publisher publishes health events to the platform connector via the
// shared healthpub publisher (commons/pkg/healthpub).
type Publisher struct {
	pub                *healthpub.Publisher
	processingStrategy pb.ProcessingStrategy
}

// New creates a Publisher. target must match the gRPC target string
// used to dial client (typically "unix:///var/run/nvsentinel.sock").
func New(client pb.PlatformConnectorClient, target string, processingStrategy pb.ProcessingStrategy) *Publisher {
	return &Publisher{
		pub:                healthpub.New(client, target, agentName),
		processingStrategy: processingStrategy,
	}
}

// PublishDrainEvents publishes one health event per matched reason (or one healthy event when isHealthy).
// When isHealthy, reasons can be empty; a single healthy event is sent per (nodeName, podNamespace/Name).
func (p *Publisher) PublishDrainEvents(
	ctx context.Context, reasons []parser.MatchedReason, nodeName string,
	isHealthy bool, podNamespace, podName string,
) error {
	entityValue := podName
	if podNamespace != "" {
		entityValue = fmt.Sprintf("%s/%s", podNamespace, podName)
	}

	entitiesImpacted := []*pb.Entity{
		{EntityType: "v1/Pod", EntityValue: entityValue},
	}

	var events []*pb.HealthEvent

	if isHealthy {
		if len(reasons) == 0 {
			// Fallback: no previous reasons stored, send a single generic healthy event.
			events = []*pb.HealthEvent{
				{
					Version:            1,
					Agent:              agentName,
					CheckName:          agentName,
					ComponentClass:     "NODE",
					GeneratedTimestamp: timestamppb.New(time.Now()),
					Message:            "Slurm external drain cleared",
					IsHealthy:          true,
					NodeName:           nodeName,
					RecommendedAction:  pb.RecommendedAction_NONE,
					ProcessingStrategy: p.processingStrategy,
					EntitiesImpacted:   entitiesImpacted,
				},
			}
		} else {
			// Send one healthy event per previously-matched check name.
			now := timestamppb.New(time.Now())
			for _, r := range reasons {
				events = append(events, &pb.HealthEvent{
					Version:            1,
					Agent:              agentName,
					CheckName:          r.CheckName,
					ComponentClass:     r.ComponentClass,
					GeneratedTimestamp: now,
					Message:            "Slurm external drain cleared",
					IsHealthy:          true,
					NodeName:           nodeName,
					RecommendedAction:  pb.RecommendedAction_NONE,
					ProcessingStrategy: p.processingStrategy,
					EntitiesImpacted:   entitiesImpacted,
				})
			}
		}
	} else {
		for _, r := range reasons {
			events = append(events, &pb.HealthEvent{
				Version:            1,
				Agent:              agentName,
				CheckName:          r.CheckName,
				ComponentClass:     r.ComponentClass,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				Message:            r.Message,
				IsFatal:            r.IsFatal,
				IsHealthy:          false,
				NodeName:           nodeName,
				RecommendedAction:  mapRecommendedAction(r.RecommendedAction),
				ProcessingStrategy: p.processingStrategy,
				EntitiesImpacted:   entitiesImpacted,
			})
		}
	}

	if len(events) == 0 {
		return nil
	}

	return p.pub.Publish(ctx, &pb.HealthEvents{Version: 1, Events: events})
}

func mapRecommendedAction(action string) pb.RecommendedAction {
	if action == "" {
		return pb.RecommendedAction_CONTACT_SUPPORT
	}

	if value, exists := pb.RecommendedAction_value[action]; exists {
		return pb.RecommendedAction(value)
	}

	return pb.RecommendedAction_CONTACT_SUPPORT
}
