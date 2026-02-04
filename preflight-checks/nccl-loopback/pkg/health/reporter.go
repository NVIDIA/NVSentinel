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

package health

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	agentName      = "preflight-nccl-loopback"
	componentClass = "GPU"
	checkName      = "NCCLLoopbackTest"

	maxRetries    = 5
	initialDelay  = 2 * time.Second
	backoffFactor = 1.5
	rpcTimeout    = 30 * time.Second
)

// Reporter sends health events to the Platform Connector.
type Reporter struct {
	socketPath         string
	nodeName           string
	processingStrategy pb.ProcessingStrategy
}

// NewReporter creates a new health event reporter.
func NewReporter(socketPath, nodeName string, strategy pb.ProcessingStrategy) *Reporter {
	// Remove unix:// prefix if present for grpc.Dial
	socketPath = strings.TrimPrefix(socketPath, "unix://")

	return &Reporter{
		socketPath:         socketPath,
		nodeName:           nodeName,
		processingStrategy: strategy,
	}
}

// SendEvent sends a health event to the Platform Connector.
func (r *Reporter) SendEvent(ctx context.Context, isHealthy, isFatal bool, message string, errorCode string) error {
	recommendedAction := pb.RecommendedAction_NONE
	if !isHealthy {
		recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
	}

	var errorCodes []string
	if errorCode != "" {
		errorCodes = []string{errorCode}
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		ComponentClass:     componentClass,
		CheckName:          checkName,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		Message:            message,
		RecommendedAction:  recommendedAction,
		ErrorCode:          errorCodes,
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           r.nodeName,
		ProcessingStrategy: r.processingStrategy,
		EntitiesImpacted: []*pb.Entity{
			{
				EntityType:  "NODE",
				EntityValue: r.nodeName,
			},
		},
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Sending health event",
		"is_healthy", isHealthy,
		"is_fatal", isFatal,
		"message", message,
		"error_code", errorCode,
		"recommended_action", pb.RecommendedAction_name[int32(recommendedAction)])

	return r.sendWithRetries(ctx, healthEvents)
}

func (r *Reporter) sendWithRetries(ctx context.Context, events *pb.HealthEvents) error {
	delay := initialDelay

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := r.send(ctx, events)
		if err == nil {
			slog.Info("Health event sent successfully")
			return nil
		}

		lastErr = err

		slog.Warn("Failed to send health event",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err)

		if attempt < maxRetries-1 {
			time.Sleep(delay)
			delay = time.Duration(float64(delay) * backoffFactor)
		}
	}

	return fmt.Errorf("failed to send health event after %d retries: %w", maxRetries, lastErr)
}

func (r *Reporter) send(ctx context.Context, events *pb.HealthEvents) error {
	conn, err := grpc.NewClient(
		"unix://"+r.socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to platform connector: %w", err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	_, err = client.HealthEventOccurredV1(ctx, events)
	if err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	return nil
}
