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
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

const (
	agentName = "kubernetes-object-monitor"
)

type Publisher struct {
	pcClient           pb.PlatformConnectorClient
	processingStrategy pb.ProcessingStrategy
}

func New(client pb.PlatformConnectorClient, processingStrategy pb.ProcessingStrategy) *Publisher {
	return &Publisher{
		pcClient:           client,
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

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		CheckName:          policy.Name,
		ComponentClass:     policy.HealthEvent.ComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		Message:            policy.HealthEvent.Message,
		IsFatal:            policy.HealthEvent.IsFatal,
		IsHealthy:          isHealthy,
		NodeName:           nodeName,
		RecommendedAction:  mapRecommendedAction(policy.HealthEvent.RecommendedAction),
		ErrorCode:          policy.HealthEvent.ErrorCode,
		ProcessingStrategy: strategy,
		EntitiesImpacted:   entitiesImpacted,
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Publishing health event", "event", event)

	return p.sendWithRetry(ctx, healthEvents)
}

func (p *Publisher) sendWithRetry(ctx context.Context, events *pb.HealthEvents) error {
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 2 * time.Second,
		Factor:   1.5,
		Jitter:   0.1,
	}

	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := p.pcClient.HealthEventOccurredV1(ctx, events)
		if err == nil {
			slog.Info("Successfully sent health event", "events", events)
			return true, nil
		}

		if isRetryable(err) {
			slog.Warn("Retryable error sending health event", "error", err)
			return false, nil
		}

		slog.Error("Non-retryable error sending health event", "error", err)

		return false, fmt.Errorf("non-retryable error: %w", err)
	})
}

func isRetryable(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.Unavailable || s.Code() == codes.DeadlineExceeded
	}

	errStr := err.Error()

	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF")
}

func mapRecommendedAction(action string) pb.RecommendedAction {
	if value, exists := pb.RecommendedAction_value[action]; exists {
		return pb.RecommendedAction(value)
	}

	return pb.RecommendedAction_CONTACT_SUPPORT
}
