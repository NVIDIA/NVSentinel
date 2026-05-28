// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

const (
	agentName = "kubernetes-object-monitor"
)

// Publisher publishes health events to the platform connector via the
// shared healthpub publisher (commons/pkg/healthpub).
type Publisher struct {
	pub                *healthpub.Publisher
	processingStrategy pb.ProcessingStrategy
}

// New constructs a Publisher. target must match the gRPC target string
// used to dial client (typically "unix:///var/run/nvsentinel.sock").
func New(client pb.PlatformConnectorClient, target string, processingStrategy pb.ProcessingStrategy) *Publisher {
	return &Publisher{
		pub:                healthpub.New(client, target, agentName),
		processingStrategy: processingStrategy,
	}
}

// PublishHealthEvent publishes a health event to the platform connector.
// The resourceInfo parameter is used to populate the entitiesImpacted field,
// which allows fault-quarantine to track each resource individually.
func (p *Publisher) PublishHealthEvent(ctx context.Context,
	policy *config.Policy, nodeName string, isHealthy bool, resourceInfo *config.ResourceInfo) error {
	strategy := p.processingStrategy

	if policy.HealthEvent.ProcessingStrategy != "" {
		value, ok := pb.ProcessingStrategy_value[policy.HealthEvent.ProcessingStrategy]
		if !ok {
			return fmt.Errorf("unexpected processingStrategy value: %q", policy.HealthEvent.ProcessingStrategy)
		}

		strategy = pb.ProcessingStrategy(value)
	}

	// Build entitiesImpacted from resource info

	var entitiesImpacted []*pb.Entity

	if resourceInfo != nil {
		entityValue := resourceInfo.Name
		if resourceInfo.Namespace != "" {
			entityValue = fmt.Sprintf("%s/%s", resourceInfo.Namespace, resourceInfo.Name)
		}

		entitiesImpacted = []*pb.Entity{
			{
				EntityType:  resourceInfo.GVK(),
				EntityValue: entityValue,
			},
		}
	}

	quarantineOverrides := behaviourOverridesFromSpec(policy.HealthEvent.QuarantineOverrides)
	drainOverrides := behaviourOverridesFromSpec(policy.HealthEvent.DrainOverrides)

	event := &pb.HealthEvent{
		Version:             1,
		Agent:               agentName,
		CheckName:           policy.Name,
		ComponentClass:      policy.HealthEvent.ComponentClass,
		GeneratedTimestamp:  timestamppb.New(time.Now()),
		Message:             policy.HealthEvent.Message,
		IsFatal:             policy.HealthEvent.IsFatal,
		IsHealthy:           isHealthy,
		NodeName:            nodeName,
		RecommendedAction:   mapRecommendedAction(policy.HealthEvent.RecommendedAction),
		ErrorCode:           policy.HealthEvent.ErrorCode,
		ProcessingStrategy:  strategy,
		EntitiesImpacted:    entitiesImpacted,
		QuarantineOverrides: quarantineOverrides,
		DrainOverrides:      drainOverrides,
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Publishing health event", "event", event)

	return p.pub.Publish(ctx, healthEvents)
}

func mapRecommendedAction(action string) pb.RecommendedAction {
	if value, exists := pb.RecommendedAction_value[action]; exists {
		return pb.RecommendedAction(value)
	}

	return pb.RecommendedAction_CONTACT_SUPPORT
}

func behaviourOverridesFromSpec(spec *config.BehaviourOverridesSpec) *pb.BehaviourOverrides {
	if spec == nil {
		return nil
	}

	return &pb.BehaviourOverrides{
		Force: spec.Force,
		Skip:  spec.Skip,
	}
}
