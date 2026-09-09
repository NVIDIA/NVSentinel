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

	multierror "github.com/hashicorp/go-multierror"
	"github.com/nvidia/nvsentinel/commons/pkg/healthstatus"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	corev1 "k8s.io/api/core/v1"
)

const (
	batchSize                = 1000
	defaultColdStartLookback = 24 * time.Hour
	// RecoveryCompletionStatusPath stores terminal decisions that cold-start
	// scans must not replay.
	RecoveryCompletionStatusPath = healthstatus.FaultQuarantineRecoveryPath
	// RecoveryCompletionValue is shared by all terminal decisions. The detailed
	// result remains available through the cold-start metric label.
	RecoveryCompletionValue = "completed"
)

type recoveryContextKey struct{}

type recoveryState struct {
	mu    sync.Mutex
	nodes map[string]recoveryNode
}

type recoveryNode struct {
	node *corev1.Node
	err  error
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
	if err == nil {
		return false
	}

	// Check the direct marker before walking wrappers. errors.As cannot distinguish
	// a permanent wrapper from a joined permanent-plus-transient child.
	if _, ok := err.(*permanentError); ok { //nolint:errorlint // Directness is required for all-errors classification.
		return true
	}

	if aggregate, ok := err.(*multierror.Error); ok { //nolint:errorlint // Multierror's chain hides the current child.
		return allPermanentErrors(aggregate.Errors)
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return allPermanentErrors(joined.Unwrap())
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsPermanentError(wrapped.Unwrap())
	}

	return false
}

func allPermanentErrors(errs []error) bool {
	if len(errs) == 0 {
		return false
	}

	for _, err := range errs {
		if !IsPermanentError(err) {
			return false
		}
	}

	return true
}

// WithRecoveryContext marks event processing as cold-start replay. Consumers
// can use it to bypass eventually-consistent caches between ordered events.
func WithRecoveryContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryContextKey{}, &recoveryState{
		nodes: make(map[string]recoveryNode),
	})
}

// IsRecoveryContext reports whether the current event came from cold start.
func IsRecoveryContext(ctx context.Context) bool {
	_, recovering := ctx.Value(recoveryContextKey{}).(*recoveryState)

	return recovering
}

