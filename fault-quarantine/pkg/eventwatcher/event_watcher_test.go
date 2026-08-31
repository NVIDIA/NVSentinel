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

package eventwatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

type databaseClientStub struct {
	client.DatabaseClient
	updatedID       string
	updatedFields   map[string]any
	updateCalls     int
	updateErr       error
	updateManyCalls int
	batchFilter     any
	batchUpdate     any
}

func (s *databaseClientStub) UpdateManyDocuments(
	_ context.Context,
	filter any,
	update any,
) (*client.UpdateResult, error) {
	s.batchFilter = filter
	s.batchUpdate = update
	s.updateManyCalls++

	return &client.UpdateResult{}, s.updateErr
}

func (s *databaseClientStub) UpdateDocumentStatusFields(
	_ context.Context,
	documentID string,
	fields map[string]any,
) error {
	s.updatedID = documentID
	s.updatedFields = fields
	s.updateCalls++

	return s.updateErr
}

type objectIDStoreStub struct {
	last string
}

func (s *objectIDStoreStub) StoreLastProcessedObjectID(id string) {
	s.last = id
}

func (s *objectIDStoreStub) LoadLastProcessedObjectID() (string, bool) {
	return s.last, s.last != ""
}

type clientEventStub struct {
	document   datastore.Event
	eventID    string
	recordUUID string
	token      []byte
}

func (s *clientEventStub) GetDocumentID() (string, error) { return s.eventID, nil }
func (s *clientEventStub) GetRecordUUID() (string, error) { return s.recordUUID, nil }
func (s *clientEventStub) GetNodeName() (string, error)   { return "node-a", nil }
func (s *clientEventStub) GetResumeToken() []byte         { return s.token }

func (s *clientEventStub) UnmarshalDocument(value any) error {
	encoded, err := json.Marshal(s.document)
	if err != nil {
		return err
	}

	return json.Unmarshal(encoded, value)
}

type changeStreamWatcherStub struct {
	started     bool
	closed      bool
	events      chan client.Event
	closeFn     func()
	metricCalls chan struct{}
}

func (s *changeStreamWatcherStub) GetUnprocessedEventCount(context.Context, string) (int64, error) {
	if s.metricCalls != nil {
		select {
		case s.metricCalls <- struct{}{}:
		default:
		}
	}

	return 7, nil
}

func (s *changeStreamWatcherStub) Start(context.Context) {
	s.started = true
}

func (s *changeStreamWatcherStub) Events() <-chan client.Event {
	return s.events
}

func (s *changeStreamWatcherStub) MarkProcessed(context.Context, []byte) error {
	return nil
}

func (s *changeStreamWatcherStub) Close(context.Context) error {
	s.closed = true
	if s.closeFn != nil {
		s.closeFn()
	}

	return nil
}

func storedHealthEvent(id string) datastore.Event {
	return datastore.Event{
		"id": id,
		"healthevent": datastore.Event{
			"nodeName":       "node-a",
			"agent":          "gpu-health-monitor",
			"componentClass": "GPU",
			"checkName":      "GpuNvlinkWatch",
			"isHealthy":      false,
			"isFatal":        true,
		},
		"healtheventstatus": datastore.Event{
			"spanIds": map[string]string{},
		},
	}
}

func parsedStoredHealthEvent(t *testing.T, id string) model.HealthEventWithStatus {
	t.Helper()

	parsed, err := eventutil.ParseHealthEventFromEvent(storedHealthEvent(id))
	require.NoError(t, err)

	return parsed
}

func TestProcessStoredEventUsesLiveProcessingPathAndDeduplicatesReplay(t *testing.T) {
	dbClient := &databaseClientStub{}
	objectIDs := &objectIDStoreStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, objectIDs)

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(_ context.Context, event *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++
		assert.Equal(t, "event-uuid", event.HealthEvent.Id)

		status := model.Quarantined

		return &status, nil
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.NoError(t, err)
	assert.Equal(t, coldstart.ProcessResultProcessed, result)
	assert.Equal(t, "event-uuid", dbClient.updatedID)
	assert.Equal(t, string(model.Quarantined),
		dbClient.updatedFields["healtheventstatus.nodequarantined"])
	assert.Equal(t, 1, dbClient.updateCalls)

	err = watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "42",
		recordUUID: "event-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, "42", objectIDs.last)
	assert.Equal(t, 1, callbackCalls, "the buffered live copy must not apply quarantine twice")
	assert.Equal(t, 1, dbClient.updateCalls)
}

func TestProcessStoredEventDoesNotDeduplicateSkippedEvent(t *testing.T) {
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++

		return nil, nil
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.NoError(t, err)
	assert.Equal(t, coldstart.ProcessResultSkipped, result)

	err = watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "43",
		recordUUID: "event-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, callbackCalls)
}

func TestProcessStoredEventReturnsStatusUpdateFailure(t *testing.T) {
	updateErr := errors.New("database unavailable")
	dbClient := &databaseClientStub{updateErr: updateErr}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		status := model.Quarantined

		return &status, nil
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.ErrorIs(t, err, updateErr)
	assert.Equal(t, coldstart.ProcessResultFailed, result)
}

