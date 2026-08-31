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
	"fmt"
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
	completionID    string
	completionPath  string
	completionValue any
	completionCalls int
	completionErr   error
	completionErrs  []error
	actions         *[]string
	completed       map[string]bool
	countCalls      int
	countFilter     any
	countResult     int64
	countErr        error
	countErrs       []error
}

func (s *databaseClientStub) UpdateDocumentStatus(
	_ context.Context,
	documentID string,
	statusPath string,
	status any,
) error {
	s.completionID = documentID
	s.completionPath = statusPath
	s.completionValue = status
	s.completionCalls++
	if s.actions != nil {
		*s.actions = append(*s.actions, "complete")
	}
	if index := s.completionCalls - 1; index < len(s.completionErrs) {
		if err := s.completionErrs[index]; err != nil {
			return err
		}
	}

	if s.completionErr != nil {
		return s.completionErr
	}

	if statusPath == coldstart.RecoveryCompletionStatusPath && status == coldstart.RecoveryCompletionValue {
		if s.completed == nil {
			s.completed = make(map[string]bool)
		}

		s.completed[documentID] = true
	}

	return nil
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

func (s *databaseClientStub) CountDocuments(
	_ context.Context,
	filter any,
	_ *client.CountOptions,
) (int64, error) {
	s.countCalls++
	s.countFilter = filter
	if index := s.countCalls - 1; index < len(s.countErrs) {
		if err := s.countErrs[index]; err != nil {
			return 0, err
		}
	}

	return s.countResult, s.countErr
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

type completionFilteringHealthStoreStub struct {
	datastore.HealthEventStore
	db      *databaseClientStub
	record  datastore.HealthEventWithStatus
	query   datastore.QueryBuilder
	scanned int
}

func (s *completionFilteringHealthStoreStub) FindHealthEventsByQueryBatched(
	_ context.Context,
	builder datastore.QueryBuilder,
	_ int,
	fn func([]datastore.HealthEventWithStatus) error,
) error {
	s.query = builder
	s.scanned++
	if s.db.completed["event-uuid"] {
		return nil
	}

	return fn([]datastore.HealthEventWithStatus{s.record})
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
	markCalls   int
	markTokens  [][]byte
	actions     *[]string
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

func (s *changeStreamWatcherStub) MarkProcessed(_ context.Context, token []byte) error {
	s.markCalls++
	s.markTokens = append(s.markTokens, token)
	if s.actions != nil {
		*s.actions = append(*s.actions, "checkpoint")
	}

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

func TestProcessStoredEvent_LivePath_DeduplicatesReplay(t *testing.T) {
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

func TestProcessStoredEvent_SkippedEvent_DeduplicatesAfterCompletion(t *testing.T) {
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
	require.NoError(t, watcher.CompleteStoredEvents(context.Background(), []coldstart.StoredEventCompletion{{
		DocumentID: coldstart.StoredDocumentID{String: "event-uuid", Native: "event-uuid"},
		Result:     result,
	}}))

	err = watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "43",
		recordUUID: "event-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, callbackCalls,
		"a terminal recovery skip must not be re-decided by its buffered live copy")
}

func TestProcessEvent_IntentionallySkippedMarksRecoveryComplete(t *testing.T) {
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})

	err := watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "44",
		recordUUID: "event-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, "event-uuid", dbClient.completionID)
	assert.Equal(t, coldstart.RecoveryCompletionStatusPath, dbClient.completionPath)
	assert.Equal(t, coldstart.RecoveryCompletionValue, dbClient.completionValue)
	assert.Equal(t, 1, dbClient.completionCalls)
}

func TestProcessEvent_SkippedCompletionFailureIsReplayable(t *testing.T) {
	completionErr := errors.New("database unavailable")
	dbClient := &databaseClientStub{completionErr: completionErr}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})

	err := watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "45",
		recordUUID: "event-uuid",
	})
	require.ErrorIs(t, err, completionErr)
	assert.Equal(t, liveRecoveryStoreRetryAttempts, dbClient.completionCalls)
	assert.False(t, dbClient.completed["event-uuid"])
}

