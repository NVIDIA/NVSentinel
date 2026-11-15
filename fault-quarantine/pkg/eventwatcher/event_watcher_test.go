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
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
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

// sessionStore is an in-memory store for the cancellation tests. It reads the filters that the
// query builder renders for MongoDB and lists events like the health event store of both
// databases, in the order of the store. Its FindOne answers like the MongoDB client: it never
// fails, and a failed query or a missing document shows when the result is decoded. Like a
// PostgreSQL document, the decoded document holds no ID and an earlier createdAt than the one the
// store orders by. findErrs, listErrs and updateErrs fail those calls in order. The first
// partialUpdates failed updates write the first event they match before they fail, as a
// multi-document update can. beforeList runs before each list with its number, so a test can
// write events while the cancellation runs.
type sessionStore struct {
	client.DatabaseClient
	datastore.HealthEventStore
	events         []*storedEvent
	findErrs       []error
	findCalls      int
	listErrs       []error
	listCalls      int
	updateErrs     []error
	updateCalls    int
	partialUpdates int
	beforeList     func(s *sessionStore, call int)
}

type storedEvent struct {
	id        string
	node      string
	status    model.Status
	createdAt time.Time
}

var sessionTestStart = time.Date(2026, 11, 14, 22, 0, 0, 0, time.UTC)

// addEvent stores an event of node-a created the given number of minutes after the test start.
func (s *sessionStore) addEvent(id string, status model.Status, minute int) {
	s.addEventAt("node-a", id, status, time.Duration(minute)*time.Minute)
}

// addEventAt stores an event of a node created the given time after the test start.
func (s *sessionStore) addEventAt(node, id string, status model.Status, after time.Duration) {
	s.events = append(s.events, &storedEvent{
		id: id, node: node, status: status, createdAt: sessionTestStart.Add(after),
	})
}

func (s *sessionStore) statuses() map[string]model.Status {
	statuses := make(map[string]model.Status, len(s.events))
	for _, event := range s.events {
		statuses[event.id] = event.status
	}

	return statuses
}

func nextStoreErr(errs []error, calls int) error {
	if index := calls - 1; index < len(errs) {
		return errs[index]
	}

	return nil
}

func (s *sessionStore) matching(filter any) ([]*storedEvent, error) {
	builder, ok := filter.(*query.Builder)
	if !ok {
		return nil, fmt.Errorf("unexpected filter %T", filter)
	}

	rendered := builder.ToMongo()

	var matched []*storedEvent

	for _, event := range s.events {
		if eventMatches(event, rendered) {
			matched = append(matched, event)
		}
	}

	return matched, nil
}

func eventMatches(event *storedEvent, filter map[string]any) bool {
	for field, condition := range filter {
		if field == "$and" {
			for _, part := range condition.([]any) {
				if !eventMatches(event, part.(map[string]any)) {
					return false
				}
			}

			continue
		}

		if !valueMatches(storedField(event, field), condition) {
			return false
		}
	}

	return true
}

func storedField(event *storedEvent, field string) any {
	switch field {
	case "_id":
		return event.id
	case "healthevent.nodename":
		return event.node
	case "healtheventstatus.nodequarantined":
		return string(event.status)
	case "createdAt":
		return event.createdAt
	default:
		panic("unexpected field " + field)
	}
}

// hexID is a MongoDB ObjectID, which the query builder renders for an ID in hex.
type hexID interface{ Hex() string }

// storedValue reads an ObjectID of a filter as the hex ID the store holds.
func storedValue(operand any) any {
	if id, ok := operand.(hexID); ok {
		return id.Hex()
	}

	return operand
}

func valueMatches(value any, condition any) bool {
	operators, ok := condition.(map[string]any)
	if !ok {
		return value == storedValue(condition)
	}

	for operator, operand := range operators {
		var matches bool

		switch operator {
		case "$in":
			matches = slices.ContainsFunc(operand.([]any), func(item any) bool { return storedValue(item) == value })
		case "$gt":
			matches = value.(time.Time).After(operand.(time.Time))
		case "$gte":
			matches = !value.(time.Time).Before(operand.(time.Time))
		case "$lte":
			matches = !value.(time.Time).After(operand.(time.Time))
		default:
			panic("unexpected operator " + operator)
		}

		if !matches {
			return false
		}
	}

	return true
}