// GetRecoveryNode returns one consistent API-server snapshot per node and
// replayed event. The first caller loads it; reconciler and rule evaluators
// then share the cached value (including a load error).
func GetRecoveryNode(
	ctx context.Context,
	nodeName string,
	load func() (*corev1.Node, error),
) (*corev1.Node, error) {
	state, ok := ctx.Value(recoveryContextKey{}).(*recoveryState)
	if !ok {
		return load()
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if result, exists := state.nodes[nodeName]; exists {
		return result.node, result.err
	}

	node, err := load()
	if err == nil && node != nil {
		node = node.DeepCopy()
		node.Status = corev1.NodeStatus{}
	}

	state.nodes[nodeName] = recoveryNode{node: node, err: err}

	return node, err
}

type ProcessResult string

const (
	ProcessResultProcessed  ProcessResult = "processed"
	ProcessResultSkipped    ProcessResult = "skipped"
	ProcessResultSuperseded ProcessResult = "superseded"
	ProcessResultInvalid    ProcessResult = "invalid"
	ProcessResultFailed     ProcessResult = "failed"
)

// StoredDocumentID carries both the service-facing string form and the native
// datastore key. MongoDB bulk filters require the native ObjectID, while
// PostgreSQL uses the same UUID string for both forms.
type StoredDocumentID struct {
	String string
	Native any
}

// StoredEventCompletion associates a terminal recovery result with its durable
// datastore key.
type StoredEventCompletion struct {
	DocumentID StoredDocumentID
	Result     ProcessResult
}

type EventProcessor interface {
	ProcessStoredEvent(
		ctx context.Context,
		event model.HealthEventWithStatus,
		documentID string,
	) (ProcessResult, error)
	CompleteStoredEvents(
		ctx context.Context,
		completions []StoredEventCompletion,
	) error
}

type Dependencies struct {
	HealthEventStore   datastore.HealthEventStore
	EventProcessor     EventProcessor
	ColdStartAfterTime time.Time
	ColdStartUntilTime time.Time
}

type recoveryProgress struct {
	firstErr error
	failures int
}

type storedRecoveryEvent struct {
	record   datastore.HealthEventWithStatus
	parsed   model.HealthEventWithStatus
	parseErr error
}

// Handle replays unresolved current state. Faults are applied before healthy
// recoveries so an older recovery cannot transiently uncordon a node before a
// newer fault in the same scan is restored. Events that overlap newer state
// are projected or skipped; terminal decisions are persisted for later scans.
func Handle(ctx context.Context, deps Dependencies) error {
	if deps.HealthEventStore == nil {
		return fmt.Errorf("health event store is required")
	}

	if deps.EventProcessor == nil {
		return fmt.Errorf("event processor is required")
	}

	startedAt := time.Now().UTC()
	defer func() {
		metrics.ColdStartDuration.Observe(time.Since(startedAt).Seconds())
	}()

	slog.InfoContext(ctx, "Recovering unresolved fault-quarantine events")

	if deps.ColdStartAfterTime.IsZero() {
		if deps.ColdStartUntilTime.IsZero() {
			deps.ColdStartAfterTime = startedAt
		} else {
			// Callers should supply their startup watermark. Keep a bounded fallback
			// for direct use so an upper bound cannot produce an empty recovery window.
			deps.ColdStartAfterTime = deps.ColdStartUntilTime.Add(-defaultColdStartLookback)
		}
	}

	resolver := newSupersessionResolver(deps.HealthEventStore, deps.ColdStartUntilTime)
	progress := &recoveryProgress{}

	recoveryQuery := coldStartQuery(deps.ColdStartAfterTime, deps.ColdStartUntilTime)

	err := deps.HealthEventStore.FindHealthEventsByQueryBatched(
		ctx,
		recoveryQuery,
		batchSize,
		func(events []datastore.HealthEventWithStatus) error {
			return progress.recoverBatch(ctx, resolver, deps.EventProcessor, events, false)
		},
	)
	if err != nil {
		return fmt.Errorf("fault-quarantine cold start failed: %w", err)
	}

	// If any fault failed transiently, do not run recoveries against incomplete
	// fault state. They remain unresolved and will retry on the next startup.
	if progress.firstErr == nil {
		err = deps.HealthEventStore.FindHealthEventsByQueryBatched(
			ctx,
			recoveryQuery,
			batchSize,
			func(events []datastore.HealthEventWithStatus) error {
				return progress.recoverBatch(ctx, resolver, deps.EventProcessor, events, true)
			},
		)
		if err != nil {
			return fmt.Errorf("fault-quarantine healthy recovery phase failed: %w", err)
		}
	}

	if progress.firstErr != nil {
		return fmt.Errorf("fault-quarantine cold start completed with %d event failures: %w",
			progress.failures, progress.firstErr)
	}

	slog.InfoContext(ctx, "Fault-quarantine event recovery completed")

	return nil
}

func (p *recoveryProgress) recoverBatch(
	ctx context.Context,
	resolver *supersessionResolver,
	processor EventProcessor,
	events []datastore.HealthEventWithStatus,
	recoverHealthy bool,
) error {
	selected := make([]storedRecoveryEvent, 0, len(events))
	for i := range events {
		candidate := parseStoredRecoveryEvent(events[i])

		isHealthy := candidate.parseErr == nil && candidate.parsed.HealthEvent != nil &&
			candidate.parsed.HealthEvent.GetIsHealthy()
		if isHealthy != recoverHealthy {
			continue
		}

		selected = append(selected, candidate)
	}

	return p.recoverEvents(ctx, resolver, processor, selected)
}

func (p *recoveryProgress) recoverEvents(
	ctx context.Context,
	resolver *supersessionResolver,
	processor EventProcessor,
	events []storedRecoveryEvent,
) error {
	completions := make([]StoredEventCompletion, 0, len(events))

	for i := range events {
		completion, err := recoverStoredEvent(ctx, resolver, processor, events[i])
		if completion != nil {
			completions = append(completions, *completion)
		}

		if err := p.recordFailure(ctx, err); err != nil {
			return err
		}
	}

	if len(completions) == 0 {
		return nil
	}

	if err := processor.CompleteStoredEvents(ctx, completions); err != nil {
		return recordRecoveryFailure(fmt.Errorf("failed to record recovery completion batch: %w", err))
	}

	for i := range completions {
		metrics.ColdStartEvents.WithLabelValues(string(completions[i].Result)).Inc()
	}

	return nil
}

func (p *recoveryProgress) recordFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	p.failures++
	if p.firstErr == nil {
		p.firstErr = err
	}

	slog.ErrorContext(ctx, "Stored event recovery failed; continuing with the batch", "error", err)

	return nil
}

