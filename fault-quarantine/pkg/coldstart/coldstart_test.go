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

package coldstart

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type healthEventStoreStub struct {
	datastore.HealthEventStore
	findBatched func(
		ctx context.Context,
		builder datastore.QueryBuilder,
		batchSize int,
		fn func([]datastore.HealthEventWithStatus) error,
	) error
}

func (s *healthEventStoreStub) FindHealthEventsByQueryBatched(
	ctx context.Context,
	builder datastore.QueryBuilder,
	batchSize int,
	fn func([]datastore.HealthEventWithStatus) error,
) error {
	return s.findBatched(ctx, builder, batchSize, fn)
}

type eventProcessorStub struct {
	process           func(context.Context, model.HealthEventWithStatus, string) (ProcessResult, error)
	completionBatches [][]string
	completeErr       error
}

func (s *eventProcessorStub) ProcessStoredEvent(
	ctx context.Context,
	event model.HealthEventWithStatus,
	documentID string,
) (ProcessResult, error) {
	return s.process(ctx, event, documentID)
}

func (s *eventProcessorStub) CompleteStoredEvents(
	_ context.Context,
	completions []StoredEventCompletion,
) error {
	batch := make([]string, 0, len(completions))
	for i := range completions {
		batch = append(batch, completions[i].DocumentID.String)
	}

	s.completionBatches = append(s.completionBatches, batch)

	return s.completeErr
}

func replayRecord(id string) datastore.HealthEventWithStatus {
	return datastore.HealthEventWithStatus{RawEvent: datastore.Event{
		"id": id,
		"healthevent": map[string]any{
			"agent": "agent", "componentClass": "GPU", "checkName": "check", "nodeName": "node-a",
		},
		"healtheventstatus": map[string]any{},
	}}
}

func healthyReplayRecord(id string) datastore.HealthEventWithStatus {
	record := replayRecord(id)
	record.RawEvent["healthevent"].(map[string]any)["isHealthy"] = true

	return record
}

func TestStoredDocumentID_NativeDatabaseKey_PreservesValue(t *testing.T) {
	id, err := storedDocumentID(datastore.Event{"_id": 42})

	require.NoError(t, err)
	assert.Equal(t, "42", id.String)
	assert.Equal(t, 42, id.Native)
}

func TestColdStartQuery_StoredEvents_MatchesOnlyUnresolvedProcessable(t *testing.T) {
	cutoff := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	until := cutoff.Add(time.Hour)
	builder := coldStartQuery(cutoff, until)

	mongoFilter := builder.ToMongo()
	assert.Equal(t, map[string]any{
		"$and": []any{
			map[string]any{
				"createdAt": map[string]any{"$gt": cutoff},
				"$and": []any{
					map[string]any{"$or": []any{
						map[string]any{"healtheventstatus.nodequarantined": nil},
						map[string]any{"healtheventstatus.nodequarantined": ""},
						map[string]any{"healtheventstatus.nodequarantined": "NotStarted"},
					}},
					map[string]any{"$or": []any{
						map[string]any{RecoveryCompletionStatusPath: nil},
						map[string]any{RecoveryCompletionStatusPath: ""},
					}},
					map[string]any{"$or": []any{
						map[string]any{"healthevent.processingstrategy": int32(1)},
						map[string]any{"healthevent.processingStrategy": int32(1)},
						map[string]any{
							"healthevent.processingstrategy": nil,
							"healthevent.processingStrategy": nil,
						},
					}},
				},
			},
			map[string]any{"createdAt": map[string]any{"$lte": until}},
		},
	}, mongoFilter)

	sqlFilter, args := builder.ToSQL()
	assert.Contains(t, sqlFilter, "created_at > $1")
	assert.Contains(t, sqlFilter, "created_at <= $7")
	assert.Contains(t, sqlFilter, "nodequarantined")
	assert.Contains(t, sqlFilter, "faultquarantinerecovery")
	assert.Contains(t, sqlFilter, "processingstrategy")
	assert.Contains(t, sqlFilter, "processingStrategy")
	assert.Contains(t, sqlFilter, "IS NULL")
	assert.Equal(t, []any{cutoff, "", "NotStarted", "", "1", "1", until}, args)
}