// encodeStoredEvent renders the document of an event. Like a PostgreSQL document, it holds no ID
// and an earlier createdAt than the one the store orders by.
func encodeStoredEvent(event *storedEvent, value any) error {
	encoded, err := json.Marshal(map[string]any{
		"createdAt":         event.createdAt.Add(-5 * time.Millisecond),
		"healthEvent":       map[string]any{"nodeName": event.node},
		"healthEventStatus": map[string]any{"nodeQuarantined": string(event.status)},
	})
	if err != nil {
		return err
	}

	return json.Unmarshal(encoded, value)
}

func (s *sessionStore) FindOne(_ context.Context, filter any, options *client.FindOneOptions) (client.SingleResult, error) {
	matched, err := s.matching(filter)
	if err != nil {
		return nil, err
	}

	newestFirst := options != nil && options.Sort != nil && options.Sort.(map[string]any)["createdAt"] == -1
	slices.SortFunc(matched, func(a, b *storedEvent) int {
		if newestFirst {
			return b.createdAt.Compare(a.createdAt)
		}

		return a.createdAt.Compare(b.createdAt)
	})

	result := &sessionResult{store: s}
	if len(matched) > 0 {
		result.event = matched[0]
	}

	return result, nil
}

// FindHealthEventsByQueryBatched lists like both stores: ordered by the store's creation time
// and then by ID, with that creation time on each record.
func (s *sessionStore) FindHealthEventsByQueryBatched(
	_ context.Context, builder datastore.QueryBuilder, batchSize int,
	fn func([]datastore.HealthEventWithStatus) error,
) error {
	s.listCalls++

	if err := nextStoreErr(s.listErrs, s.listCalls); err != nil {
		return err
	}

	if s.beforeList != nil {
		s.beforeList(s, s.listCalls)
	}

	matched, err := s.matching(builder)
	if err != nil {
		return err
	}

	slices.SortFunc(matched, func(a, b *storedEvent) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}

		return strings.Compare(a.id, b.id)
	})

	for chunk := range slices.Chunk(matched, batchSize) {
		records := make([]datastore.HealthEventWithStatus, 0, len(chunk))

		for _, event := range chunk {
			status := datastore.Status(event.status)
			records = append(records, datastore.HealthEventWithStatus{
				CreatedAt:         event.createdAt,
				HealthEventStatus: datastore.HealthEventStatus{NodeQuarantined: &status},
				RawEvent:          map[string]any{"id": event.id},
			})
		}

		if err := fn(records); err != nil {
			return err
		}
	}

	return nil
}

func (s *sessionStore) UpdateManyDocuments(_ context.Context, filter any, _ any) (*client.UpdateResult, error) {
	s.updateCalls++

	matched, err := s.matching(filter)
	if err != nil {
		return nil, err
	}

	if err := nextStoreErr(s.updateErrs, s.updateCalls); err != nil {
		if s.partialUpdates > 0 && len(matched) > 0 {
			s.partialUpdates--
			matched[0].status = model.Cancelled
		}

		return nil, err
	}

	for _, event := range matched {
		event.status = model.Cancelled
	}

	return &client.UpdateResult{MatchedCount: int64(len(matched)), ModifiedCount: int64(len(matched))}, nil
}

type sessionResult struct {
	store *sessionStore
	event *storedEvent
}

func (r *sessionResult) Decode(value any) error {
	r.store.findCalls++

	if err := nextStoreErr(r.store.findErrs, r.store.findCalls); err != nil {
		return err
	}

	if r.event == nil {
		return client.ErrNoDocuments
	}

	return encodeStoredEvent(r.event, value)
}

func (r *sessionResult) Err() error { return nil }

func newSessionWatcher(store *sessionStore) *EventWatcher {
	watcher := NewEventWatcher(nil, store, time.Minute, &objectIDStoreStub{})
	watcher.SetHealthEventStore(store)

	return watcher
}

