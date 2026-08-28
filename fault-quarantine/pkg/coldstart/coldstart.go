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

const (
	batchSize = 1000
	// RecoveryCompletionStatusPath stores terminal cold-start decisions.
	RecoveryCompletionStatusPath = "healtheventstatus.faultquarantinerecovery"
)

type recoveryContextKey struct{}

type recoveryState struct {
	mu           sync.Mutex
	err          error
	permanentErr error
}

type permanentError struct {
	err error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// PermanentError marks an event-specific processing error that retrying the
// same stored event cannot resolve.
func PermanentError(err error) error {
	if err == nil {
		return nil
	}

	return &permanentError{err: err}
}

// IsPermanentError reports whether an error is deterministic for the event.
func IsPermanentError(err error) bool {
	var target *permanentError

	return errors.As(err, &target)
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

// RecordPermanentError records an event-specific failure that should not
// block every subsequent startup.
func RecordPermanentError(ctx context.Context, err error) {
	state, ok := ctx.Value(recoveryContextKey{}).(*recoveryState)
	if !ok || err == nil {
		return
	}

	state.mu.Lock()
	state.permanentErr = errors.Join(state.permanentErr, err)
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

// RecordedPermanentError returns deterministic errors recorded while
// processing the current recovered event.
func RecordedPermanentError(ctx context.Context) error {
	state, ok := ctx.Value(recoveryContextKey{}).(*recoveryState)
	if !ok {
		return nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	return state.permanentErr
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
	CompleteStoredEvent(
		ctx context.Context,
		event datastore.HealthEventWithStatus,
		result ProcessResult,
	) error
}

type Dependencies struct {
	HealthEventStore   datastore.HealthEventStore
	EventProcessor     EventProcessor
	ColdStartAfterTime time.Time
	ColdStartUntilTime time.Time
}

// Handle replays unresolved events in creation order. Events that overlap
// newer state are skipped so obsolete history cannot cause transient node
// changes. Processing stops on transient failures; terminal skip decisions
// are persisted and excluded from later scans.
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
				if err := recoverStoredEvent(ctx, resolver, deps.EventProcessor, events[i]); err != nil {
					return err
				}
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

func recoverStoredEvent(
	ctx context.Context,
	resolver *supersessionResolver,
	processor EventProcessor,
	event datastore.HealthEventWithStatus,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	superseded, err := resolver.superseded(ctx, event)
	if err != nil {
		return recordRecoveryFailure(fmt.Errorf("failed to resolve stored event state: %w", err))
	}

	if superseded {
		if err := completeStoredEvent(ctx, processor, event, ProcessResultSuperseded); err != nil {
			return recordRecoveryFailure(err)
		}

		metrics.ColdStartEvents.WithLabelValues(string(ProcessResultSuperseded)).Inc()

		return nil
	}

	result, err := processor.ProcessStoredEvent(ctx, event)
	if err != nil {
		return recordRecoveryFailure(fmt.Errorf("failed to recover stored event: %w", err))
	}

	if result == ProcessResultSkipped || result == ProcessResultInvalid {
		if err := completeStoredEvent(ctx, processor, event, result); err != nil {
			return recordRecoveryFailure(err)
		}
	}

	metrics.ColdStartEvents.WithLabelValues(string(result)).Inc()

	return nil
}

func recordRecoveryFailure(err error) error {
	metrics.ColdStartEvents.WithLabelValues(string(ProcessResultFailed)).Inc()

	return err
}

func completeStoredEvent(
	ctx context.Context,
	processor EventProcessor,
	event datastore.HealthEventWithStatus,
	result ProcessResult,
) error {
	if err := processor.CompleteStoredEvent(ctx, event, result); err != nil {
		return fmt.Errorf("failed to record %s recovery completion: %w", result, err)
	}

	return nil
}

func coldStartQuery(coldStartAfter, coldStartUntil time.Time) *query.Builder {
	unresolved := query.Or(
		query.Eq("healtheventstatus.nodequarantined", nil),
		query.Eq("healtheventstatus.nodequarantined", ""),
		query.Eq("healtheventstatus.nodequarantined", string(model.StatusNotStarted)),
	)
	recoveryIncomplete := query.Or(
		query.Eq(RecoveryCompletionStatusPath, nil),
		query.Eq(RecoveryCompletionStatusPath, ""),
	)
	condition := query.And(unresolved, recoveryIncomplete, processableCondition())

	if !coldStartAfter.IsZero() {
		condition = query.And(query.Gt("createdAt", coldStartAfter), condition)
	}

	if !coldStartUntil.IsZero() {
		condition = query.And(condition, query.Lte("createdAt", coldStartUntil))
	}

	return query.New().Build(condition)
}