func TestColdStartQuery_UnboundedLowerTimestamp_OmitsLowerBound(t *testing.T) {
	until := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	builder := coldStartQuery(time.Time{}, until)

	sqlFilter, args := builder.ToSQL()
	assert.NotContains(t, sqlFilter, "created_at >")
	assert.Contains(t, sqlFilter, "created_at <= $6")
	assert.Equal(t, until, args[len(args)-1])
}

func TestHandle_MultipleBatches_ProcessesInOrder(t *testing.T) {
	const eventCount = batchSize + 7

	events := make([]datastore.HealthEventWithStatus, eventCount)
	for i := range events {
		events[i] = replayRecord(fmt.Sprintf("event-%04d", i))
	}

	store := &healthEventStoreStub{
		findBatched: func(
			_ context.Context,
			_ datastore.QueryBuilder,
			gotBatchSize int,
			fn func([]datastore.HealthEventWithStatus) error,
		) error {
			require.Equal(t, batchSize, gotBatchSize)

			if err := fn(events[:batchSize]); err != nil {
				return err
			}

			return fn(events[batchSize:])
		},
	}

	processedIDs := make([]string, 0, eventCount)
	processor := &eventProcessorStub{
		process: func(_ context.Context, _ model.HealthEventWithStatus, documentID string) (ProcessResult, error) {
			processedIDs = append(processedIDs, documentID)

			return ProcessResultProcessed, nil
		},
	}

	err := Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	})
	require.NoError(t, err)
	require.Len(t, processedIDs, eventCount)
	assert.Equal(t, "event-0000", processedIDs[0])
	assert.Equal(t, fmt.Sprintf("event-%04d", eventCount-1), processedIDs[eventCount-1])
}

func TestHandle_ProcessesFaultsBeforeHealthyEventsAcrossBatches(t *testing.T) {
	firstBatch := []datastore.HealthEventWithStatus{
		healthyReplayRecord("healthy-old"), replayRecord("fault-old"),
	}
	secondBatch := []datastore.HealthEventWithStatus{
		healthyReplayRecord("healthy-new"), replayRecord("fault-new"),
	}
	store := &healthEventStoreStub{findBatched: func(
		_ context.Context,
		_ datastore.QueryBuilder,
		_ int,
		visit func([]datastore.HealthEventWithStatus) error,
	) error {
		if err := visit(firstBatch); err != nil {
			return err
		}

		return visit(secondBatch)
	}}
	var processed []string
	processor := &eventProcessorStub{process: func(
		_ context.Context,
		_ model.HealthEventWithStatus,
		documentID string,
	) (ProcessResult, error) {
		processed = append(processed, documentID)

		return ProcessResultProcessed, nil
	}}

	require.NoError(t, Handle(context.Background(), Dependencies{
		HealthEventStore: store, EventProcessor: processor,
	}))
	assert.Equal(t, []string{"fault-old", "fault-new", "healthy-old", "healthy-new"}, processed)
}

func TestHandle_FaultFailureDefersHealthyEventsUntilRestart(t *testing.T) {
	processErr := errors.New("temporary fault failure")
	events := []datastore.HealthEventWithStatus{
		healthyReplayRecord("healthy"), replayRecord("fault"),
	}
	store := &healthEventStoreStub{findBatched: func(
		_ context.Context,
		_ datastore.QueryBuilder,
		_ int,
		visit func([]datastore.HealthEventWithStatus) error,
	) error {
		return visit(events)
	}}
	var processed []string
	processor := &eventProcessorStub{process: func(
		_ context.Context,
		_ model.HealthEventWithStatus,
		documentID string,
	) (ProcessResult, error) {
		processed = append(processed, documentID)
		if documentID == "fault" {
			return ProcessResultFailed, processErr
		}

		return ProcessResultProcessed, nil
	}}

	err := Handle(context.Background(), Dependencies{
		HealthEventStore: store, EventProcessor: processor,
	})
	require.ErrorIs(t, err, processErr)
	assert.Equal(t, []string{"fault"}, processed,
		"a recovery must remain unresolved while the preceding fault phase is incomplete")
}