func recoverStoredEvent(
	ctx context.Context,
	resolver *supersessionResolver,
	processor EventProcessor,
	event storedRecoveryEvent,
) (*StoredEventCompletion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	documentID, idErr := storedDocumentID(event.record.RawEvent)

	if event.parseErr != nil {
		slog.ErrorContext(ctx, "Skipping invalid stored health event",
			"documentID", documentID.String, "error", event.parseErr)

		if idErr != nil {
			return nil, recordRecoveryFailure(fmt.Errorf(
				"cannot identify invalid stored event: %w", idErr))
		}

		return &StoredEventCompletion{DocumentID: documentID, Result: ProcessResultInvalid}, nil
	}

	if idErr != nil {
		return nil, recordRecoveryFailure(fmt.Errorf("cannot identify stored event: %w", idErr))
	}

	superseded, effects, err := resolver.resolve(
		ctx, event.parsed, event.record.CreatedAt, documentID.String)
	if err != nil {
		return nil, recordRecoveryFailure(fmt.Errorf("failed to resolve stored event state: %w", err))
	}

	if superseded {
		return &StoredEventCompletion{DocumentID: documentID, Result: ProcessResultSuperseded}, nil
	}

	result, err := processor.ProcessStoredEvent(
		withRecoveryEffects(ctx, effects), event.parsed, documentID.String)
	if err != nil {
		return nil, recordRecoveryFailure(fmt.Errorf("failed to recover stored event: %w", err))
	}

	if result == ProcessResultSkipped || result == ProcessResultInvalid {
		return &StoredEventCompletion{DocumentID: documentID, Result: result}, nil
	}

	metrics.ColdStartEvents.WithLabelValues(string(result)).Inc()

	return nil, nil
}

func parseStoredRecoveryEvent(record datastore.HealthEventWithStatus) storedRecoveryEvent {
	parsed, err := parseStoredRecord(record)

	return storedRecoveryEvent{record: record, parsed: parsed, parseErr: err}
}

func storedDocumentID(event datastore.Event) (StoredDocumentID, error) {
	stringID, stringErr := utils.ExtractDocumentID(event)

	nativeID, nativeErr := utils.ExtractDocumentIDNative(event)
	if stringErr != nil || nativeErr != nil {
		return StoredDocumentID{}, errors.Join(stringErr, nativeErr)
	}

	return StoredDocumentID{String: stringID, Native: nativeID}, nil
}

func recordRecoveryFailure(err error) error {
	metrics.ColdStartEvents.WithLabelValues(string(ProcessResultFailed)).Inc()

	return err
}

func coldStartQuery(coldStartAfter, coldStartUntil time.Time) *query.Builder {
	unresolved := query.Or(
		query.Eq("healtheventstatus.nodequarantined", nil),
		query.Eq("healtheventstatus.nodequarantined", ""),
		query.Eq("healtheventstatus.nodequarantined", string(model.StatusNotStarted)),
	)
	condition := query.And(unresolved, recoveryIncompleteCondition(), processableCondition())

	if !coldStartAfter.IsZero() {
		condition = query.And(query.Gt("createdAt", coldStartAfter), condition)
	}

	if !coldStartUntil.IsZero() {
		condition = query.And(condition, query.Lte("createdAt", coldStartUntil))
	}

	return query.New().Build(condition)
}

func recoveryIncompleteCondition() query.Condition {
	return query.Or(
		query.Eq(RecoveryCompletionStatusPath, nil),
		query.Eq(RecoveryCompletionStatusPath, ""),
	)
}
