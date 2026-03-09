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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
)

const (
	agentName = "slurm-drain-monitor"
)

// Publisher publishes health events to the platform connector.
type Publisher struct {
	pcClient           pb.PlatformConnectorClient
	processingStrategy pb.ProcessingStrategy
}

// New creates a Publisher.
func New(client pb.PlatformConnectorClient, processingStrategy pb.ProcessingStrategy) *Publisher {
	return &Publisher{
		pcClient:           client,
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

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  events,
	}

	slog.Info("Publishing health events", "count", len(events), "node", nodeName, "isHealthy", isHealthy)

	return p.sendWithRetry(ctx, healthEvents)
}

func (p *Publisher) sendWithRetry(ctx context.Context, events *pb.HealthEvents) error {
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 2 * time.Second,
		Factor:   1.5,
		Jitter:   0.1,
	}

	var lastErr error

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, lastErr = p.pcClient.HealthEventOccurredV1(ctx, events)
		if lastErr == nil {
			slog.Info("Successfully sent health events", "count", len(events.Events))
			return true, nil
		}

		if isRetryable(lastErr) {
			slog.Warn("Retryable error sending health events", "error", lastErr)

			return false, nil
		}

		slog.Error("Non-retryable error sending health events", "error", lastErr)

		return false, fmt.Errorf("non-retryable error: %w", lastErr)
	})

	if err != nil && lastErr != nil && !errors.Is(err, lastErr) {
		return fmt.Errorf("%w: last error: %w", err, lastErr)
	}

	return err
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
	if action == "" {
		return pb.RecommendedAction_CONTACT_SUPPORT
	}

	if value, exists := pb.RecommendedAction_value[action]; exists {
		return pb.RecommendedAction(value)
	}

	return pb.RecommendedAction_CONTACT_SUPPORT
}