func TestFindQuarantineSession_FindsTheSessionOfTheEvent(t *testing.T) {
	transient := errors.New("(NotWritablePrimary) not primary")

	tests := []struct {
		name            string
		eventID         string
		listErrs        []error
		wantStart       string
		wantQuarantined bool
	}{
		{name: "the first event of a session", eventID: "start", wantStart: "start", wantQuarantined: true},
		{name: "a later event of a session", eventID: "later", wantStart: "start", wantQuarantined: true},
		{
			name: "a later event that a cancellation already wrote", eventID: "written",
			wantStart: "start", wantQuarantined: true,
		},
		{
			name: "a lookup that fails once is retried", eventID: "later", listErrs: []error{transient},
			wantStart: "start", wantQuarantined: true,
		},
		{
			name: "an event of a session a healthy event ended", eventID: "older-later",
			wantStart: "older-start", wantQuarantined: true,
		},
		{name: "a cancelled session with nothing before it", eventID: "cancelled-start"},
		{name: "an event that is not stored", eventID: "missing"},
		{
			name: "the latest session of the node", eventID: common.QuarantineCancellationLatestSession,
			wantStart: "newer-start", wantQuarantined: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sessionStore{listErrs: tt.listErrs}
			store.addEvent("cancelled-start", model.Cancelled, 0)
			store.addEvent("older-start", model.Quarantined, 2)
			store.addEvent("older-later", model.AlreadyQuarantined, 3)
			store.addEvent("older-healthy", model.UnQuarantined, 4)
			store.addEvent("start", model.Quarantined, 10)
			store.addEvent("later", model.AlreadyQuarantined, 11)
			store.addEvent("written", model.Cancelled, 12)
			store.addEvent("newer-start", model.Quarantined, 20)
			watcher := newSessionWatcher(store)

			start, quarantined, err := watcher.FindQuarantineSession(context.Background(), "node-a", []string{tt.eventID})
			require.NoError(t, err)
			assert.Equal(t, tt.wantStart, start)
			assert.Equal(t, tt.wantQuarantined, quarantined)
		})
	}
}

func TestFindQuarantineSession_LatestSessionEndedByAHealthyEvent(t *testing.T) {
	store := &sessionStore{}
	store.addEvent("start", model.Quarantined, 0)
	store.addEvent("healthy", model.UnQuarantined, 5)
	watcher := newSessionWatcher(store)

	_, quarantined, err := watcher.FindQuarantineSession(
		context.Background(), "node-a", []string{common.QuarantineCancellationLatestSession})
	require.NoError(t, err)
	assert.False(t, quarantined)
}

// An event whose status write failed still has its place in the store, so it leads to its
// session like an AlreadyQuarantined event.
func TestFindQuarantineSession_EventWithoutStatus(t *testing.T) {
	store := &sessionStore{}
	store.addEvent("start", model.Quarantined, 10)
	store.addEvent("later", "", 11)
	watcher := newSessionWatcher(store)

	start, quarantined, err := watcher.FindQuarantineSession(context.Background(), "node-a", []string{"later"})
	require.NoError(t, err)
	assert.Equal(t, "start", start)
	assert.True(t, quarantined)
}

// The first event of the session is used as is when it is among the IDs. Events reach MongoDB
// through more than one platform-connectors pod, so a later event of a batch can sort before
// the first one, and only the stored status tells them apart.
func TestFindQuarantineSession_FirstEventAmongTheIDs(t *testing.T) {
	store := &sessionStore{}
	store.addEvent("older-start", model.Quarantined, 0)
	store.addEventAt("node-a", "older-healthy", model.UnQuarantined, 5*time.Minute)
	store.addEventAt("node-a", "later", model.AlreadyQuarantined, 10*time.Minute)
	store.addEventAt("node-a", "start", model.Quarantined, 10*time.Minute+3*time.Millisecond)
	watcher := newSessionWatcher(store)

	start, quarantined, err := watcher.FindQuarantineSession(
		context.Background(), "node-a", []string{"later", "start", "missing"})
	require.NoError(t, err)
	assert.Equal(t, "start", start)
	assert.True(t, quarantined)

	_, quarantined, err = watcher.FindQuarantineSession(context.Background(), "node-a", []string{"missing"})
	require.NoError(t, err)
	assert.False(t, quarantined, "IDs that are not stored have nothing to cancel")
}