func TestProcessEvent_SkippedCompletionRetriesTransientFailure(t *testing.T) {
	transientErr := errors.New("database temporarily unavailable")
	dbClient := &databaseClientStub{completionErrs: []error{transientErr, transientErr, nil}}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document: storedHealthEvent("event-uuid"), eventID: "45", recordUUID: "event-uuid",
	}))
	assert.Equal(t, 3, dbClient.completionCalls)
	assert.True(t, dbClient.completed["event-uuid"])
}

func TestProcessEvent_CanceledSkipIsNotMarkedComplete(t *testing.T) {
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := watcher.processEvent(ctx, &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "46",
		recordUUID: "event-uuid",
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, dbClient.completionCalls)
}

func TestWatchEvents_SkippedCompletionPrecedesCheckpoint(t *testing.T) {
	var actions []string
	events := make(chan client.Event, 1)
	events <- &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "47",
		recordUUID: "event-uuid",
		token:      []byte("resume-token"),
	}
	close(events)

	changeStream := &changeStreamWatcherStub{events: events, actions: &actions}
	dbClient := &databaseClientStub{actions: &actions}
	watcher := NewEventWatcher(changeStream, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})

	require.NoError(t, watcher.watchEvents(context.Background()))
	assert.Equal(t, []string{"complete", "checkpoint"}, actions)
	assert.Equal(t, 1, changeStream.markCalls)
	assert.Equal(t, [][]byte{[]byte("resume-token")}, changeStream.markTokens)
}

func TestWatchEvents_SkippedCompletionFailureDoesNotCheckpoint(t *testing.T) {
	completionErr := errors.New("database unavailable")
	events := make(chan client.Event, 1)
	events <- &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "49",
		recordUUID: "event-uuid",
		token:      []byte("resume-token"),
	}
	close(events)

	changeStream := &changeStreamWatcherStub{events: events}
	dbClient := &databaseClientStub{completionErr: completionErr}
	watcher := NewEventWatcher(changeStream, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		return nil, nil
	})

	err := watcher.watchEvents(context.Background())
	require.ErrorIs(t, err, completionErr)
	assert.Zero(t, changeStream.markCalls, "a live skip without its durable marker must remain replayable")
}

func TestWatchEvents_RecoveryOverlapLookupFailureDoesNotCheckpoint(t *testing.T) {
	lookupErr := errors.New("database unavailable")
	document := storedHealthEvent("event-uuid")
	document["createdAt"] = time.Now().Add(-time.Minute)
	events := make(chan client.Event, 1)
	events <- &clientEventStub{
		document:   document,
		eventID:    "50",
		recordUUID: "event-uuid",
		token:      []byte("resume-token"),
	}
	close(events)

	changeStream := &changeStreamWatcherStub{events: events}
	dbClient := &databaseClientStub{countErr: lookupErr}
	watcher := NewEventWatcher(changeStream, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.recoveryOverlapCutoff = time.Now()
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		t.Fatal("event must not be processed when its durable overlap state is unknown")

		return nil, nil
	})

	err := watcher.watchEvents(context.Background())
	require.ErrorIs(t, err, lookupErr)
	assert.Equal(t, liveRecoveryStoreRetryAttempts, dbClient.countCalls)
	assert.Zero(t, changeStream.markCalls, "an overlap event with unknown durable state must remain replayable")
}

func TestRecoveryOverlapLookup_RetriesTransientFailure(t *testing.T) {
	transientErr := errors.New("database temporarily unavailable")
	now := time.Now().UTC()
	dbClient := &databaseClientStub{
		countResult: 1,
		countErrs:   []error{transientErr, transientErr, nil},
	}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.recoveryOverlapCutoff = now

	terminal, err := watcher.recoveryOverlapEventAlreadyTerminal(
		context.Background(), now.Add(-time.Minute), "event-uuid")
	require.NoError(t, err)
	assert.True(t, terminal)
	assert.Equal(t, 3, dbClient.countCalls)
}

