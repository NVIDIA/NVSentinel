// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package publisher sends certification-failure and recovery health events to
// the local platform-connector over gRPC, tagged with the configured
// processing strategy.
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
)

const (
	agentName = "nvcre-certification-monitor"

	CheckNameNVCRECertFailed = "NVCRECertFailed"

	nodeEntityType = "v1/Node"

	componentClassNode = "Node"

	defaultMessage = "certification failure has occurred on this node, investigate the cause"
)

// PublishFunc is the signature for publishing a health event. Used for testing.
type PublishFunc func(ctx context.Context, nodeName string, isHealthy bool, message string, errorCode string) error

type Publisher struct {
	pcClient           pb.PlatformConnectorClient
	processingStrategy pb.ProcessingStrategy
	publishOverride    PublishFunc
}

// New constructs a Publisher backed by the given Platform Connector gRPC client.
func New(client pb.PlatformConnectorClient, processingStrategy pb.ProcessingStrategy) *Publisher {
	return &Publisher{
		pcClient:           client,
		processingStrategy: processingStrategy,
	}
}

// NewForTesting creates a Publisher that delegates to the provided function.
func NewForTesting(fn PublishFunc) *Publisher {
	return &Publisher{publishOverride: fn}
}

// PublishHealthEvent publishes a single per-(node, variant, reason) health event.
//
// errorCode is "<variant>/<reason>" — stable and cert-independent. Combined
// with the node entity, this lets Platform Connector and Fault Quarantine track
// and clear each failure independently.
func (p *Publisher) PublishHealthEvent(
	ctx context.Context,
	nodeName string,
	isHealthy bool,
	message string,
	errorCode string,
) error {
	if p.publishOverride != nil {
		return p.publishOverride(ctx, nodeName, isHealthy, message, errorCode)
	}

	entitiesImpacted := []*pb.Entity{
		{
			EntityType:  nodeEntityType,
			EntityValue: nodeName,
		},
	}

	isFatal := !isHealthy

	if message == "" {
		message = defaultMessage
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		CheckName:          CheckNameNVCRECertFailed,
		ComponentClass:     componentClassNode,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		Message:            message,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		NodeName:           nodeName,
		RecommendedAction:  pb.RecommendedAction_CONTACT_SUPPORT,
		ErrorCode:          []string{errorCode},
		ProcessingStrategy: p.processingStrategy,
		EntitiesImpacted:   entitiesImpacted,
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Publishing health event",
		"node", nodeName, "isHealthy", isHealthy, "errorCode", errorCode)

	if err := p.sendWithRetry(ctx, healthEvents); err != nil {
		return fmt.Errorf("failed to send health event for node %s: %w", nodeName, err)
	}

	return nil
}

func (p *Publisher) sendWithRetry(ctx context.Context, events *pb.HealthEvents) error {
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 2 * time.Second,
		Factor:   1.5,
		Jitter:   0.1,
	}

	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		_, err := p.pcClient.HealthEventOccurredV1(ctx, events)
		if err == nil {
			slog.Debug("Successfully sent health event")

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
