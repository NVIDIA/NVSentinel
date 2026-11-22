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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

// cancelStore is a fake datastore that holds the events of a few nodes and applies the cancel's
// filters the way MongoDB renders them.
type cancelStore struct {
	client.DatabaseClient
	datastore.HealthEventStore

	events []*storedEvent

	findErrs   []error
	findCalls  int
	updateErrs []error
	// updates holds the status that each update call cancelled, failed calls included.
	updates []model.Status
}

type storedEvent struct {
	id        string
	node      string
	status    model.Status
	createdAt time.Time
}

const (
	cancelTestNode  = "node-a"
	firstEventID    = "6741f2a0c3b1d4e5f6a7b8c1"
	secondEventID   = "6741f2a0c3b1d4e5f6a7b8c2"
	thirdEventID    = "6741f2a0c3b1d4e5f6a7b8c3"
	earlierFirstID  = "6741f2a0c3b1d4e5f6a7b8a1"
	earlierSecondID = "6741f2a0c3b1d4e5f6a7b8a2"
	earlierEndID    = "6741f2a0c3b1d4e5f6a7b8a3"
	otherNodeID     = "6741f2a0c3b1d4e5f6a7b8d1"
)

var cancelTestStart = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// add stores an event for a node, created the given time after the test start.
func (s *cancelStore) add(node, id string, status model.Status, after time.Duration) {
	s.events = append(s.events, &storedEvent{id: id, node: node, status: status, createdAt: cancelTestStart.Add(after)})
}

func (s *cancelStore) statuses() map[string]model.Status {
	statuses := make(map[string]model.Status, len(s.events))
	for _, event := range s.events {
		statuses[event.id] = event.status
	}

	return statuses
}

// newCancelStore returns a store with an earlier quarantine of the node that a healthy event
// ended, the node's current quarantine, and a quarantine of another node.
func newCancelStore() *cancelStore {
	store := &cancelStore{}

	store.add(cancelTestNode, earlierFirstID, model.Quarantined, 0)
	store.add(cancelTestNode, earlierSecondID, model.AlreadyQuarantined, time.Minute)
	store.add(cancelTestNode, earlierEndID, model.UnQuarantined, 2*time.Minute)

	store.add(cancelTestNode, firstEventID, model.Quarantined, 10*time.Minute)
	store.add(cancelTestNode, secondEventID, model.AlreadyQuarantined, 11*time.Minute)
	store.add(cancelTestNode, thirdEventID, model.AlreadyQuarantined, 12*time.Minute)

	store.add("node-b", otherNodeID, model.Quarantined, 11*time.Minute)

	return store
}

func nextStoreErr(errs []error, calls int) error {
	if index := calls - 1; index < len(errs) {
		return errs[index]
	}

	return nil
}

func (s *cancelStore) matching(filter any) ([]*storedEvent, error) {
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
		case "$gte":
			matches = !value.(time.Time).Before(operand.(time.Time))
		default:
			panic("unexpected operator " + operator)
		}

		if !matches {
			return false
		}
	}

	return true
}

// FindLatestHealthEventByQuery returns the newest match by creation time and then by ID, as
// both stores do.
func (s *cancelStore) FindLatestHealthEventByQuery(
	_ context.Context, builder datastore.QueryBuilder,
) (*datastore.HealthEventWithStatus, error) {
	s.findCalls++

	if err := nextStoreErr(s.findErrs, s.findCalls); err != nil {
		return nil, err
	}

	matched, err := s.matching(builder)
	if err != nil || len(matched) == 0 {
		return nil, err
	}

	latest := slices.MaxFunc(matched, func(a, b *storedEvent) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}

		return strings.Compare(a.id, b.id)
	})

	status := datastore.Status(latest.status)

	return &datastore.HealthEventWithStatus{
		CreatedAt:         latest.createdAt,
		HealthEventStatus: datastore.HealthEventStatus{NodeQuarantined: &status},
		RawEvent: map[string]any{
			"healthevent": map[string]any{
				"nodename":           latest.node,
				"recommendedaction":  int32(protos.RecommendedAction_RESTART_VM),
				"generatedtimestamp": map[string]any{"seconds": latest.createdAt.Unix()},
			},
			"healtheventstatus": map[string]any{"nodequarantined": string(latest.status)},
		},
	}, nil
}

func (s *cancelStore) UpdateManyDocuments(_ context.Context, filter any, _ any) (*client.UpdateResult, error) {
	matched, err := s.matching(filter)
	if err != nil {
		return nil, err
	}

	s.updates = append(s.updates, cancelledStatus(filter))

	if err := nextStoreErr(s.updateErrs, len(s.updates)); err != nil {
		return nil, err
	}

	for _, event := range matched {
		event.status = model.Cancelled
	}

	return &client.UpdateResult{MatchedCount: int64(len(matched)), ModifiedCount: int64(len(matched))}, nil
}