// The events of a long session are cancelled in batches, below the PostgreSQL limit on bind
// parameters, and the first event still goes last.
func TestCancelQuarantineSession_ManyEvents(t *testing.T) {
	store := &sessionStore{}
	store.addEvent("start", model.Quarantined, 10)

	for i := range 2*cancelBatchSize + 1 {
		store.addEventAt("node-a", fmt.Sprintf("later-%05d", i), model.AlreadyQuarantined,
			11*time.Minute+time.Duration(i)*time.Millisecond)
	}

	watcher := newSessionWatcher(store)

	require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test"))
	assert.Equal(t, 4, store.updateCalls, "three batches of events, then the first event")

	for id, status := range store.statuses() {
		assert.Equal(t, model.Cancelled, status, id)
	}
}

// Events of one batch are close together: a few milliseconds apart in PostgreSQL, where the
// document holds an earlier createdAt than the column the store orders by, and often in the same
// millisecond in MongoDB. The session follows the order of the store.
func TestQuarantineSession_EventsOfOneBatch(t *testing.T) {
	tests := []struct {
		name string
		gap  time.Duration
	}{
		{name: "a few milliseconds apart", gap: 2 * time.Millisecond},
		{name: "in the same millisecond", gap: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One batch ends the earlier session with a healthy event and starts the next one.
			store := &sessionStore{}
			store.addEvent("a-older-start", model.Quarantined, 0)
			store.addEventAt("node-a", "a5-older-later", model.AlreadyQuarantined, 10*time.Minute)
			store.addEventAt("node-a", "b-older-healthy", model.UnQuarantined, 10*time.Minute)
			store.addEventAt("node-a", "c-start", model.Quarantined, 10*time.Minute+tt.gap)
			store.addEventAt("node-a", "d-later", model.AlreadyQuarantined, 10*time.Minute+2*tt.gap)
			watcher := newSessionWatcher(store)

			start, quarantined, err := watcher.FindQuarantineSession(context.Background(), "node-a", []string{"d-later"})
			require.NoError(t, err)
			assert.Equal(t, "c-start", start)
			assert.True(t, quarantined)

			require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", start, "test"))
			assert.Equal(t, map[string]model.Status{
				"a-older-start":   model.Quarantined,
				"a5-older-later":  model.AlreadyQuarantined,
				"b-older-healthy": model.UnQuarantined,
				"c-start":         model.Cancelled,
				"d-later":         model.Cancelled,
			}, store.statuses())
		})
	}
}

func TestCancelQuarantineSession_CancelsOnlyThatSession(t *testing.T) {
	transient := errors.New("(NotWritablePrimary) not primary")

	tests := []struct {
		name       string
		updateErrs []error
	}{
		{name: "the writes work"},
		{name: "an update that fails once is retried", updateErrs: []error{transient}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sessionStore{updateErrs: tt.updateErrs}
			store.addEvent("older-start", model.Quarantined, 0)
			store.addEvent("older-later", model.AlreadyQuarantined, 1)
			store.addEvent("older-healthy", model.UnQuarantined, 2)
			store.addEvent("start", model.Quarantined, 10)
			store.addEvent("later", model.AlreadyQuarantined, 11)
			store.addEvent("last", model.AlreadyQuarantined, 12)
			store.addEventAt("node-b", "other-node-later", model.AlreadyQuarantined, 11*time.Minute)
			watcher := newSessionWatcher(store)

			require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test"))
			assert.Equal(t, map[string]model.Status{
				"older-start":      model.Quarantined,
				"older-later":      model.AlreadyQuarantined,
				"older-healthy":    model.UnQuarantined,
				"start":            model.Cancelled,
				"later":            model.Cancelled,
				"last":             model.Cancelled,
				"other-node-later": model.AlreadyQuarantined,
			}, store.statuses())
		})
	}
}