func TestHandle_EventProcessingFailure_ContinuesBatch(t *testing.T) {
	processErr := errors.New("status update failed")
	events := []datastore.HealthEventWithStatus{
		replayRecord("first"),
		replayRecord("second"),
		replayRecord("third"),
	}

	store := &healthEventStoreStub{
		findBatched: func(
			_ context.Context,
			_ datastore.QueryBuilder,
			_ int,
			fn func([]datastore.HealthEventWithStatus) error,
		) error {
			return fn(events)
		},
	}

	var processedIDs []string
	processor := &eventProcessorStub{
		process: func(_ context.Context, _ model.HealthEventWithStatus, id string) (ProcessResult, error) {
			processedIDs = append(processedIDs, id)
			if id == "second" {
				return ProcessResultFailed, processErr
			}

			return ProcessResultProcessed, nil
		},
	}

	err := Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	})
	require.ErrorIs(t, err, processErr)
	assert.Equal(t, []string{"first", "second", "third"}, processedIDs)
}

func TestHandle_InvalidStoredEvent_ContinuesBatch(t *testing.T) {
	events := []datastore.HealthEventWithStatus{
		replayRecord("invalid-one"),
		replayRecord("invalid-two"),
		replayRecord("valid"),
	}

	store := &healthEventStoreStub{
		findBatched: func(
			_ context.Context,
			_ datastore.QueryBuilder,
			_ int,
			fn func([]datastore.HealthEventWithStatus) error,
		) error {
			return fn(events)
		},
	}

	processed := 0
	processor := &eventProcessorStub{
		process: func(_ context.Context, _ model.HealthEventWithStatus, documentID string) (ProcessResult, error) {
			if documentID == "invalid-one" || documentID == "invalid-two" {
				return ProcessResultInvalid, nil
			}

			processed++

			return ProcessResultProcessed, nil
		},
	}

	require.NoError(t, Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	}))
	assert.Equal(t, 1, processed)
	assert.Equal(t, [][]string{{"invalid-one", "invalid-two"}}, processor.completionBatches)
}

func TestHandle_MalformedStoredEvent_ParsesOnceAndCompletes(t *testing.T) {
	events := []datastore.HealthEventWithStatus{
		{RawEvent: datastore.Event{"id": "malformed"}},
		replayRecord("valid"),
	}
	store := &healthEventStoreStub{findBatched: func(
		_ context.Context,
		_ datastore.QueryBuilder,
		_ int,
		fn func([]datastore.HealthEventWithStatus) error,
	) error {
		return fn(events)
	}}
	var processed []string
	processor := &eventProcessorStub{process: func(
		_ context.Context, _ model.HealthEventWithStatus, documentID string,
	) (ProcessResult, error) {
		processed = append(processed, documentID)

		return ProcessResultProcessed, nil
	}}

	require.NoError(t, Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	}))
	assert.Equal(t, []string{"valid"}, processed)
	assert.Equal(t, [][]string{{"malformed"}}, processor.completionBatches)
}

func TestHandle_CompletionPersistenceFailure_StopsRecovery(t *testing.T) {
	completionErr := errors.New("database unavailable")
	store := &healthEventStoreStub{
		findBatched: func(
			_ context.Context,
			_ datastore.QueryBuilder,
			_ int,
			fn func([]datastore.HealthEventWithStatus) error,
		) error {
			return fn([]datastore.HealthEventWithStatus{replayRecord("invalid")})
		},
	}
	processor := &eventProcessorStub{
		process: func(context.Context, model.HealthEventWithStatus, string) (ProcessResult, error) {
			return ProcessResultInvalid, nil
		},
		completeErr: completionErr,
	}

	err := Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	})
	require.ErrorIs(t, err, completionErr)
}

