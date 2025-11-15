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

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	maxRetries int           = 5
	delay      time.Duration = 5 * time.Second
)

type PublisherConfig struct {
	platformConnectorClient protos.PlatformConnectorClient
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
	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: delay,
		Factor:   2,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := p.platformConnectorClient.HealthEventOccurredV1(ctx, healthEvents)
		if err == nil {
			slog.Debug("Successfully sent health events", "events", healthEvents)

			return true, nil
		}

		if isRetryableError(err) {
			slog.Error("Retryable error occurred", "error", err)
			fatalEventPublishingError.WithLabelValues("retryable_error").Inc()

			return false, nil
		}

		slog.Error("Non-retryable error occurred", "error", err)
		fatalEventPublishingError.WithLabelValues("non_retryable_error").Inc()

		return false, fmt.Errorf("non retryable error occurred while sending health event: %w", err)
	})
	if err != nil {
		slog.Error("All retry attempts to send health event failed", "error", err)
		fatalEventPublishingError.WithLabelValues("event_publishing_to_UDS_error").Inc()

		return fmt.Errorf("all retry attempts to send health event failed: %w", err)
	}

	return nil
}

func NewPublisher(platformConnectorClient protos.PlatformConnectorClient) *PublisherConfig {
	return &PublisherConfig{platformConnectorClient: platformConnectorClient}
}

func (p *PublisherConfig) Publish(ctx context.Context, event *protos.HealthEvent,
	recommendedAction protos.RecommendedAction, ruleName string) error {
	newEvent := proto.Clone(event).(*protos.HealthEvent)

	newEvent.Agent = "health-events-analyzer"
	newEvent.CheckName = ruleName
	newEvent.RecommendedAction = recommendedAction
	newEvent.IsHealthy = false
	newEvent.IsFatal = true

	req := &protos.HealthEvents{
		Version: 1,
		Events:  []*protos.HealthEvent{newEvent},
	}

	return p.sendHealthEventWithRetry(ctx, req)
}
