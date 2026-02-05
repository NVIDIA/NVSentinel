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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	agentName      = "preflight-nccl-loopback"
	componentClass = "Node"
	checkName      = "NCCLLoopbackTest"

	rpcTimeout = 30 * time.Second
)

type Reporter struct {
	socketPath         string
	nodeName           string
	processingStrategy pb.ProcessingStrategy
}

func NewReporter(socketPath, nodeName string, strategy pb.ProcessingStrategy) *Reporter {
	// Remove unix:// prefix if present for grpc.Dial
	socketPath = strings.TrimPrefix(socketPath, "unix://")

	return &Reporter{
		socketPath:         socketPath,
		nodeName:           nodeName,
		processingStrategy: strategy,
	}
}

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
		EntitiesImpacted:   []*pb.Entity{},
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
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 2 * time.Second,
		Factor:   1.5,
		Jitter:   0.1,
	}

	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := r.send(ctx, events)
		if err == nil {
			slog.Info("Health event sent successfully")
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
		if s.Code() == codes.Unavailable || s.Code() == codes.DeadlineExceeded ||
			s.Code() == codes.ResourceExhausted {
			return true
		}
	}

	errStr := err.Error()

	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF")
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