// A newer quarantine of the node is left alone, and so is the ended session before it: the
// cancellation of any session of a node makes fault-remediation clear the remediation state of
// the whole node, which belongs to the newer quarantine by then. Fault-quarantine writes the
// first event of a session before any of its events is AlreadyQuarantined. When that write
// fails, the quarantine annotation of the node still names the first event.
func TestCancelQuarantineSession_NewerQuarantineIsLeftAlone(t *testing.T) {
	newerSession := func(s *sessionStore) {
		s.addEvent("newer-start", model.Quarantined, 20)
		s.addEvent("newer-later", model.AlreadyQuarantined, 21)
	}
	newerStartWithoutStatus := func(s *sessionStore) {
		s.addEvent("6a1f00000000000000000020", "", 20)
		s.addEvent("newer-later", model.AlreadyQuarantined, 21)
	}

	newer := map[string]model.Status{"newer-start": model.Quarantined, "newer-later": model.AlreadyQuarantined}

	tests := []struct {
		name           string
		before         func(s *sessionStore)
		beforeListCall int
		quarantineIDs  []string
		wantOthers      map[string]model.Status
		wantCancelled  bool
	}{
		{name: "the newer quarantine started before the cancellation", before: newerSession, wantOthers: newer},
		{name: "the newer quarantine starts as the events are read", beforeListCall: 2, wantOthers: newer},
		{name: "the newer quarantine starts between the two reads", beforeListCall: 3, wantOthers: newer},
		{
			name:          "the status write of the newer first event failed",
			before:        newerStartWithoutStatus,
			quarantineIDs: []string{"not-a-record-id", "6a1f00000000000000000020"},
			wantOthers: map[string]model.Status{
				"6a1f00000000000000000020": "",
				"newer-later":              model.AlreadyQuarantined,
			},
		},
		{
			name:          "the quarantine annotation names no later event of the node",
			before:        func(s *sessionStore) { s.addEvent("6a1f00000000000000000005", model.UnQuarantined, 5) },
			quarantineIDs: []string{"6a1f00000000000000000005", "6a1f00000000000000000030", "6a1f00000000000000000099"},
			wantOthers:     map[string]model.Status{"6a1f00000000000000000005": model.UnQuarantined},
			wantCancelled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sessionStore{}
			if tt.beforeListCall > 0 {
				store.beforeList = func(s *sessionStore, call int) {
					if call == tt.beforeListCall {
						newerSession(s)
					}
				}
			}

			store.addEvent("start", model.Quarantined, 10)
			store.addEvent("6a1f00000000000000000011", model.AlreadyQuarantined, 11)

			if tt.before != nil {
				tt.before(store)
			}

			store.addEventAt("node-b", "6a1f00000000000000000030", model.Quarantined, 30*time.Minute)

			watcher := newSessionWatcher(store)
			watcher.SetFetchDocIDsFn(func(context.Context, string) []string { return tt.quarantineIDs })

			require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test"))

			want := map[string]model.Status{
				"start":                    model.Quarantined,
				"6a1f00000000000000000011": model.AlreadyQuarantined,
				"6a1f00000000000000000030": model.Quarantined,
			}
			if tt.wantCancelled {
				want["start"], want["6a1f00000000000000000011"] = model.Cancelled, model.Cancelled
			}

			maps.Copy(want, tt.wantOthers)

			assert.Equal(t, want, store.statuses())

			if !tt.wantCancelled {
				assert.Zero(t, store.updateCalls, "nothing of the node is written")
			}
		})
	}
}

// No event of the node gets processed while a session of the node is cancelled, so a newer
// quarantine can't start between the check and the writes. Events of other nodes go on.
func TestCancelQuarantineSession_HoldsTheNode(t *testing.T) {
	reading := make(chan struct{})
	release := make(chan struct{})

	store := &sessionStore{}
	store.beforeList = func(_ *sessionStore, call int) {
		if call == 1 {
			close(reading)
			<-release
		}
	}
	store.addEvent("start", model.Quarantined, 10)
	store.addEvent("later", model.AlreadyQuarantined, 11)
	watcher := newSessionWatcher(store)

	var mu sync.Mutex

	processed := map[string]bool{}

	watcher.SetProcessEventCallback(func(_ context.Context, event *model.HealthEventWithStatus) (*model.Status, error) {
		mu.Lock()
		defer mu.Unlock()

		processed[event.HealthEvent.GetNodeName()] = true

		return nil, nil
	})

	wasProcessed := func(nodeName string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()

			return processed[nodeName]
		}
	}

	cancelled := make(chan error, 1)

	go func() {
		cancelled <- watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test")
	}()

	<-reading

	for _, nodeName := range []string{"node-a", "node-b"} {
		go func() {
			_, _ = watcher.processHealthEvent(context.Background(), &model.HealthEventWithStatus{
				HealthEvent:       &protos.HealthEvent{NodeName: nodeName},
				HealthEventStatus: &protos.HealthEventStatus{},
			}, "event-"+nodeName, "event-"+nodeName)
		}()
	}

	require.Eventually(t, wasProcessed("node-b"), 5*time.Second, 5*time.Millisecond, "another node goes on")
	require.Never(t, wasProcessed("node-a"), 200*time.Millisecond, 5*time.Millisecond,
		"an event of the node waits for the cancellation")

	close(release)
	require.NoError(t, <-cancelled)
	require.Eventually(t, wasProcessed("node-a"), 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, model.Cancelled, store.statuses()["start"])
}

