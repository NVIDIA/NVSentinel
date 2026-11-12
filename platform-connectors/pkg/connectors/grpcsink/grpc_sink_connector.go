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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

const defaultRPCTimeout = 10 * time.Second

// permanentRejectionCodes are the statuses the target answers the same way on
// every resend of the same batch, so retrying one only holds the batches
// behind it: the batch failed validation (InvalidArgument), the connector may
// not send what it sent (PermissionDenied), or the target does not serve the
// RPC (Unimplemented). Unauthenticated is not here, because the token rotates,
// and neither is ResourceExhausted: gRPC uses it for transient overload and
// quotas, and a proxy may answer it for throttling, so dropping on it could
// lose batches for good. The health event publisher uses the same set
// (commons/pkg/healthpub).
var permanentRejectionCodes = map[codes.Code]bool{
	codes.InvalidArgument:  true,
	codes.PermissionDenied: true,
	codes.Unimplemented:    true,
}

// isPermanentRejection reports whether err is a status the target would give
// every resend of the batch.
func isPermanentRejection(err error) bool {
	s, ok := status.FromError(err)

	return ok && permanentRejectionCodes[s.Code()]
}

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
	}

	dialOpts = append(dialOpts, grpcclient.DialOptions(tokenPath)...)

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
			// Shutdown stops a loop that has not emptied its queue in time;
			// the batches left behind were acknowledged when they were queued.
			if queued := g.ringBuffer.CurrentLength(); queued > 0 {
				slog.Error("Context canceled with batches still queued, dropping them permanently",
					"batchCount", queued)
			}

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

			// The batch is retried here, before the next one is taken, so the
			// sink gets a monitor's batches in the order it sent them, and a
			// send that fails at shutdown is not handed back to a queue that
			// takes no batch anymore.
			retryCount, err := g.sendWithRetry(ctx, queued.Events)
			if err != nil {
				slog.Error(dropMessage(ctx, err),
					"error", err,
					"retryCount", retryCount,
					"maxRetries", g.maxRetries,
					"eventCount", len(queued.Events.GetEvents()),
					"firstEventNodeName", queued.Events.GetEvents()[0].GetNodeName(),
					"firstEventCheckName", queued.Events.GetEvents()[0].GetCheckName())
			}

			g.ringBuffer.HealthMetricEleProcessingCompleted(queued)
		}
	}
}

// dropMessage says why the loop drops a batch: it was stopped first, the
// target refused the batch for good, or the retries ran out.
func dropMessage(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "Stopped before the batch was sent, dropping health events permanently"
	case isPermanentRejection(err):
		return "gRPC sink refused the batch, dropping health events permanently"
	default:
		return "Max retries exceeded, dropping health events permanently"
	}
}

// sendWithRetry sends one batch and, after a failed send, retries it in place
// with exponential backoff, up to maxRetries times. The loop holds the batch
// meanwhile, so no later batch reaches the sink ahead of it. The backoff is
// the one the ring buffer was configured with, each retry counts in the
// queue's retries metric as a requeue did, and a canceled context ends the
// retries. A batch the target refuses for good is not retried, so it does not
// hold the batches behind it. It returns the number of retries made.
func (g *GRPCSinkConnector) sendWithRetry(ctx context.Context, healthEvents *pb.HealthEvents) (int, error) {
	delay, maxDelay := g.ringBuffer.RetryDelays()

	for retries := 0; ; retries++ {
		err := g.sendHealthEvents(ctx, healthEvents)
		if err == nil || retries >= g.maxRetries || ctx.Err() != nil || isPermanentRejection(err) {
			return retries, err
		}

		slog.Warn("Error forwarding health events to gRPC sink, will retry in place with exponential backoff",
			"error", err,
			"retryCount", retries+1,
			"maxRetries", g.maxRetries,
			"retryDelay", delay,
			"eventCount", len(healthEvents.GetEvents()))
		grpcSinkRetryCounter.Inc()
		g.ringBuffer.CountRetry()

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return retries, err
		case <-timer.C:
		}

		delay = min(delay*2, maxDelay)
	}
}

// ProcessBatch forwards one batch to the sink with the connector's RPC
// timeout and no retry. The deployment platform connector calls it from inside
// the request, best effort, instead of through the ring buffer.
func (g *GRPCSinkConnector) ProcessBatch(ctx context.Context, healthEvents *pb.HealthEvents) error {
	if len(healthEvents.GetEvents()) == 0 {
		return nil
	}

	return g.sendHealthEvents(ctx, healthEvents)
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

// Close closes the underlying gRPC client connection.
func (g *GRPCSinkConnector) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}

	return nil
}
