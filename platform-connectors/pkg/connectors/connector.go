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

// Package connectors defines how the platform connector server hands a batch
// of health events to the connectors that act on it, and how connectors
// compose.
//
// A Connector receives one batch. In the node-local DaemonSet every connector
// drains its own ring buffer, so the buffer is the Connector the server
// sees: it accepts the batch at once and the connector processes it later
// with its own retries. The deployment platform connector has no queue: its
// connectors process the batch inside the request, and the reply follows
// their result.
//
// Set hands one batch to several connectors and reports every failure.
// BestEffort wraps a connector whose failure must not fail the batch. Which
// connector decides the reply is therefore visible where the set is built:
// the member that is not wrapped.
package connectors

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Connector receives one batch of health events. A nil error means the
// connector is done with the batch as far as the caller is concerned: queued,
// stored, forwarded or counted, as the connector defines it.
type Connector interface {
	ProcessBatch(ctx context.Context, he *pb.HealthEvents) error
}

// Set hands one batch to every member at the same time and waits for all of
// them. Every member runs to completion on the caller's context: each one's
// work is idempotent and stands on its own, so a resend of the batch finds it
// done. The result joins every failure, so the log line names each member
// that failed. A member whose failure must not fail the batch is wrapped in
// BestEffort.
type Set []Connector

// ProcessBatch implements Connector.
func (s Set) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	errs := make([]error, len(s))

	var wg sync.WaitGroup

	for i, c := range s {
		wg.Go(func() {
			errs[i] = c.ProcessBatch(ctx, he)
		})
	}

	wg.Wait()

	return errors.Join(errs...)
}

// Reasons a best-effort connector did not finish a batch.
const (
	reasonFailed   = "failed"
	reasonTimedOut = "timeout"
)

var bestEffortFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "platform_connector_best_effort_failures_total",
	Help: "Batches a best-effort connector did not finish although the batch was acknowledged, " +
		"by connector and reason (failed, timeout)",
}, []string{"connector", "reason"})

// BestEffort returns a Connector that gives c at most timeout per batch (no
// bound when timeout is zero) and never fails the batch: a failure or a
// timeout is logged and counted under name, and the batch is acknowledged
// anyway. It is for work that repairs itself, like node conditions the next
// report rewrites, and for forwards nobody waits for. When the caller's own
// context ends first, the connector was cut short with the request and
// nothing is counted: the batch is not acknowledged and will be resent.
func BestEffort(name string, c Connector, timeout time.Duration) Connector {
	return &bestEffort{name: name, next: c, timeout: timeout}
}

type bestEffort struct {
	name    string
	next    Connector
	timeout time.Duration
}

func (b *bestEffort) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	boundedCtx := ctx

	if b.timeout > 0 {
		var cancel context.CancelFunc

		boundedCtx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}

	err := b.next.ProcessBatch(boundedCtx, he)
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		// The request ended: the batch is not acknowledged and will be
		// resent, so this is not a failure of ours.
		slog.DebugContext(ctx, "Connector cut short with the request", "connector", b.name, "error", err)

		return nil
	}

	reason := reasonFailed
	if errors.Is(boundedCtx.Err(), context.DeadlineExceeded) {
		reason = reasonTimedOut
	}

	bestEffortFailures.WithLabelValues(b.name, reason).Inc()
	slog.WarnContext(ctx, "Connector did not finish the batch; the batch is acknowledged anyway",
		"connector", b.name, "reason", reason, "error", err, "eventCount", len(he.GetEvents()))

	return nil
}
