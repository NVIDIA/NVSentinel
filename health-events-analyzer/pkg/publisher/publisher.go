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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	agentName = "health-events-analyzer"

	maxRetries int           = 5
	delay      time.Duration = 5 * time.Second

	// Carries the triggering event's generated timestamp, which the derived event replaces
	// with its own.
	sourceGeneratedTimestampMetadataKey = "source_generated_timestamp"
)

type PublisherConfig struct {
	pub                *healthpub.Publisher
	processingStrategy protos.ProcessingStrategy
}

// sendHealthEventWithRetry forwards health events through the shared
// healthpub publisher, which retries with the policy set in NewPublisher (see
// healthpub.Publish for what is retried in each mode). A batch the server
// refuses for good comes back as healthpub.ErrPublishRejected and is returned
// as is, so the caller marks the event processed instead of replaying it
// forever.
func (p *PublisherConfig) sendHealthEventWithRetry(ctx context.Context, healthEvents *protos.HealthEvents) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.grpc.publish")
	defer span.End()

	if err := p.pub.Publish(ctx, healthEvents); err != nil {
		if errors.Is(err, healthpub.ErrPublishRejected) {
			slog.ErrorContext(ctx, "Platform connector rejected the health event for good; it will not be retried",
				"error", err)
			fatalEventPublishingError.WithLabelValues("event_rejected").Inc()
			tracing.RecordError(span, err)

			return err
		}

		slog.ErrorContext(ctx, "All retry attempts to send health event failed", "error", err)
		fatalEventPublishingError.WithLabelValues("event_publishing_to_UDS_error").Inc()

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "grpc_publish_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return fmt.Errorf("all retry attempts to send health event failed: %w", err)
	}

	slog.DebugContext(ctx, "Successfully sent health events", "events", healthEvents)

	return nil
}

// NewPublisher creates a PublisherConfig that sends health events to the
// platform-connector via gRPC. opts are forwarded to healthpub.New, main
// passing the option healthpub.DialFromEnvOr returns. The publisher target is
// empty because the analyzer never gated sends on the node-local socket file:
// the retry policy alone reproduces its previous socket-mode behavior.
func NewPublisher(platformConnectorClient protos.PlatformConnectorClient,
	processingStrategy protos.ProcessingStrategy, opts ...healthpub.Option) *PublisherConfig {
	pubOpts := append([]healthpub.Option{
		healthpub.WithRetryPolicy(maxRetries, delay, 2, 0.1),
	}, opts...)

	return &PublisherConfig{
		pub:                healthpub.New(platformConnectorClient, "", agentName, pubOpts...),
		processingStrategy: processingStrategy,
	}
}

// Close shuts down the publisher and the connection it owns.
func (p *PublisherConfig) Close() {
	p.pub.CloseOrWarn()
}

// Publish clones the incoming health event, updates the fields defined by the
// rule (agent, check name, recommended action, isFatal, and processing strategy),
// stamps its own generated timestamp, and sends the resulting event to the
// platform-connector with retries.
func (p *PublisherConfig) Publish(ctx context.Context, event *protos.HealthEvent,
	recommendedAction protos.RecommendedAction, ruleName string, message string,
	rule *config.HealthEventsAnalyzerRule) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.publish")
	defer span.End()

	span.SetAttributes(
		attribute.String("health_events_analyzer.publish.rule_name", ruleName),
		attribute.String("health_events_analyzer.publish.recommended_action", recommendedAction.String()),
	)

	newEvent := proto.Clone(event).(*protos.HealthEvent)

	// The source's idempotency key belongs to ingestion. Current platform
	// connectors stamp their own key on the derived event, but during a chart
	// upgrade a node-local connector of the previous version can still be
	// serving this node: it stores metadata as received, and the inherited key
	// would make the unique index refuse the derived event.
	delete(newEvent.Metadata, datastore.HealthEventIdempotencyKeyMetadataField)

	newEvent.Agent = agentName
	newEvent.CheckName = ruleName
	newEvent.RecommendedAction = recommendedAction
	newEvent.IsHealthy = false
	newEvent.Message = message

	// The clone inherits the triggering event's timestamp, which dates the derived event to
	// the original fault rather than to detection. The source value is kept in metadata.
	if src := event.GetGeneratedTimestamp(); src != nil {
		if newEvent.Metadata == nil {
			newEvent.Metadata = make(map[string]string, 1)
		}

		newEvent.Metadata[sourceGeneratedTimestampMetadataKey] = src.AsTime().UTC().Format(time.RFC3339Nano)
	}

	newEvent.GeneratedTimestamp = timestamppb.New(time.Now())

	// Default from module configuration, with an optional rule-level override.
	newEvent.ProcessingStrategy = p.processingStrategy

	if rule != nil && rule.ProcessingStrategy != "" {
		value, ok := protos.ProcessingStrategy_value[rule.ProcessingStrategy]
		if !ok {
			span.SetAttributes(
				attribute.String("health_events_analyzer.error.type", "invalid_processing_strategy"),
				attribute.String("health_events_analyzer.error.message",
					fmt.Sprintf("unexpected processingStrategy: %q", rule.ProcessingStrategy)),
			)
			tracing.RecordError(span, fmt.Errorf("unexpected processingStrategy value: %q", rule.ProcessingStrategy))

			return fmt.Errorf("unexpected processingStrategy value: %q", rule.ProcessingStrategy)
		}

		newEvent.ProcessingStrategy = protos.ProcessingStrategy(value)
	}

	if recommendedAction == protos.RecommendedAction_NONE {
		newEvent.IsFatal = false
	} else {
		newEvent.IsFatal = true
	}

	req := &protos.HealthEvents{
		Version: 1,
		Events:  []*protos.HealthEvent{newEvent},
	}

	return p.sendHealthEventWithRetry(ctx, req)
}