func TestProcessEvent_UnresolvedRecoveryOverlapStillProcesses(t *testing.T) {
	now := time.Now().UTC()
	document := storedHealthEvent("event-uuid")
	document["createdAt"] = now.Add(-time.Minute)
	dbClient := &databaseClientStub{countResult: 0}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.recoveryOverlapCutoff = now

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++
		status := model.Quarantined

		return &status, nil
	})

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document: document, eventID: "52", recordUUID: "event-uuid",
	}))
	assert.Equal(t, 1, dbClient.countCalls)
	assert.Equal(t, 1, callbackCalls, "an unresolved overlap event must not be mistaken for terminal")
	assert.Equal(t, 1, dbClient.updateCalls)
}

func TestProcessEvent_PostCutoffEventBypassesRecoveryLookup(t *testing.T) {
	now := time.Now().UTC()
	document := storedHealthEvent("event-uuid")
	document["createdAt"] = now.Add(time.Minute)
	dbClient := &databaseClientStub{countErr: errors.New("lookup must not run")}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.recoveryOverlapCutoff = now

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++
		status := model.Quarantined

		return &status, nil
	})

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document: document, eventID: "53", recordUUID: "event-uuid",
	}))
	assert.Zero(t, dbClient.countCalls, "events created after recovery opened cannot overlap the cold-start scan")
	assert.Equal(t, 1, callbackCalls)
}

func TestRecoveryOverlapLookup_PredicateCoversEveryTerminalState(t *testing.T) {
	now := time.Now().UTC()
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.recoveryOverlapCutoff = now

	terminal, err := watcher.recoveryOverlapEventAlreadyTerminal(
		context.Background(), now.Add(-time.Minute), "event-uuid")
	require.NoError(t, err)
	assert.False(t, terminal)

	filter, ok := dbClient.countFilter.(datastore.QueryBuilder)
	require.True(t, ok)
	sql, args := filter.ToSQL()
	assert.Contains(t, sql, "id = $1")
	assert.Contains(t, sql, "faultquarantinerecovery")
	assert.Contains(t, sql, "nodequarantined")
	assert.Contains(t, sql, "IS NOT NULL")
	assert.Equal(t, []any{
		"event-uuid", coldstart.RecoveryCompletionValue, "", string(model.StatusNotStarted),
	}, args)

	mongoFilter := filter.ToMongo()
	assert.Contains(t, fmt.Sprint(mongoFilter), coldstart.RecoveryCompletionStatusPath)
	assert.Contains(t, fmt.Sprint(mongoFilter), "healtheventstatus.nodequarantined")
	assert.Contains(t, fmt.Sprint(mongoFilter), string(model.StatusNotStarted))
}