// A multi-document update can fail after it wrote part of the events. The first event of the
// session is written last, so it stays Quarantined, and any event of the session still leads to
// it, so a later call cancels the rest.
func TestCancelQuarantineSession_PartialWriteIsFinishedByTheNextCall(t *testing.T) {
	transient := errors.New("(NotWritablePrimary) not primary")

	store := &sessionStore{
		updateErrs:     slices.Repeat([]error{transient}, liveRecoveryStoreRetryAttempts),
		partialUpdates: 1,
	}
	store.addEvent("start", model.Quarantined, 10)
	store.addEvent("later", model.AlreadyQuarantined, 11)
	store.addEvent("last", model.AlreadyQuarantined, 12)
	watcher := newSessionWatcher(store)

	err := watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test")
	require.ErrorIs(t, err, transient)

	statuses := store.statuses()
	assert.Equal(t, model.Quarantined, statuses["start"], "the first event is written last")
	assert.Equal(t, model.Cancelled, statuses["later"], "the failed update wrote one event")
	assert.Equal(t, model.AlreadyQuarantined, statuses["last"])

	start, quarantined, err := watcher.FindQuarantineSession(context.Background(), "node-a", []string{"later"})
	require.NoError(t, err)
	require.True(t, quarantined)
	require.Equal(t, "start", start, "the written event still leads to the session")

	require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", start, "test"))
	assert.Equal(t, map[string]model.Status{
		"start": model.Cancelled,
		"later": model.Cancelled,
		"last":  model.Cancelled,
	}, store.statuses())
}

func TestCancelQuarantineSession_NothingToCancel(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *sessionStore)
	}{
		{
			name: "a healthy event ended the session",
			setup: func(s *sessionStore) {
				s.addEvent("start", model.Quarantined, 10)
				s.addEvent("later", model.AlreadyQuarantined, 11)
				s.addEvent("healthy", model.UnQuarantined, 12)
			},
		},
		{
			name: "the session is already cancelled",
			setup: func(s *sessionStore) {
				s.addEvent("start", model.Cancelled, 10)
				s.addEvent("later", model.AlreadyQuarantined, 11)
			},
		},
		{
			name:  "the first event is not stored",
			setup: func(s *sessionStore) { s.addEvent("later", model.AlreadyQuarantined, 11) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sessionStore{}
			tt.setup(store)
			before := store.statuses()
			watcher := newSessionWatcher(store)

			require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test"))
			assert.Equal(t, before, store.statuses())
			assert.Zero(t, store.updateCalls)
		})
	}
}

func TestCancelQuarantineSession_StopsAtTheCallersDeadline(t *testing.T) {
	store := &sessionStore{
		updateErrs: []error{errors.New("not primary"), errors.New("not primary"), errors.New("not primary")},
	}
	store.addEvent("start", model.Quarantined, 10)
	watcher := newSessionWatcher(store)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := watcher.CancelQuarantineSession(ctx, "node-a", "start", "test")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, store.updateCalls, liveRecoveryStoreRetryAttempts, "the retry must stop at the deadline")
}

func TestCancelQuarantineSession_ListThatFailsOnceIsRetried(t *testing.T) {
	store := &sessionStore{listErrs: []error{errors.New("(NotWritablePrimary) not primary")}}
	store.addEvent("start", model.Quarantined, 10)
	store.addEvent("later", model.AlreadyQuarantined, 11)
	watcher := newSessionWatcher(store)

	require.NoError(t, watcher.CancelQuarantineSession(context.Background(), "node-a", "start", "test"))
	assert.Equal(t, 4, store.listCalls, "three lists, and the first one is tried twice")
	assert.Equal(t, map[string]model.Status{"start": model.Cancelled, "later": model.Cancelled}, store.statuses())
}
