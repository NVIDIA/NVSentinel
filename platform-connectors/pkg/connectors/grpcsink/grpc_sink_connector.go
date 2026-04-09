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

// Package grpcsink provides a connector that forwards HealthEvent protos
// to an external gRPC server using the PlatformConnector service.
package grpcsink

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

const defaultRPCTimeout = 10 * time.Second

// GRPCSinkConnector forwards health events to an external gRPC server
// via the PlatformConnector HealthEventOccurredV1 RPC.
type GRPCSinkConnector struct {
	client     pb.PlatformConnectorClient
	conn       *grpc.ClientConn
	ringBuffer *ringbuffer.RingBuffer
	maxRetries int
	rpcTimeout time.Duration
}

// InitializeGRPCSinkConnector creates a connector that dials the given target
// using insecure credentials (cluster-internal network). If tokenPath is
// non-empty, a Kubernetes ServiceAccount bearer token is attached to every RPC
// (same pattern as janitor → janitor-provider, ADR-030).
func InitializeGRPCSinkConnector(
	ringBuffer *ringbuffer.RingBuffer,
	target string,
	maxRetries int,
	tokenPath string,
) (*GRPCSinkConnector, error) {
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	if tokenPath != "" {
		slog.Info("Enabling SA token authentication for gRPC sink", "tokenPath", tokenPath)
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(tokenInterceptor(tokenPath)))
	}

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client for target %s: %w", target, err)
	}

	client := pb.NewPlatformConnectorClient(conn)

	slog.Info("Initialized gRPC sink connector",
		"target", target, "maxRetries", maxRetries, "authEnabled", tokenPath != "")

	return &GRPCSinkConnector{
		client:     client,
		conn:       conn,
		ringBuffer: ringBuffer,
		maxRetries: maxRetries,
		rpcTimeout: defaultRPCTimeout,
	}, nil
}

// FetchAndProcessHealthMetric dequeues health events from the ring buffer and
// forwards them to the gRPC target. Blocks until ctx is canceled or the ring
// buffer signals shutdown.
func (g *GRPCSinkConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.Info("Context canceled, exiting gRPC sink processing loop")
			return
		default:
			queued, quit := g.ringBuffer.Dequeue()
			if quit {
				slog.Info("Queue signaled shutdown, exiting gRPC sink processing loop")
				return
			}

			if queued == nil || queued.Events == nil || len(queued.Events.GetEvents()) == 0 {
				g.ringBuffer.HealthMetricEleProcessingCompleted(queued)
				continue
			}

			err := g.sendHealthEvents(ctx, queued.Events)
			if err != nil {
				retryCount := g.ringBuffer.NumRequeues(queued)
				if retryCount < g.maxRetries {
					slog.Warn("Error forwarding health events to gRPC sink, will retry with exponential backoff",
						"error", err,
						"retryCount", retryCount,
						"maxRetries", g.maxRetries,
						"eventCount", len(queued.Events.GetEvents()))

					grpcSinkRetryCounter.Inc()
					g.ringBuffer.AddRateLimited(queued)
				} else {
					slog.Error("Max retries exceeded, dropping health events permanently",
						"error", err,
						"retryCount", retryCount,
						"maxRetries", g.maxRetries,
						"eventCount", len(queued.Events.GetEvents()),
						"firstEventNodeName", queued.Events.GetEvents()[0].GetNodeName(),
						"firstEventCheckName", queued.Events.GetEvents()[0].GetCheckName())
					g.ringBuffer.HealthMetricEleProcessingCompleted(queued)
				}
			} else {
				g.ringBuffer.HealthMetricEleProcessingCompleted(queued)
			}
		}
	}
}

func (g *GRPCSinkConnector) sendHealthEvents(ctx context.Context, healthEvents *pb.HealthEvents) error {
	start := time.Now()

	rpcCtx, cancel := context.WithTimeout(ctx, g.rpcTimeout)
	defer cancel()

	_, err := g.client.HealthEventOccurredV1(rpcCtx, healthEvents)

	duration := time.Since(start)
	grpcSinkSendDuration.Observe(float64(duration.Milliseconds()))

	if err != nil {
		grpcSinkSendCounter.WithLabelValues(statusFailed).Inc()
		return fmt.Errorf("failed to forward health events to gRPC sink: %w", err)
	}

	grpcSinkSendCounter.WithLabelValues(statusSuccess).Inc()

	slog.Debug("Successfully forwarded health events to gRPC sink",
		"eventCount", len(healthEvents.GetEvents()),
		"durationMs", duration.Milliseconds())

	return nil
}

// tokenInterceptor returns a gRPC unary client interceptor that reads a
// ServiceAccount token from tokenPath on every call and attaches it as a
// Bearer token in the "authorization" gRPC metadata header.
// The token is re-read on each invocation to handle Kubernetes token rotation.
//
// TODO: extract to commons/pkg/grpcclient and share with janitor/pkg/client.TokenInterceptor
// which has identical logic. See janitor/pkg/client/grpc_auth.go.
func tokenInterceptor(tokenPath string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		//nolint:gosec // G304: tokenPath is operator-controlled config, not user input.
		tokenBytes, err := os.ReadFile(tokenPath)
		if err != nil {
			return fmt.Errorf("reading SA token from %q: %w", tokenPath, err)
		}

		token := strings.TrimSpace(string(tokenBytes))
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// ShutdownRingBuffer drains the ring buffer and stops the processing loop.
func (g *GRPCSinkConnector) ShutdownRingBuffer() {
	if g.ringBuffer != nil {
		slog.Info("Shutting down gRPC sink connector ring buffer with drain")
		g.ringBuffer.ShutDownHealthMetricQueue()
		slog.Info("gRPC sink connector ring buffer drained successfully")
	}
}

// Close closes the underlying gRPC client connection.
func (g *GRPCSinkConnector) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}

	return nil
}