// cancelledStatus returns the status that the filter of an update selects.
func cancelledStatus(filter any) model.Status {
	return statusOfFilter(filter.(*query.Builder).ToMongo())
}

func statusOfFilter(filter map[string]any) model.Status {
	if status, ok := filter["healtheventstatus.nodequarantined"].(string); ok {
		return model.Status(status)
	}

	parts, _ := filter["$and"].([]any)
	for _, part := range parts {
		if status := statusOfFilter(part.(map[string]any)); status != "" {
			return status
		}
	}

	return ""
}

func newCancelWatcher(store *cancelStore) *EventWatcher {
	watcher := NewEventWatcher(nil, store, time.Minute, &objectIDStoreStub{})
	watcher.SetHealthEventStore(store, datastore.ProviderMongoDB)

	return watcher
}

func remediationDurationSamples(t *testing.T) uint64 {
	t.Helper()

	histogram, err := metrics.NodeRemediationDurationSeconds.GetMetricWithLabelValues(
		protos.RecommendedAction_RESTART_VM.String())
	require.NoError(t, err)

	metric := &dto.Metric{}
	require.NoError(t, histogram.(interface{ Write(*dto.Metric) error }).Write(metric))

	return metric.GetHistogram().GetSampleCount()
}

func TestCancelLatestQuarantiningEvents_CancelsTheOpenSession(t *testing.T) {
	store := newCancelStore()
	samplesBefore := remediationDurationSamples(t)

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstEventID, secondEventID, thirdEventID}, "test")
	require.NoError(t, err)

	assert.Equal(t, map[string]model.Status{
		earlierFirstID:  model.Quarantined,
		earlierSecondID: model.AlreadyQuarantined,
		earlierEndID:    model.UnQuarantined,
		firstEventID:    model.Cancelled,
		secondEventID:   model.Cancelled,
		thirdEventID:    model.Cancelled,
		otherNodeID:     model.Quarantined,
	}, store.statuses(), "only the open session of the node is cancelled")

	assert.Equal(t, samplesBefore+1, remediationDurationSamples(t), "the cancelled session is counted once")
}

// The quarantine annotation holds one event per entity, so it doesn't name every event. The
// events after the first one are cancelled without their IDs.
func TestCancelLatestQuarantiningEvents_CancelsEventsTheAnnotationDoesNotName(t *testing.T) {
	store := newCancelStore()

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, nil, "test")
	require.NoError(t, err)

	statuses := store.statuses()
	assert.Equal(t, model.Cancelled, statuses[firstEventID])
	assert.Equal(t, model.Cancelled, statuses[secondEventID])
	assert.Equal(t, model.Cancelled, statuses[thirdEventID])
	assert.Equal(t, model.Quarantined, statuses[earlierFirstID])
	assert.Equal(t, model.AlreadyQuarantined, statuses[earlierSecondID])
}

// Two events of one batch can be stored a few milliseconds apart in the other order, so an
// AlreadyQuarantined event can sit just before the first event. The annotation names it, and
// it's cancelled by its ID.
func TestCancelLatestQuarantiningEvents_CancelsAnEventStoredBeforeTheFirstEvent(t *testing.T) {
	const storedBeforeID = "6741f2a0c3b1d4e5f6a7b8c0"

	store := newCancelStore()
	store.add(cancelTestNode, storedBeforeID, model.AlreadyQuarantined, 10*time.Minute-3*time.Millisecond)

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstEventID, storedBeforeID}, "test")
	require.NoError(t, err)

	statuses := store.statuses()
	assert.Equal(t, model.Cancelled, statuses[storedBeforeID], "the event before the first event is cancelled")
	assert.Equal(t, model.Cancelled, statuses[firstEventID])
	assert.Equal(t, model.Cancelled, statuses[secondEventID])
	assert.Equal(t, model.Cancelled, statuses[thirdEventID])
	assert.Equal(t, model.AlreadyQuarantined, statuses[earlierSecondID], "the earlier session is left as it is")
}