func TestLiveSkippedEventIsExcludedFromLaterColdStart(t *testing.T) {
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	liveCallbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		liveCallbackCalls++

		return nil, nil
	})

	require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "48",
		recordUUID: "event-uuid",
	}))
	require.True(t, dbClient.completed["event-uuid"])
	assert.Equal(t, 1, liveCallbackCalls)

	// Simulate a rule change: this same event would now quarantine the node if a
	// fresh cold start were allowed to replay it.
	coldStartCallbackCalls := 0
	cordoned := false
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		coldStartCallbackCalls++
		cordoned = true
		status := model.Quarantined

		return &status, nil
	})

	now := time.Now().UTC()
	store := &completionFilteringHealthStoreStub{
		db: dbClient,
		record: datastore.HealthEventWithStatus{
			RawEvent:  storedHealthEvent("event-uuid"),
			CreatedAt: now,
		},
	}
	require.NoError(t, coldstart.Handle(context.Background(), coldstart.Dependencies{
		HealthEventStore:   store,
		EventProcessor:     watcher,
		ColdStartAfterTime: now.Add(-time.Minute),
		ColdStartUntilTime: now.Add(time.Minute),
	}))
	assert.Equal(t, 2, store.scanned, "cold start scans bounded fault and healthy phases")
	require.NotNil(t, store.query)
	assert.Contains(t, fmt.Sprint(store.query.ToMongo()), coldstart.RecoveryCompletionStatusPath)
	assert.Zero(t, coldStartCallbackCalls, "the terminal live skip must not be replayed under changed rules")
	assert.False(t, cordoned, "an event skipped live must not cordon the node after a rule change")

	// A failed later recovery can restart the process before the original stream
	// token advances. A fresh watcher must reconstruct this decision from the
	// durable marker rather than its empty in-memory dedup map.
	dbClient.countResult = 1
	restartedWatcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	restartedWatcher.recoveryOverlapCutoff = now.Add(time.Minute)
	restartedWatcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		cordoned = true
		status := model.Quarantined

		return &status, nil
	})
	overlapDocument := storedHealthEvent("event-uuid")
	overlapDocument["createdAt"] = now
	require.NoError(t, restartedWatcher.processEvent(context.Background(), &clientEventStub{
		document:   overlapDocument,
		eventID:    "48",
		recordUUID: "event-uuid",
	}))
	assert.False(t, cordoned, "a restart must not reapply a durably completed overlap event")
	assert.Equal(t, 1, dbClient.countCalls)
	overlapFilter, ok := dbClient.countFilter.(datastore.QueryBuilder)
	require.True(t, ok)
	assert.Contains(t, fmt.Sprint(overlapFilter.ToMongo()), coldstart.RecoveryCompletionStatusPath)
}

func TestProcessStoredEvent_StatusUpdateFailure_ReturnsError(t *testing.T) {
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

func TestProcessStoredEvent_ReconcilerFailure_ReturnsError(t *testing.T) {
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

func TestProcessStoredEvent_PermanentEvaluationFailure_ClassifiesPermanent(t *testing.T) {
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

func TestProcessStoredEvent_SuccessfulStatusWithPermanentEvaluationFailure_KeepsSuccess(t *testing.T) {
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

func TestProcessStoredEvent_MixedPermanentAndTransientFailures_Replays(t *testing.T) {
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

func TestProcessStoredEvent_TransientFailureDoesNotPersistPartialStatus(t *testing.T) {
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
	assert.Zero(t, dbClient.updateCalls,
		"a partial status would exclude the event from the next cold-start scan")
}

func TestProcessEvent_LiveTransientFailureStillPersistsStatus(t *testing.T) {
	processingErr := errors.New("node API unavailable")
	dbClient := &databaseClientStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		status := model.Quarantined

		return &status, processingErr
	})

	err := watcher.processEvent(context.Background(), &clientEventStub{
		document:   storedHealthEvent("event-uuid"),
		eventID:    "51",
		recordUUID: "event-uuid",
	})
	require.ErrorIs(t, err, processingErr)
	assert.Equal(t, 1, dbClient.updateCalls,
		"withholding partial status is recovery-only and must not change the established live path")
}

func TestCompleteStoredEvents_MixedTerminalResults_PersistsAndDeduplicatesAll(t *testing.T) {
	dbClient := &databaseClientStub{}
	objectIDs := &objectIDStoreStub{}
	watcher := NewEventWatcher(nil, dbClient, time.Minute, objectIDs)

	callbackCalls := 0
	watcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++

		return nil, nil
	})

	require.NoError(t, watcher.CompleteStoredEvents(
		context.Background(), []coldstart.StoredEventCompletion{
			{
				DocumentID: coldstart.StoredDocumentID{String: "event-invalid", Native: "native-invalid"},
				Result:     coldstart.ProcessResultInvalid,
			},
			{
				DocumentID: coldstart.StoredDocumentID{String: "event-invalid", Native: "native-invalid"},
				Result:     coldstart.ProcessResultInvalid,
			},
			{
				DocumentID: coldstart.StoredDocumentID{String: "event-skipped", Native: "native-skipped"},
				Result:     coldstart.ProcessResultSkipped,
			},
			{
				DocumentID: coldstart.StoredDocumentID{String: "event-superseded", Native: "native-superseded"},
				Result:     coldstart.ProcessResultSuperseded,
			},
		}))
	assert.Equal(t, 1, dbClient.updateManyCalls)
	filter, ok := dbClient.batchFilter.(datastore.QueryBuilder)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"_id": map[string]any{"$in": []any{
		"native-invalid", "native-skipped", "native-superseded",
	}}},
		filter.ToMongo())
	update, ok := dbClient.batchUpdate.(*query.UpdateBuilder)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"$set": map[string]any{
		coldstart.RecoveryCompletionStatusPath: coldstart.RecoveryCompletionValue,
	}}, update.ToMongo())
	assert.NotEmpty(t, update.ToMongoPipeline(), "bulk updates must tolerate a null status parent")

	for _, eventID := range []string{"event-invalid", "event-skipped", "event-superseded"} {
		require.NoError(t, watcher.processEvent(context.Background(), &clientEventStub{
			document:   storedHealthEvent(eventID),
			eventID:    eventID + "-token",
			recordUUID: eventID,
		}))
	}
	assert.Zero(t, callbackCalls, "no terminal recovery decision should be replayed live")
	assert.Equal(t, "event-superseded-token", objectIDs.last)
}

