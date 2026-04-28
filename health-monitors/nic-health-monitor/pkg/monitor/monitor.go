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

// Package monitor hosts the orchestrator that runs registered checks on
// their polling cadence and forwards their events to the platform
// connector.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
)

// NICHealthMonitor orchestrates state-category checks.
// Every enqueued check is run once per polling cycle; events are
// batched to a single gRPC send so the platform connector sees fewer RPCs.
type NICHealthMonitor struct {
	nodeName string
	pcClient pb.PlatformConnectorClient

	stateChecks []checks.Check

	stateInterval time.Duration
}

// NewNICHealthMonitor constructs a NICHealthMonitor with the given checks.
func NewNICHealthMonitor(
	nodeName string,
	pcClient pb.PlatformConnectorClient,
	stateChecks []checks.Check,
	stateInterval time.Duration,
) *NICHealthMonitor {
	m := &NICHealthMonitor{
		nodeName:      nodeName,
		pcClient:      pcClient,
		stateChecks:   stateChecks,
		stateInterval: stateInterval,
	}

	slog.Info("NIC Health Monitor initialized",
		"state_checks", len(m.stateChecks),
		"state_interval", m.stateInterval,
	)

	return m
}

// RunStateChecks executes all state-category checks once. The context
// is threaded into the gRPC send path so that in-flight RPCs and retry
// sleeps are cancelled promptly on shutdown.
func (m *NICHealthMonitor) RunStateChecks(ctx context.Context) error {
	return m.runChecks(ctx, m.stateChecks, "state")
}

// StateInterval returns the state polling interval (used by main to
// drive the polling loop).
func (m *NICHealthMonitor) StateInterval() time.Duration { return m.stateInterval }

// runChecks executes the checks in a category and sends any resulting
// events in a single batch. Check errors are logged and do not cancel
// the remaining checks.
func (m *NICHealthMonitor) runChecks(ctx context.Context, checkList []checks.Check, category string) error {
	start := time.Now()

	for _, chk := range checkList {
		events, err := chk.Run()
		if err != nil {
			slog.Error("Check failed",
				"check", chk.Name(),
				"category", category,
				"error", err,
			)

			continue
		}

		if len(events) == 0 {
			continue
		}

		slog.Info("Check produced events", "check", chk.Name(), "count", len(events))

		batch := &pb.HealthEvents{Version: 1, Events: events}

		if err := m.sendWithRetry(ctx, batch, 5, 2*time.Second); err != nil {
			slog.Error("Failed to send health events",
				"check", chk.Name(), "error", err)

			continue
		}

		for _, evt := range events {
			slog.Info("Health event sent",
				"check", evt.CheckName,
				"is_fatal", evt.IsFatal,
				"is_healthy", evt.IsHealthy,
				"recommended_action", evt.RecommendedAction.String(),
				"entities", formatEntities(evt.EntitiesImpacted),
				"message", evt.Message,
			)

			isFatal := "false"
			if evt.IsFatal {
				isFatal = "true"
			}

			metrics.HealthEventsSent.WithLabelValues(
				m.nodeName, chk.Name(), isFatal,
			).Inc()
		}
	}

	metrics.PollCycleDuration.WithLabelValues(m.nodeName, category).
		Observe(time.Since(start).Seconds())

	return nil
}

// sendWithRetry wraps the HealthEventOccurredV1 RPC in bounded exponential
// backoff. Only transient errors (Unavailable, DeadlineExceeded, broken
// connection) are retried. The context is used for the RPC calls so
// shutdown can cancel in-flight sends.
func (m *NICHealthMonitor) sendWithRetry(
	ctx context.Context, batch *pb.HealthEvents, maxRetries int, retryDelay time.Duration,
) error {
	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: retryDelay,
		Factor:   1.5,
		Jitter:   0.1,
	}

	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		_, err := m.pcClient.HealthEventOccurredV1(ctx, batch)
		if err == nil {
			return true, nil
		}

		if isRetryable(err) {
			slog.Warn("Retryable error sending health event, will retry", "error", err)
			return false, nil
		}

		slog.Error("Non-retryable error sending health event", "error", err)

		return false, fmt.Errorf("non-retryable error: %w", err)
	})
}

// isRetryable reports whether a gRPC error is transient.
func isRetryable(err error) bool {
	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable || s.Code() == codes.DeadlineExceeded {
			return true
		}
	}

	if errors.Is(err, io.EOF) {
		return true
	}

	msg := err.Error()
	if strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "broken pipe") {
		return true
	}

	return false
}

// formatEntities produces a compact "NIC=mlx5_0, NICPort=1" string for logs.
func formatEntities(entities []*pb.Entity) string {
	if len(entities) == 0 {
		return "[]"
	}

	var b strings.Builder

	for i, e := range entities {
		if i > 0 {
			b.WriteString(", ")
		}

		fmt.Fprintf(&b, "%s=%s", e.EntityType, e.EntityValue)
	}

	return b.String()
}
