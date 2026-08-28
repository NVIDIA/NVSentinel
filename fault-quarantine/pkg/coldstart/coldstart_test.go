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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	process func(context.Context, datastore.Event) (ProcessResult, error)
}

func (s *eventProcessorStub) ProcessStoredEvent(
	ctx context.Context,
	event datastore.Event,
) (ProcessResult, error) {
	return s.process(ctx, event)
}

func TestColdStartQueryMatchesOnlyUnresolvedProcessableEvents(t *testing.T) {
	cutoff := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	builder := coldStartQuery(cutoff)

	mongoFilter := builder.ToMongo()
	assert.Equal(t, map[string]any{
		"createdAt": map[string]any{"$gt": cutoff},
		"$and": []any{
			map[string]any{"$or": []any{
				map[string]any{"healtheventstatus.nodequarantined": nil},
				map[string]any{"healtheventstatus.nodequarantined": ""},
				map[string]any{"healtheventstatus.nodequarantined": "NotStarted"},
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
	}, mongoFilter)

	sqlFilter, args := builder.ToSQL()
	assert.Contains(t, sqlFilter, "created_at > $1")
	assert.Contains(t, sqlFilter, "nodequarantined")
	assert.Contains(t, sqlFilter, "processingstrategy")
	assert.Contains(t, sqlFilter, "processingStrategy")
	assert.Contains(t, sqlFilter, "IS NULL")
	assert.Equal(t, []any{cutoff, "", "NotStarted", "1", "1"}, args)
}

func TestHandleProcessesEveryBatchInOrder(t *testing.T) {
	const eventCount = batchSize + 7

	events := make([]datastore.HealthEventWithStatus, eventCount)
	for i := range events {
		events[i].RawEvent = datastore.Event{"id": fmt.Sprintf("event-%04d", i)}
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
		process: func(_ context.Context, event datastore.Event) (ProcessResult, error) {
			processedIDs = append(processedIDs, event["id"].(string))

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

func TestHandleStopsOnProcessingFailure(t *testing.T) {
	processErr := errors.New("status update failed")
	events := []datastore.HealthEventWithStatus{
		{RawEvent: datastore.Event{"id": "first"}},
		{RawEvent: datastore.Event{"id": "second"}},
		{RawEvent: datastore.Event{"id": "third"}},
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
		process: func(_ context.Context, event datastore.Event) (ProcessResult, error) {
			id := event["id"].(string)
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
	assert.Equal(t, []string{"first", "second"}, processedIDs)
}

func TestHandleContinuesPastInvalidStoredEvent(t *testing.T) {
	events := []datastore.HealthEventWithStatus{
		{RawEvent: datastore.Event{"id": "invalid"}},
		{RawEvent: datastore.Event{"id": "valid"}},
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
		process: func(_ context.Context, event datastore.Event) (ProcessResult, error) {
			if event["id"] == "invalid" {
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
}

func TestHandleValidatesDependencies(t *testing.T) {
	processor := &eventProcessorStub{
		process: func(context.Context, datastore.Event) (ProcessResult, error) {
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