func TestCompleteStoredEvents_SkippedDecisionSuppressesRestartOverlap(t *testing.T) {
	dbClient := &databaseClientStub{}
	recoveringWatcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	require.NoError(t, recoveringWatcher.CompleteStoredEvents(
		context.Background(), []coldstart.StoredEventCompletion{{
			DocumentID: coldstart.StoredDocumentID{String: "event-skipped", Native: "event-skipped"},
			Result:     coldstart.ProcessResultSkipped,
		}}))
	assert.Equal(t, 1, dbClient.updateManyCalls)

	dbClient.countResult = 1
	now := time.Now().UTC()
	restartedWatcher := NewEventWatcher(nil, dbClient, time.Minute, &objectIDStoreStub{})
	restartedWatcher.recoveryOverlapCutoff = now
	callbackCalls := 0
	restartedWatcher.SetProcessEventCallback(func(context.Context, *model.HealthEventWithStatus) (*model.Status, error) {
		callbackCalls++

		return nil, nil
	})
	document := storedHealthEvent("event-skipped")
	document["createdAt"] = now.Add(-time.Minute)
	require.NoError(t, restartedWatcher.processEvent(context.Background(), &clientEventStub{
		document: document, eventID: "event-skipped-token", recordUUID: "event-skipped",
	}))
	assert.Zero(t, callbackCalls,
		"a completed recovery skip must not be re-decided after a restart")
	assert.Equal(t, 1, dbClient.countCalls)
}

func TestExpireRecoveredEventIDs_ExpiredEntry_DoesNotSuppressLiveEvent(t *testing.T) {
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

func TestRememberRecoveredEvent_ColdStartEntry_UsesArmedDeadlineShape(t *testing.T) {
	watcher := NewEventWatcher(nil, &databaseClientStub{}, time.Minute, &objectIDStoreStub{})
	watcher.rememberRecoveredEvent("event-id")

	value, loaded := watcher.recoveredEventIDs.Load("event-id")
	require.True(t, loaded)
	unarmed, ok := value.(time.Time)
	require.True(t, ok)
	assert.True(t, unarmed.IsZero())

	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	watcher.armRecoveredEventExpiry(now)

	value, loaded = watcher.recoveredEventIDs.Load("event-id")
	require.True(t, loaded)
	armed, ok := value.(time.Time)
	require.True(t, ok)
	assert.Equal(t, now.Add(recoveredEventDedupRetention), armed)
}

func TestStart_ColdStartFailure_OpensWatcherThenCloses(t *testing.T) {
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

func TestStart_ColdStartInProgress_ReportsBacklog(t *testing.T) {
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

func TestStart_ColdStartCancellation_TreatsAsShutdown(t *testing.T) {
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

func TestStart_ColdStartCancellation_DoesNotEnterWatchLoop(t *testing.T) {
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