func TestProcessStoredEventReturnsReconcilerFailure(t *testing.T) {
	processingErr := errors.New("node API unavailable")
	watcher := NewEventWatcher(nil, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, processingErr
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.ErrorIs(t, err, processingErr)
	assert.Equal(t, coldstart.ProcessResultFailed, result)
}

func TestProcessStoredEventClassifiesPermanentEvaluationFailure(t *testing.T) {
	processingErr := coldstart.PermanentError(errors.New("missing CEL field"))
	watcher := NewEventWatcher(nil, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, processingErr
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.NoError(t, err)
	assert.Equal(t, coldstart.ProcessResultInvalid, result)
}

func TestProcessStoredEventKeepsSuccessfulStatusWithPermanentEvaluationFailure(t *testing.T) {
	processingErr := coldstart.PermanentError(errors.New("missing CEL field"))
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		status := model.Quarantined

		return &status, processingErr
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.NoError(t, err)
	assert.Equal(t, coldstart.ProcessResultProcessed, result)
	assert.Equal(t, 1, dbClient.updateCalls)
}

func TestProcessStoredEventReplaysMixedPermanentAndTransientFailures(t *testing.T) {
	permanentErr := coldstart.PermanentError(errors.New("missing CEL field"))
	transientErr := errors.New("node API unavailable")
	watcher := NewEventWatcher(nil, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, errors.Join(permanentErr, transientErr)
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.ErrorIs(t, err, transientErr)
	assert.Equal(t, coldstart.ProcessResultFailed, result)
}

func TestProcessStoredEventReplaysTransientFailureAfterStatusUpdate(t *testing.T) {
	processingErr := errors.New("node API unavailable")
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		status := model.Quarantined

		return &status, processingErr
	})

	result, err := watcher.ProcessStoredEvent(
		context.Background(), parsedStoredHealthEvent(t, "event-uuid"), "event-uuid")
	require.ErrorIs(t, err, processingErr)
	assert.Equal(t, coldstart.ProcessResultFailed, result)
	assert.Equal(t, 1, dbClient.updateCalls)
}

func TestCompleteStoredEventsUsesOneBulkUpdateAndDeduplicatesItsUpdates(t *testing.T) {
	dbClient := &databaseClientStub{}
	objectIDs := &objectIDStoreStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, objectIDs)

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++

		return nil, nil
	})

	require.NoError(t, watcher.CompleteStoredEvents(
		context.Background(), []coldstart.StoredDocumentID{
			{String: "event-uuid", Native: "native-one"},
			{String: "event-uuid", Native: "native-one"},
			{String: "event-two", Native: "native-two"},
		}))
	assert.Equal(t, 1, dbClient.updateManyCalls)
	filter, ok := dbClient.batchFilter.(datastore.QueryBuilder)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"_id": map[string]any{"$in": []any{"native-one", "native-two"}}},
		filter.ToMongo())
	update, ok := dbClient.batchUpdate.(*query.UpdateBuilder)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"$set": map[string]any{
		coldstart.RecoveryCompletionStatusPath: coldstart.RecoveryCompletionValue,
	}}, update.ToMongo())
	assert.NotEmpty(t, update.ToMongoPipeline(), "bulk updates must tolerate a null status parent")

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "44",
		recordUUID: "event-uuid",
	}))
	assert.Equal(t, 0, callbackCalls)
	assert.Equal(t, "44", objectIDs.last)
}

func TestExpiredRecoveryDedupEntryDoesNotSuppressLiveEvent(t *testing.T) {
	watcher := NewEventWatcher(nil, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++

		return nil, nil
	})
	watcher.recoveredEventIDs.Store("event-uuid", time.Now().Add(-time.Minute))

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "45",
		recordUUID: "event-uuid",
	}))
	assert.Equal(t, 1, callbackCalls)
}

func TestStartOpensWatcherBeforeColdStartAndClosesOnFailure(t *testing.T) {
	recoveryErr := errors.New("recovery failed")
	changeStream := &changeStreamWatcherStub{events: make(chan client.Event)}
	watcher := NewEventWatcher(changeStream, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.SetColdStartCallback(func(context.Context) error {
		assert.True(t, changeStream.started)

		return recoveryErr
	})

	err := watcher.Start(context.Background())
	require.ErrorIs(t, err, recoveryErr)
	assert.True(t, changeStream.closed)
}

func TestStartReportsBacklogWhileColdStartIsRunning(t *testing.T) {
	recoveryErr := errors.New("recovery stopped")
	metricCalls := make(chan struct{}, 1)
	changeStream := &changeStreamWatcherStub{
		events:      make(chan client.Event),
		metricCalls: metricCalls,
	}
	objectIDs := &objectIDStoreStub{last: "41"}
	watcher := NewEventWatcher(changeStream, &databaseClientStub{}, time.Millisecond, objectIDs)
	watcher.SetColdStartCallback(func(context.Context) error {
		select {
		case <-metricCalls:
			return recoveryErr
		case <-time.After(time.Second):
			return errors.New("backlog metric did not run during cold start")
		}
	})

	err := watcher.Start(context.Background())
	require.ErrorIs(t, err, recoveryErr)
}

func TestStartTreatsColdStartCancellationAsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	changeStream := &changeStreamWatcherStub{events: make(chan client.Event)}
	watcher := NewEventWatcher(changeStream, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.SetColdStartCallback(func(context.Context) error {
		cancel()

		return context.Canceled
	})

	require.NoError(t, watcher.Start(ctx))
	assert.True(t, changeStream.closed)
}

func TestStartDoesNotEnterWatchLoopAfterColdStartCancellation(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan client.Event)
		changeStream := &changeStreamWatcherStub{
			events: events,
			closeFn: func() {
				close(events)
			},
		}
		watcher := NewEventWatcher(changeStream, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
		watcher.SetColdStartCallback(func(context.Context) error {
			cancel()

			return context.Canceled
		})

		require.NoError(t, watcher.Start(ctx))
		assert.True(t, changeStream.closed)
	}
}