// The first event stays Quarantined until the other events are cancelled, so a call that fails
// partway is finished by the next one.
func TestCancelLatestQuarantiningEvents_FirstEventIsCancelledLast(t *testing.T) {
	storeErr := errors.New("(NotWritablePrimary) not primary")
	ids := []string{firstEventID, secondEventID, thirdEventID}

	tests := []struct {
		name         string
		failedUpdate int
		wantAfter    map[string]model.Status
	}{
		{
			name:         "the first update fails",
			failedUpdate: 1,
			wantAfter: map[string]model.Status{
				firstEventID: model.Quarantined, secondEventID: model.AlreadyQuarantined, thirdEventID: model.AlreadyQuarantined,
			},
		},
		{
			name:         "the update of the first event fails",
			failedUpdate: 3,
			wantAfter: map[string]model.Status{
				firstEventID: model.Quarantined, secondEventID: model.Cancelled, thirdEventID: model.Cancelled,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newCancelStore()
			watcher := newCancelWatcher(store)

			// Every attempt of one update fails.
			store.updateErrs = make([]error, tt.failedUpdate-1+liveRecoveryStoreRetryAttempts)
			for i := tt.failedUpdate - 1; i < len(store.updateErrs); i++ {
				store.updateErrs[i] = storeErr
			}

			err := watcher.CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, ids, "test")
			require.ErrorIs(t, err, storeErr)

			statuses := store.statuses()
			for id, want := range tt.wantAfter {
				assert.Equal(t, want, statuses[id], "event %s after the failed call", id)
			}

			failedCall := len(store.updates)

			require.NoError(t, watcher.CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, ids, "test"))

			statuses = store.statuses()
			for _, id := range ids {
				assert.Equal(t, model.Cancelled, statuses[id], "event %s after the next call", id)
			}

			for _, updates := range [][]model.Status{store.updates[:failedCall], store.updates[failedCall:]} {
				firstQuarantined := slices.Index(updates, model.Quarantined)
				if firstQuarantined != -1 {
					assert.NotContains(t, updates[firstQuarantined:], model.AlreadyQuarantined,
						"a call cancels the AlreadyQuarantined events before a Quarantined event")
				}
			}

			assert.Contains(t, store.updates[failedCall:], model.Quarantined)
		})
	}
}

// The node cleanup can still fail after the events are cancelled, and then the cancel runs
// again. It finds nothing open and changes nothing.
func TestCancelLatestQuarantiningEvents_SecondCallChangesNothing(t *testing.T) {
	store := newCancelStore()
	watcher := newCancelWatcher(store)
	ids := []string{firstEventID, secondEventID, thirdEventID}

	require.NoError(t, watcher.CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, ids, "test"))

	after := store.statuses()
	samples := remediationDurationSamples(t)

	require.NoError(t, watcher.CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, ids, "test"))

	assert.Equal(t, after, store.statuses())
	assert.Equal(t, samples, remediationDurationSamples(t), "the session is not counted again")
}

func TestCancelLatestQuarantiningEvents_NoOpenSession(t *testing.T) {
	tests := []struct {
		name     string
		store    func() *cancelStore
		eventIDs []string
	}{
		{
			name:  "no event of the node",
			store: func() *cancelStore { return &cancelStore{} },
		},
		{
			name: "a healthy event ended the latest session",
			store: func() *cancelStore {
				store := &cancelStore{}
				store.add(cancelTestNode, earlierFirstID, model.Quarantined, 0)
				store.add(cancelTestNode, earlierSecondID, model.AlreadyQuarantined, time.Minute)
				store.add(cancelTestNode, earlierEndID, model.UnQuarantined, 2*time.Minute)

				return store
			},
		},
		{
			name:     "IDs of an older version",
			store:    func() *cancelStore { return &cancelStore{} },
			eventIDs: []string{"12345", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.store()
			before := store.statuses()

			err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
				context.Background(), cancelTestNode, tt.eventIDs, "test")
			require.NoError(t, err)

			assert.Equal(t, before, store.statuses())
			assert.Empty(t, store.updates, "nothing is written")
		})
	}
}

// The status write of the first event can fail, which leaves no Quarantined event. The later
// events are still cancelled by the IDs in the quarantine annotation.
func TestCancelLatestQuarantiningEvents_FirstEventWithoutStatus(t *testing.T) {
	store := &cancelStore{}
	store.add(cancelTestNode, earlierFirstID, model.Quarantined, 0)
	store.add(cancelTestNode, earlierEndID, model.UnQuarantined, 2*time.Minute)
	store.add(cancelTestNode, firstEventID, "", 10*time.Minute)
	store.add(cancelTestNode, secondEventID, model.AlreadyQuarantined, 11*time.Minute)

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstEventID, secondEventID}, "test")
	require.NoError(t, err)

	assert.Equal(t, map[string]model.Status{
		earlierFirstID: model.Quarantined,
		earlierEndID:   model.UnQuarantined,
		firstEventID:   "",
		secondEventID:  model.Cancelled,
	}, store.statuses())
}