func TestHandle_InvalidDependencies_ReturnsError(t *testing.T) {
	processor := &eventProcessorStub{
		process: func(context.Context, model.HealthEventWithStatus, string) (ProcessResult, error) {
			return ProcessResultSkipped, nil
		},
	}

	err := Handle(context.Background(), Dependencies{EventProcessor: processor})
	assert.EqualError(t, err, "health event store is required")

	store := &healthEventStoreStub{
		findBatched: func(
			context.Context,
			datastore.QueryBuilder,
			int,
			func([]datastore.HealthEventWithStatus) error,
		) error {
			return nil
		},
	}

	err = Handle(context.Background(), Dependencies{HealthEventStore: store})
	assert.EqualError(t, err, "event processor is required")
}

func TestHandle_MissingCutoff_UsesRecoveryStart(t *testing.T) {
	var captured datastore.QueryBuilder
	store := &healthEventStoreStub{findBatched: func(
		_ context.Context,
		builder datastore.QueryBuilder,
		_ int,
		_ func([]datastore.HealthEventWithStatus) error,
	) error {
		captured = builder

		return nil
	}}
	processor := &eventProcessorStub{process: func(
		context.Context, model.HealthEventWithStatus, string,
	) (ProcessResult, error) {
		return ProcessResultProcessed, nil
	}}

	before := time.Now().UTC()
	require.NoError(t, Handle(context.Background(), Dependencies{
		HealthEventStore: store,
		EventProcessor:   processor,
	}))
	after := time.Now().UTC()

	sql, args := captured.ToSQL()
	require.Contains(t, sql, "created_at > $1")
	cutoff, ok := args[0].(time.Time)
	require.True(t, ok)
	assert.False(t, cutoff.Before(before))
	assert.False(t, cutoff.After(after))
}

func TestHandle_MissingCutoffWithUpperBoundary_UsesBoundedLookback(t *testing.T) {
	until := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	var captured datastore.QueryBuilder
	store := &healthEventStoreStub{findBatched: func(
		_ context.Context,
		builder datastore.QueryBuilder,
		_ int,
		_ func([]datastore.HealthEventWithStatus) error,
	) error {
		captured = builder

		return nil
	}}
	processor := &eventProcessorStub{process: func(
		context.Context, model.HealthEventWithStatus, string,
	) (ProcessResult, error) {
		return ProcessResultProcessed, nil
	}}

	require.NoError(t, Handle(context.Background(), Dependencies{
		HealthEventStore:   store,
		EventProcessor:     processor,
		ColdStartUntilTime: until,
	}))

	_, args := captured.ToSQL()
	require.NotEmpty(t, args)
	assert.Equal(t, until.Add(-defaultColdStartLookback), args[0])
	assert.Equal(t, until, args[len(args)-1])
}

func TestIsPermanentError_JoinedFailures_RequiresEveryFailurePermanent(t *testing.T) {
	permanentErr := PermanentError(errors.New("node no longer exists"))
	otherPermanentErr := PermanentError(errors.New("invalid CEL expression"))
	transientErr := errors.New("API server unavailable")

	assert.True(t, IsPermanentError(fmt.Errorf("evaluate event: %w", permanentErr)))
	assert.True(t, IsPermanentError(errors.Join(permanentErr, otherPermanentErr)))
	assert.False(t, IsPermanentError(errors.Join(permanentErr, transientErr)))
	assert.True(t, IsPermanentError(multierror.Append(nil, permanentErr, otherPermanentErr)))
	assert.False(t, IsPermanentError(multierror.Append(nil, permanentErr, transientErr)))
}

func TestGetRecoveryNode_RepeatedRead_UsesOneSanitizedSnapshot(t *testing.T) {
	ctx := WithRecoveryContext(context.Background())
	calls := 0
	source := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	load := func() (*corev1.Node, error) {
		calls++

		return source, nil
	}

	first, err := GetRecoveryNode(ctx, "node-a", load)
	require.NoError(t, err)
	second, err := GetRecoveryNode(ctx, "node-a", load)
	require.NoError(t, err)

	assert.Same(t, first, second)
	assert.NotSame(t, source, first)
	assert.Empty(t, first.Status)
	assert.NotEmpty(t, source.Status)
	assert.Equal(t, 1, calls)
}
