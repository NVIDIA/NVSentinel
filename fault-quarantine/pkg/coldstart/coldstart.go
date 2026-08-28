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

// Package coldstart recovers processable health events that fault-quarantine
// did not handle before its change-stream position was lost.
package coldstart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

const batchSize = 1000

type recoveryContextKey struct{}

type recoveryState struct {
	mu  sync.Mutex
	err error
}

// WithRecoveryContext marks event processing as cold-start replay. Consumers
// can use it to bypass eventually-consistent caches between ordered events.
func WithRecoveryContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryContextKey{}, &recoveryState{})
}

// IsRecoveryContext reports whether the current event came from cold start.
func IsRecoveryContext(ctx context.Context) bool {
	_, recovering := ctx.Value(recoveryContextKey{}).(*recoveryState)

	return recovering
}

// RecordError marks the current recovered event as retryable. Reconciler code
// calls this when an operation fails but its public status API must return nil.
func RecordError(ctx context.Context, err error) {
	state, ok := ctx.Value(recoveryContextKey{}).(*recoveryState)
	if !ok || err == nil {
		return
	}

	state.mu.Lock()
	state.err = errors.Join(state.err, err)
	state.mu.Unlock()
}

// Error returns errors recorded while processing the current recovered event.
func Error(ctx context.Context) error {
	state, ok := ctx.Value(recoveryContextKey{}).(*recoveryState)
	if !ok {
		return nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	return state.err
}

type ProcessResult string

const (
	ProcessResultProcessed  ProcessResult = "processed"
	ProcessResultSkipped    ProcessResult = "skipped"
	ProcessResultSuperseded ProcessResult = "superseded"
	ProcessResultInvalid    ProcessResult = "invalid"
	ProcessResultFailed     ProcessResult = "failed"
)

type EventProcessor interface {
	ProcessStoredEvent(
		ctx context.Context,
		event datastore.HealthEventWithStatus,
	) (ProcessResult, error)
}

type Dependencies struct {
	HealthEventStore   datastore.HealthEventStore
	EventProcessor     EventProcessor
	ColdStartAfterTime time.Time
	ColdStartUntilTime time.Time
}

// Handle replays unresolved events in creation order. Failures fully replaced
// by a later event are skipped so obsolete history cannot cause transient node
// changes. Processing stops on transient failures; invalid stored documents
// are counted and skipped.
func Handle(ctx context.Context, deps Dependencies) error {
	if deps.HealthEventStore == nil {
		return fmt.Errorf("health event store is required")
	}

	if deps.EventProcessor == nil {
		return fmt.Errorf("event processor is required")
	}

	startedAt := time.Now()
	defer func() {
		metrics.ColdStartDuration.Observe(time.Since(startedAt).Seconds())
	}()

	slog.InfoContext(ctx, "Recovering unresolved fault-quarantine events")
	resolver := newSupersessionResolver(deps.HealthEventStore, deps.ColdStartUntilTime)

	err := deps.HealthEventStore.FindHealthEventsByQueryBatched(
		ctx,
		coldStartQuery(deps.ColdStartAfterTime, deps.ColdStartUntilTime),
		batchSize,
		func(events []datastore.HealthEventWithStatus) error {
			for i := range events {
				if err := ctx.Err(); err != nil {
					return err
				}

				superseded, err := resolver.superseded(ctx, events[i])
				if err != nil {
					metrics.ColdStartEvents.WithLabelValues(string(ProcessResultFailed)).Inc()

					return fmt.Errorf("failed to resolve stored event state: %w", err)
				}

				if superseded {
					metrics.ColdStartEvents.WithLabelValues(string(ProcessResultSuperseded)).Inc()

					continue
				}

				result, err := deps.EventProcessor.ProcessStoredEvent(ctx, events[i])
				if err != nil {
					metrics.ColdStartEvents.WithLabelValues(string(ProcessResultFailed)).Inc()

					return fmt.Errorf("failed to recover stored event: %w", err)
				}

				metrics.ColdStartEvents.WithLabelValues(string(result)).Inc()
			}

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("fault-quarantine cold start failed: %w", err)
	}

	slog.InfoContext(ctx, "Fault-quarantine event recovery completed")

	return nil
}

func coldStartQuery(coldStartAfter, coldStartUntil time.Time) *query.Builder {
	unresolved := query.Or(
		query.Eq("healtheventstatus.nodequarantined", nil),
		query.Eq("healtheventstatus.nodequarantined", ""),
		query.Eq("healtheventstatus.nodequarantined", string(model.StatusNotStarted)),
	)
	condition := query.And(unresolved, processableCondition())

	if !coldStartAfter.IsZero() {
		condition = query.And(query.Gt("createdAt", coldStartAfter), condition)
	}

	if !coldStartUntil.IsZero() {
		condition = query.And(condition, query.Lte("createdAt", coldStartUntil))
	}

	return query.New().Build(condition)
}