// An earlier quarantine whose cancel was lost has no UnQuarantined event after it. The cancel
// starts at the current quarantine's first event and leaves the earlier one alone.
func TestCancelLatestQuarantiningEvents_LeavesAnEarlierSessionThatWasNeverEnded(t *testing.T) {
	store := &cancelStore{}
	store.add(cancelTestNode, earlierFirstID, model.Quarantined, 0)
	store.add(cancelTestNode, earlierSecondID, model.AlreadyQuarantined, time.Minute)
	store.add(cancelTestNode, firstEventID, model.Quarantined, 10*time.Minute)
	store.add(cancelTestNode, secondEventID, model.AlreadyQuarantined, 11*time.Minute)

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstEventID, secondEventID}, "test")
	require.NoError(t, err)

	assert.Equal(t, map[string]model.Status{
		earlierFirstID:  model.Quarantined,
		earlierSecondID: model.AlreadyQuarantined,
		firstEventID:    model.Cancelled,
		secondEventID:   model.Cancelled,
	}, store.statuses())
}

// An annotation can outlive a datastore change, and older FQ versions wrote other values. Only
// IDs in the store's own format reach it, because PostgreSQL rejects a query for an ID that
// isn't a UUID.
func TestCancelLatestQuarantiningEvents_UsesOnlyTheRecordIDsOfTheStore(t *testing.T) {
	const (
		firstUUID  = "0b5f3f0e-6f0a-4d0b-9c58-2f2f4a1d9e01"
		beforeUUID = "0b5f3f0e-6f0a-4d0b-9c58-2f2f4a1d9e02"
	)

	tests := []struct {
		name     string
		provider datastore.DataStoreProvider
		wantIDs  []any
	}{
		{"MongoDB takes the ObjectIDs", datastore.ProviderMongoDB, []any{firstEventID}},
		{"PostgreSQL takes the UUIDs", datastore.ProviderPostgreSQL, []any{firstUUID, beforeUUID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			watcher := NewEventWatcher(nil, &cancelStore{}, time.Minute, &objectIDStoreStub{})
			watcher.SetHealthEventStore(&cancelStore{}, tt.provider)

			assert.Equal(t, tt.wantIDs, watcher.recordIDs([]string{firstEventID, firstUUID, "12345", "", beforeUUID}))
		})
	}

	// On PostgreSQL the events are cancelled by their UUIDs.
	store := &cancelStore{}
	store.add(cancelTestNode, beforeUUID, model.AlreadyQuarantined, 10*time.Minute-time.Millisecond)
	store.add(cancelTestNode, firstUUID, model.Quarantined, 10*time.Minute)

	watcher := NewEventWatcher(nil, store, time.Minute, &objectIDStoreStub{})
	watcher.SetHealthEventStore(store, datastore.ProviderPostgreSQL)

	err := watcher.CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstUUID, beforeUUID, firstEventID}, "test")
	require.NoError(t, err)

	assert.Equal(t, map[string]model.Status{firstUUID: model.Cancelled, beforeUUID: model.Cancelled}, store.statuses())
}

func TestCancelLatestQuarantiningEvents_StoreErrorThatClearsIsRetried(t *testing.T) {
	storeErr := errors.New("(NotWritablePrimary) not primary")

	store := newCancelStore()
	store.findErrs = []error{storeErr}
	store.updateErrs = []error{storeErr}

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(
		context.Background(), cancelTestNode, []string{firstEventID}, "test")
	require.NoError(t, err)

	assert.Equal(t, 2, store.findCalls)

	statuses := store.statuses()
	assert.Equal(t, model.Cancelled, statuses[firstEventID])
	assert.Equal(t, model.Cancelled, statuses[secondEventID])
	assert.Equal(t, model.Cancelled, statuses[thirdEventID])
}

func TestCancelLatestQuarantiningEvents_StopsAtTheCallersDeadline(t *testing.T) {
	storeErr := errors.New("(NotWritablePrimary) not primary")

	store := newCancelStore()
	store.findErrs = slices.Repeat([]error{storeErr}, liveRecoveryStoreRetryAttempts)

	// The deadline ends during the wait before the third attempt, so the store error of the
	// last attempt is never returned.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := newCancelWatcher(store).CancelLatestQuarantiningEvents(ctx, cancelTestNode, []string{firstEventID}, "test")

	require.ErrorIs(t, err, context.DeadlineExceeded, "the retries stop at the deadline of the caller")
	assert.Less(t, store.findCalls, liveRecoveryStoreRetryAttempts)
	assert.Equal(t, model.Quarantined, store.statuses()[firstEventID])
}

func TestCancelLatestQuarantiningEvents_NeedsTheHealthEventStore(t *testing.T) {
	watcher := NewEventWatcher(nil, newCancelStore(), time.Minute, &objectIDStoreStub{})

	err := watcher.CancelLatestQuarantiningEvents(context.Background(), cancelTestNode, nil, "test")
	require.ErrorContains(t, err, "health event store is not configured")
}
