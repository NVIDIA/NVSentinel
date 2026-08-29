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

package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

const (
	maxRetries int           = 5
	delay      time.Duration = 5 * time.Second
)

type PublisherConfig struct {
	platformConnectorClient protos.PlatformConnectorClient
	processingStrategy      protos.ProcessingStrategy
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable {
			return true
		}
	}

	return false
}

func (p *PublisherConfig) sendHealthEventWithRetry(ctx context.Context, healthEvents *protos.HealthEvents) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.grpc.publish")
	defer span.End()

	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: delay,
		Factor:   2,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := p.platformConnectorClient.HealthEventOccurredV1(ctx, healthEvents)
		if err == nil {
			slog.DebugContext(ctx, "Successfully sent health events", "events", healthEvents)

			return true, nil
		}

		if isRetryableError(err) {
			slog.ErrorContext(ctx, "Retryable error occurred", "error", err)
			fatalEventPublishingError.WithLabelValues("retryable_error").Inc()

			return false, nil
		}

		slog.ErrorContext(ctx, "Non-retryable error occurred", "error", err)
		fatalEventPublishingError.WithLabelValues("non_retryable_error").Inc()

		return false, fmt.Errorf("non retryable error occurred while sending health event: %w", err)
	})
	if err != nil {
		slog.ErrorContext(ctx, "All retry attempts to send health event failed", "error", err)
		fatalEventPublishingError.WithLabelValues("event_publishing_to_UDS_error").Inc()

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "grpc_publish_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return fmt.Errorf("all retry attempts to send health event failed: %w", err)
	}

	return nil
}

// NewPublisher creates a PublisherConfig that sends health events to the
// platform-connector via gRPC.
func NewPublisher(platformConnectorClient protos.PlatformConnectorClient,
	processingStrategy protos.ProcessingStrategy) *PublisherConfig {
	return &PublisherConfig{
		platformConnectorClient: platformConnectorClient,
		processingStrategy:      processingStrategy,
	}
}

// Publish clones the incoming health event, updates the fields defined by the
// rule (agent, check name, recommended action, isFatal, and processing strategy),
// and sends the resulting event to the platform-connector with retries.
func (p *PublisherConfig) Publish(ctx context.Context, event *protos.HealthEvent,
	recommendedAction protos.RecommendedAction, ruleName string, message string,
	rule *config.HealthEventsAnalyzerRule) error {
	return p.publish(ctx, event, publishOptions{
		recommendedAction: recommendedAction,
		ruleName:          ruleName,
		message:           message,
		rule:              rule,
	})
}

// PublishRecovery publishes the healthy transition for a derived condition.
// Error codes are intentionally empty so downstream consumers clear every
// failure for the selected derived condition and entity scope.
func (p *PublisherConfig) PublishRecovery(
	ctx context.Context,
	event *protos.HealthEvent,
	ruleName string,
	entities []*protos.Entity,
	rule *config.HealthEventsAnalyzerRule,
) error {
	return p.publish(ctx, event, publishOptions{
		recommendedAction: protos.RecommendedAction_NONE,
		ruleName:          ruleName,
		message:           fmt.Sprintf("Recovered derived condition %s", ruleName),
		rule:              rule,
		isHealthy:         true,
		entities:          entities,
	})
}

type publishOptions struct {
	recommendedAction protos.RecommendedAction
	ruleName          string
	message           string
	rule              *config.HealthEventsAnalyzerRule
	isHealthy         bool
	entities          []*protos.Entity
}

func (p *PublisherConfig) publish(ctx context.Context, event *protos.HealthEvent, options publishOptions) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.publish")
	defer span.End()

	span.SetAttributes(
		attribute.String("health_events_analyzer.publish.rule_name", options.ruleName),
		attribute.String("health_events_analyzer.publish.recommended_action", options.recommendedAction.String()),
		attribute.Bool("health_events_analyzer.publish.is_healthy", options.isHealthy),
	)

	newEvent := proto.Clone(event).(*protos.HealthEvent)

	newEvent.Agent = "health-events-analyzer"
	newEvent.CheckName = options.ruleName
	newEvent.RecommendedAction = options.recommendedAction
	newEvent.IsHealthy = options.isHealthy
	newEvent.Message = options.message

	// Default from module configuration, with an optional rule-level override.
	newEvent.ProcessingStrategy = p.processingStrategy

	if options.rule != nil && options.rule.ProcessingStrategy != "" {
		value, ok := protos.ProcessingStrategy_value[options.rule.ProcessingStrategy]
		if !ok {
			span.SetAttributes(
				attribute.String("health_events_analyzer.error.type", "invalid_processing_strategy"),
				attribute.String("health_events_analyzer.error.message",
					fmt.Sprintf("unexpected processingStrategy: %q", options.rule.ProcessingStrategy)),
			)
			tracing.RecordError(span, fmt.Errorf("unexpected processingStrategy value: %q", options.rule.ProcessingStrategy))

			return fmt.Errorf("unexpected processingStrategy value: %q", options.rule.ProcessingStrategy)
		}

		newEvent.ProcessingStrategy = protos.ProcessingStrategy(value)
	}

	switch {
	case options.isHealthy:
		newEvent.IsFatal = false
		newEvent.ErrorCode = nil
		newEvent.EntitiesImpacted = cloneEntities(options.entities)
		newEvent.QuarantineOverrides = nil
		newEvent.DrainOverrides = nil
		newEvent.CustomRecommendedAction = ""
	case options.recommendedAction == protos.RecommendedAction_NONE:
		newEvent.IsFatal = false
	default:
		newEvent.IsFatal = true
	}

	req := &protos.HealthEvents{
		Version: 1,
		Events:  []*protos.HealthEvent{newEvent},
	}

	return p.sendHealthEventWithRetry(ctx, req)
}

func cloneEntities(entities []*protos.Entity) []*protos.Entity {
	if len(entities) == 0 {
		return nil
	}

	clones := make([]*protos.Entity, 0, len(entities))
	for _, entity := range entities {
		if entity == nil {
			continue
		}

		clones = append(clones, proto.Clone(entity).(*protos.Entity))
	}

	return clones
}
