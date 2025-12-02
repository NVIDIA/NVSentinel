// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package postgresql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResumeTokenAdvancedOnSend verifies that lastEventID IS updated
// when an event is sent to the channel. This ensures that MarkProcessed()
// with empty token correctly uses the position of the last SENT event.
// When an event is sent but not yet processed, it can be redelivered on restart
// because it hasn't been marked as processed in the database yet.
func TestResumeTokenAdvancedOnSend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition - start from event ID 0
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	watcher := &PostgreSQLChangeStreamWatcher{
		db:          db,
		clientName:  clientName,
		tableName:   tableName,
		events:      make(chan datastore.EventWithToken, 10),
		stopCh:      make(chan struct{}),
		lastEventID: 0,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), watcher.lastEventID, "Should start at event ID 0")

	// Simulate processing an event
	recordID := uuid.New()
	newValues := map[string]interface{}{
		"id":   recordID.String(),
		"name": "test",
	}

	events := []datastore.EventWithToken{
		{
			Event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "1",
				},
				"operationType": "insert",
				"fullDocument":  newValues,
			},
			ResumeToken: []byte("1"),
		},
	}

	// Send event to channel
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Verify lastEventID WAS updated after sending
	assert.Equal(t, int64(1), watcher.lastEventID,
		"lastEventID SHOULD be updated in sendEventsToChannel to track last sent event")

	// Receive the event from channel
	select {
	case event := <-watcher.events:
		assert.Equal(t, []byte("1"), event.ResumeToken)

		// Now simulate the application calling MarkProcessed
		mock.ExpectExec("UPDATE datastore_changelog SET processed").
			WithArgs(int64(1), tableName).
			WillReturnResult(sqlmock.NewResult(0, 1))

		mock.ExpectExec("INSERT INTO resume_tokens").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err = watcher.MarkProcessed(ctx, event.ResumeToken)
		require.NoError(t, err)

		// lastEventID remains at 1 (was already set in sendEventsToChannel)
		assert.Equal(t, int64(1), watcher.lastEventID,
			"lastEventID should remain 1 after MarkProcessed")

	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for event")
	}

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResumeTokenAdvancedEvenForFilteredEvents verifies THE CRITICAL FIX:
// lastEventID MUST advance even when events are filtered by the pipeline.
// This prevents the deadlock bug where filtered events would be re-fetched infinitely.
//
// BEFORE FIX: lastEventID stayed at 0, causing infinite re-fetch of event 1
// AFTER FIX: lastEventID advances to 1, next poll skips event 1
func TestResumeTokenAdvancedEvenForFilteredEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	// Create a pipeline filter that rejects all events
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"operationType": "update", // Only accept updates
			},
		},
	})
	require.NoError(t, err)

	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 10),
		stopCh:         make(chan struct{}),
		lastEventID:    0,
		pipelineFilter: pipelineFilter,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Create an INSERT event (which will be filtered out)
	recordID := uuid.New()
	newValues := map[string]interface{}{
		"id":   recordID.String(),
		"name": "test",
	}

	events := []datastore.EventWithToken{
		{
			Event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "1",
				},
				"operationType": "insert", // Will be filtered (we only accept "update")
				"fullDocument":  newValues,
			},
			ResumeToken: []byte("1"),
		},
	}

	// Send events through pipeline filter
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Verify event was filtered and NOT sent to channel
	select {
	case <-watcher.events:
		t.Fatal("Event should have been filtered out")
	case <-time.After(100 * time.Millisecond):
		// Expected - event was filtered
	}

	// 🔥 THE CRITICAL FIX VERIFICATION 🔥
	// lastEventID MUST advance to 1 even though the event was filtered.
	// This ensures next poll fetches WHERE id > 1, skipping the filtered event.
	assert.Equal(t, int64(1), watcher.lastEventID,
		"lastEventID MUST advance for filtered events to prevent infinite re-fetch")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResumeAfterRestartSkipsProcessedEvents verifies that when a watcher
// restarts with lastEventID=N, it correctly resumes from WHERE id > N,
// skipping event N (which was already processed before the restart).
func TestResumeAfterRestartSkipsProcessedEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Simulate restart: loadResumePosition returns eventID=5
	// (meaning events 1-5 were processed before restart)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 5, "timestamp": "2025-01-01T00:00:00Z"}`))

	watcher := &PostgreSQLChangeStreamWatcher{
		db:         db,
		clientName: clientName,
		tableName:  tableName,
		events:     make(chan datastore.EventWithToken, 10),
		stopCh:     make(chan struct{}),
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), watcher.lastEventID, "Should load last processed event ID")

	// Mock fetchNewChanges query: should fetch WHERE (changed_at > $2 OR (changed_at = $2 AND id > $3))
	// Now uses timestamp-based resume (3 args: tableName, timestamp, eventID)
	recordID := uuid.New()
	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs(tableName, sqlmock.AnyArg(), int64(5)). // timestamp-based resume
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}).
			AddRow(6, recordID, "INSERT", nil, []byte(`{"id": "test"}`), time.Now()).
			AddRow(7, recordID, "INSERT", nil, []byte(`{"id": "test2"}`), time.Now()))

	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	// Verify we received events 6 and 7 (not 5)
	event1 := <-watcher.events
	assert.Equal(t, []byte("6"), event1.ResumeToken)

	event2 := <-watcher.events
	assert.Equal(t, []byte("7"), event2.ResumeToken)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestConcurrentClientsWithDifferentFilters verifies that two clients
// with different pipeline filters can process the same events independently
// without interfering with each other's resume tokens.
func TestConcurrentClientsWithDifferentFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	tableName := "test_table"

	// Client 1: only accepts INSERT operations
	client1Name := "insert-client"
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(client1Name).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	pipelineFilter1, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"operationType": "insert",
			},
		},
	})
	require.NoError(t, err)

	watcher1 := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     client1Name,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 10),
		stopCh:         make(chan struct{}),
		pipelineFilter: pipelineFilter1,
	}

	err = watcher1.loadResumePosition(ctx)
	require.NoError(t, err)

	// Client 2: only accepts UPDATE operations
	client2Name := "update-client"
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(client2Name).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	pipelineFilter2, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"operationType": "update",
			},
		},
	})
	require.NoError(t, err)

	watcher2 := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     client2Name,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 10),
		stopCh:         make(chan struct{}),
		pipelineFilter: pipelineFilter2,
	}

	err = watcher2.loadResumePosition(ctx)
	require.NoError(t, err)

	// Create mixed events
	events := []datastore.EventWithToken{
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "1"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"id": "1"},
			},
			ResumeToken: []byte("1"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "2"},
				"operationType": "update",
				"fullDocument":  map[string]interface{}{"id": "2"},
			},
			ResumeToken: []byte("2"),
		},
	}

	// Send to both watchers
	err = watcher1.sendEventsToChannel(ctx, events)
	require.NoError(t, err)
	err = watcher2.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Client 1 should only receive INSERT (event 1)
	select {
	case event := <-watcher1.events:
		assert.Equal(t, "insert", event.Event["operationType"])
		assert.Equal(t, []byte("1"), event.ResumeToken)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Client 1 should have received INSERT event")
	}

	// Client 1 should NOT receive UPDATE
	select {
	case <-watcher1.events:
		t.Fatal("Client 1 should NOT receive UPDATE event")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	// Client 2 should only receive UPDATE (event 2)
	select {
	case event := <-watcher2.events:
		assert.Equal(t, "update", event.Event["operationType"])
		assert.Equal(t, []byte("2"), event.ResumeToken)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Client 2 should have received UPDATE event")
	}

	// Client 2 should NOT receive INSERT
	select {
	case <-watcher2.events:
		t.Fatal("Client 2 should NOT receive INSERT event")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	// Now simulate each client calling MarkProcessed for their respective events
	// Client 1 marks event 1
	mock.ExpectExec("UPDATE datastore_changelog SET processed").
		WithArgs(int64(1), tableName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO resume_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = watcher1.MarkProcessed(ctx, []byte("1"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), watcher1.lastEventID)

	// Client 2 marks event 2
	mock.ExpectExec("UPDATE datastore_changelog SET processed").
		WithArgs(int64(2), tableName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO resume_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = watcher2.MarkProcessed(ctx, []byte("2"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), watcher2.lastEventID)

	// Verify each client has independent resume positions
	assert.Equal(t, int64(1), watcher1.lastEventID, "Client 1 should be at event 1")
	assert.Equal(t, int64(2), watcher2.lastEventID, "Client 2 should be at event 2")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkProcessedUpdatesResumeToken verifies that MarkProcessed correctly
// updates both the in-memory lastEventID and persists it to the database.
func TestMarkProcessedUpdatesResumeToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	watcher := &PostgreSQLChangeStreamWatcher{
		db:          db,
		clientName:  clientName,
		tableName:   tableName,
		lastEventID: 0,
	}

	// Mock the UPDATE datastore_changelog query
	mock.ExpectExec("UPDATE datastore_changelog SET processed = TRUE").
		WithArgs(int64(42), tableName).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock the saveResumePosition INSERT/UPDATE
	mock.ExpectExec("INSERT INTO resume_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = watcher.MarkProcessed(ctx, []byte("42"))
	require.NoError(t, err)

	// Verify in-memory lastEventID was updated
	assert.Equal(t, int64(42), watcher.lastEventID,
		"lastEventID should be updated to 42")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestEmptyTokenDoesNotUpdateResumePosition verifies that calling
// MarkProcessed with an empty token uses the current lastEventID.
func TestEmptyTokenDoesNotUpdateResumePosition(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := &PostgreSQLChangeStreamWatcher{
		db:          db,
		lastEventID: 5,
		tableName:   "health_events",
		clientName:  "test-client",
	}

	// Expect UPDATE query using lastEventID when token is empty
	mock.ExpectExec(`UPDATE datastore_changelog`).
		WithArgs(5, "health_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect saveResumePosition - it takes client_name and resume_token (JSON)
	mock.ExpectExec(`INSERT INTO resume_tokens`).
		WithArgs("test-client", sqlmock.AnyArg()). // AnyArg for the JSON token
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Call with empty token - should use lastEventID
	err = watcher.MarkProcessed(ctx, []byte{})
	require.NoError(t, err)

	// lastEventID should remain 5
	assert.Equal(t, int64(5), watcher.lastEventID,
		"lastEventID should be 5")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMultipleFilteredEventsAdvancePosition verifies that when MULTIPLE events
// are filtered in a batch, lastEventID advances to the LAST event ID.
// This simulates the real-world scenario from CI logs where 11 events were filtered.
func TestMultipleFilteredEventsAdvancePosition(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition - start from event ID 917 (like in the CI logs)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 917, "timestamp": "2025-01-01T00:00:00Z"}`))

	// Create a pipeline filter that rejects all events
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.nodeName": "nonexistent-node", // Reject all
			},
		},
	})
	require.NoError(t, err)

	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 100),
		stopCh:         make(chan struct{}),
		lastEventID:    917,
		pipelineFilter: pipelineFilter,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(917), watcher.lastEventID)

	// Simulate 11 events (IDs 918-928) being fetched and ALL filtered
	// This matches the exact scenario from the CI logs
	events := make([]datastore.EventWithToken, 11)
	for i := 0; i < 11; i++ {
		eventID := 918 + i
		eventIDStr := fmt.Sprintf("%d", eventID)
		events[i] = datastore.EventWithToken{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": eventIDStr},
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"nodeName": "nvsentinel-worker2", // Doesn't match filter
					"id":       uuid.New().String(),
				},
			},
			ResumeToken: []byte(eventIDStr),
		}
	}

	// Send all 11 events through the filter
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Verify NO events were sent to channel (all filtered)
	select {
	case <-watcher.events:
		t.Fatal("All events should have been filtered out")
	case <-time.After(100 * time.Millisecond):
		// Expected - all events filtered
	}

	// 🔥 CRITICAL: lastEventID must advance to 928 (last event in batch)
	// BEFORE FIX: Would stay at 917, causing infinite re-fetch
	// AFTER FIX: Advances to 928, next poll fetches WHERE id > 928
	assert.Equal(t, int64(928), watcher.lastEventID,
		"lastEventID must advance to LAST event ID (928) even when all 11 events filtered")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMixedFilteredAndPassedEvents verifies that when some events are filtered
// and some pass, lastEventID advances correctly through ALL events.
func TestMixedFilteredAndPassedEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	// Create a pipeline filter that only accepts worker1 events
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.nodeName": "worker1",
			},
		},
	})
	require.NoError(t, err)

	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 100),
		stopCh:         make(chan struct{}),
		lastEventID:    0,
		pipelineFilter: pipelineFilter,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Create events: worker2, worker1, worker2, worker1, worker2
	// Filter passes: NO, YES, NO, YES, NO
	events := []datastore.EventWithToken{
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "1"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": "worker2", "id": "1"},
			},
			ResumeToken: []byte("1"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "2"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": "worker1", "id": "2"},
			},
			ResumeToken: []byte("2"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "3"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": "worker2", "id": "3"},
			},
			ResumeToken: []byte("3"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "4"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": "worker1", "id": "4"},
			},
			ResumeToken: []byte("4"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "5"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": "worker2", "id": "5"},
			},
			ResumeToken: []byte("5"),
		},
	}

	// Send all events
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Should receive exactly 2 events (IDs 2 and 4)
	receivedEvents := make([]datastore.EventWithToken, 0, 2)
	timeout := time.After(200 * time.Millisecond)
	for len(receivedEvents) < 2 {
		select {
		case event := <-watcher.events:
			receivedEvents = append(receivedEvents, event)
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	// Verify we received the correct events
	assert.Len(t, receivedEvents, 2)
	assert.Equal(t, []byte("2"), receivedEvents[0].ResumeToken)
	assert.Equal(t, []byte("4"), receivedEvents[1].ResumeToken)

	// No more events should be in the channel
	select {
	case <-watcher.events:
		t.Fatal("Should only receive 2 events")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	// 🔥 CRITICAL: lastEventID must be 5 (last event ID processed)
	// Even though events 1, 3, 5 were filtered, position advances through ALL
	assert.Equal(t, int64(5), watcher.lastEventID,
		"lastEventID must advance to 5 (through all events including filtered)")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestFilteredEventsDoNotBlockChannel verifies that filtered events
// don't consume channel buffer space (regression test for potential deadlock).
func TestFilteredEventsDoNotBlockChannel(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	// Create a pipeline filter that only accepts worker1
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.nodeName": "worker1",
			},
		},
	})
	require.NoError(t, err)

	// Create watcher with SMALL channel buffer
	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 2), // Only 2 slots!
		stopCh:         make(chan struct{}),
		lastEventID:    0,
		pipelineFilter: pipelineFilter,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Create 100 events: 98 worker2 (filtered), 2 worker1 (passed)
	events := make([]datastore.EventWithToken, 100)
	for i := 0; i < 100; i++ {
		nodeName := "worker2" // Default: filtered
		if i == 50 || i == 99 {
			nodeName = "worker1" // Only 2 pass
		}
		eventIDStr := fmt.Sprintf("%d", i+1)
		events[i] = datastore.EventWithToken{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": eventIDStr},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"nodeName": nodeName, "id": eventIDStr},
			},
			ResumeToken: []byte(eventIDStr),
		}
	}

	// Send all 100 events - should NOT block even though channel buffer is only 2
	// because filtered events don't go into the channel
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err, "sendEventsToChannel should not block on filtered events")

	// Verify we can receive the 2 passed events
	event1 := <-watcher.events
	event2 := <-watcher.events
	assert.Equal(t, []byte("51"), event1.ResumeToken) // Event 51 (index 50)
	assert.Equal(t, []byte("100"), event2.ResumeToken) // Event 100 (index 99)

	// lastEventID should be 100 (last event processed, regardless of filtering)
	assert.Equal(t, int64(100), watcher.lastEventID)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestSimulateRealWorldCIScenario simulates the EXACT scenario from CI logs:
// - 11 events (IDs 918-928) inserted for worker2 with various XID errors
// - All events filtered because test only listens for specific events
// - Before fix: deadlock (6+ minute delays)
// - After fix: position advances, next poll skips filtered events
func TestSimulateRealWorldCIScenario(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "health-events-analyzer"
	tableName := "health_events"

	// Mock loadResumePosition - start from 917 (like CI logs)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 917, "timestamp": "2025-11-23T14:33:48Z"}`))

	// Create pipeline filter matching the test setup
	// Tests use pipeline filters to only listen for specific node names or check names
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.checkName": "SpecificCheck", // Rejects all test events
			},
		},
	})
	require.NoError(t, err)

	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 100),
		stopCh:         make(chan struct{}),
		lastEventID:    917,
		pipelineFilter: pipelineFilter,
		pollInterval:   500 * time.Millisecond,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Simulate the EXACT 11 events from CI logs (IDs 918-928)
	// All are XID errors for worker2 with checkName="MultipleRemediations"
	events := []datastore.EventWithToken{
		// Event 918: XID 119
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "918"},
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id": "415d3b13-0ac3-4a52-a0ce-b6ac0e0468fd",
					"healthevent": map[string]interface{}{
						"checkName": "MultipleRemediations", // Doesn't match filter
						"errorCode": []interface{}{"119"},
						"nodeName":  "nvsentinel-worker2",
					},
				},
			},
			ResumeToken: []byte("918"),
		},
		// Event 919: XID 79
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "919"},
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id": "c9eee1f2-f52f-405e-b934-9b73b2b73fdb",
					"healthevent": map[string]interface{}{
						"checkName": "MultipleRemediations",
						"errorCode": []interface{}{"79"},
						"nodeName":  "nvsentinel-worker2",
					},
				},
			},
			ResumeToken: []byte("919"),
		},
		// Events 920-928 (abbreviated for brevity, but same pattern)
		{Event: buildTestHealthEvent("920"), ResumeToken: []byte("920")},
		{Event: buildTestHealthEvent("921"), ResumeToken: []byte("921")},
		{Event: buildTestHealthEvent("922"), ResumeToken: []byte("922")},
		{Event: buildTestHealthEvent("923"), ResumeToken: []byte("923")},
		{Event: buildTestHealthEvent("924"), ResumeToken: []byte("924")},
		{Event: buildTestHealthEvent("925"), ResumeToken: []byte("925")},
		{Event: buildTestHealthEvent("926"), ResumeToken: []byte("926")},
		{Event: buildTestHealthEvent("927"), ResumeToken: []byte("927")},
		{Event: buildTestHealthEvent("928"), ResumeToken: []byte("928")},
	}

	// BEFORE FIX: This would cause lastEventID to stay at 917
	// Next poll would re-fetch events 918-928 again (infinite loop)
	// CI logs showed 377+ second delays (6+ minutes)

	// AFTER FIX: lastEventID advances through all events
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Verify NO events sent (all filtered)
	select {
	case <-watcher.events:
		t.Fatal("All events should be filtered")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	// 🔥 THE FIX: lastEventID MUST be 928 (not 917)
	assert.Equal(t, int64(928), watcher.lastEventID,
		"lastEventID must advance to 928 to prevent re-fetching filtered events")

	// Simulate next poll cycle (after 500ms)
	// Query should use WHERE (changed_at > $2 OR (changed_at = $2 AND id > $3))
	// This proves we won't re-fetch the same 11 events

	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs(tableName, sqlmock.AnyArg(), int64(928)). // ✅ Uses timestamp-based resume
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}))
	// Empty result - no new events

	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	// This proves the fix: next poll skipped events 918-928
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Helper function to build test health events
func buildTestHealthEvent(id string) map[string]interface{} {
	return map[string]interface{}{
		"_id":           map[string]interface{}{"_data": id},
		"operationType": "insert",
		"fullDocument": map[string]interface{}{
			"id": uuid.New().String(),
			"healthevent": map[string]interface{}{
				"checkName": "MultipleRemediations",
				"errorCode": []interface{}{"999"},
				"nodeName":  "nvsentinel-worker2",
			},
		},
	}
}

// TestPositionAdvancesBeforeChannelSend verifies that lastEventID is updated
// BEFORE attempting to send to the channel. This ensures that if the send blocks
// (e.g., channel full), the position is still advanced when send eventually succeeds.
func TestPositionAdvancesBeforeChannelSend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "test-client"
	tableName := "test_table"

	// Mock loadResumePosition
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 0, "timestamp": "2025-01-01T00:00:00Z"}`))

	watcher := &PostgreSQLChangeStreamWatcher{
		db:          db,
		clientName:  clientName,
		tableName:   tableName,
		events:      make(chan datastore.EventWithToken, 1), // Buffer of 1
		stopCh:      make(chan struct{}),
		lastEventID: 0,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Create 2 events
	events := []datastore.EventWithToken{
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "1"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"id": "1"},
			},
			ResumeToken: []byte("1"),
		},
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "2"},
				"operationType": "insert",
				"fullDocument":  map[string]interface{}{"id": "2"},
			},
			ResumeToken: []byte("2"),
		},
	}

	// Send both events in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- watcher.sendEventsToChannel(ctx, events)
	}()

	// Give sendEventsToChannel time to process first event and block on second
	time.Sleep(50 * time.Millisecond)

	// Check lastEventID BEFORE draining the channel
	// It should be 2 (both events processed) even though channel still has events
	watcher.mu.RLock()
	currentLastEventID := watcher.lastEventID
	watcher.mu.RUnlock()

	// lastEventID should be 2 (last event processed)
	// This proves position advances BEFORE channel send completes
	assert.Equal(t, int64(2), currentLastEventID,
		"lastEventID should be 2 even before events are consumed from channel")

	// Now drain the channel
	<-watcher.events
	<-watcher.events

	// Wait for goroutine to complete
	err = <-done
	require.NoError(t, err)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestRepeatedXIDRuleCIScenario recreates the EXACT scenario from CI run 19611652848
// where TestRepeatedXIDRule failed due to the filtering deadlock bug.
//
// CI SCENARIO:
// - Test: TestRepeatedXIDRule listens for checkName="RepeatedXidError"
// - Meanwhile: TestMultipleRemediationsNotTriggered injects events with checkName="MultipleRemediations"
// - health-events-analyzer's pipeline filter only matches "RepeatedXidError" events
// - Result: All 11 "MultipleRemediations" events (IDs 918-928) were filtered
// - BEFORE FIX: lastEventID stuck at 917 → infinite re-fetch → 6+ min delays → test timeout
// - AFTER FIX: lastEventID advances to 928 → position moves forward → test passes
func TestRepeatedXIDRuleCIScenario(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	clientName := "health-events-analyzer"
	tableName := "health_events"

	// Start from lastEventID=917 (from CI logs: 14:33:48.046 events were inserted)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs(clientName).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(`{"eventID": 917, "timestamp": "2025-11-23T14:33:48Z"}`))

	// TestRepeatedXIDRule sets up a pipeline filter to ONLY listen for RepeatedXidError events
	// This mimics the real aggregation pipeline used in the E2E test
	pipelineFilter, err := NewPipelineFilter([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.checkName": "RepeatedXidError",
			},
		},
	})
	require.NoError(t, err)

	watcher := &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		events:         make(chan datastore.EventWithToken, 100),
		stopCh:         make(chan struct{}),
		lastEventID:    917,
		pipelineFilter: pipelineFilter,
		pollInterval:   500 * time.Millisecond,
	}

	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(917), watcher.lastEventID, "Should start at 917 from resume token")

	// Recreate the EXACT 11 events from CI logs (14:33:48.046 - 14:33:48.066)
	// These are XID errors injected by TestMultipleRemediationsNotTriggered
	// with checkName="MultipleRemediations" (NOT "RepeatedXidError")
	events := []datastore.EventWithToken{
		// Event 918: XID 119 on GPU-22222222 at 14:33:48.046224Z
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "918"},
				"clusterTime":   "2025-11-23T14:33:48.046224Z",
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id": "415d3b13-0ac3-4a52-a0ce-b6ac0e0468fd",
					"healthevent": map[string]interface{}{
						"checkName":         "MultipleRemediations", // ← NOT "RepeatedXidError"
						"nodeName":          "nvsentinel-worker2",
						"componentClass":    "GPU",
						"errorCode":         []interface{}{"119"},
						"isFatal":           true,
						"message":           "kernel: NVRM: Xid (PCI:0002:00:00): 119",
						"recommendedAction": 5,
						"entitiesImpacted": []interface{}{
							map[string]interface{}{"entityType": "PCI", "entityValue": "0002:00:00"},
							map[string]interface{}{"entityType": "GPU_UUID", "entityValue": "GPU-22222222-2222-2222-2222-222222222222"},
						},
					},
				},
			},
			ResumeToken: []byte("918"),
		},
		// Event 919: XID 79 on GPU-11111111 at 14:33:48.048263Z
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "919"},
				"clusterTime":   "2025-11-23T14:33:48.048263Z",
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id": "c9eee1f2-f52f-405e-b934-9b73b2b73fdb",
					"healthevent": map[string]interface{}{
						"checkName":         "MultipleRemediations",
						"nodeName":          "nvsentinel-worker2",
						"componentClass":    "GPU",
						"errorCode":         []interface{}{"79"},
						"isFatal":           true,
						"message":           "kernel: NVRM: Xid (PCI:0001:00:00): 79, GPU has fallen off the bus.",
						"recommendedAction": 5,
					},
				},
			},
			ResumeToken: []byte("919"),
		},
		// Event 920: XID 94 on GPU-00000000 at 14:33:48.050441Z
		{
			Event: map[string]interface{}{
				"_id":           map[string]interface{}{"_data": "920"},
				"clusterTime":   "2025-11-23T14:33:48.050441Z",
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id": "5fd0f4a2-ea1b-4998-9eb3-74e6144b979a",
					"healthevent": map[string]interface{}{
						"checkName":         "MultipleRemediations",
						"nodeName":          "nvsentinel-worker2",
						"errorCode":         []interface{}{"94"},
						"isFatal":           true,
						"message":           "kernel: NVRM: Xid (PCI:0000:17:00): 94, Contained ECC error.",
						"recommendedAction": 5,
					},
				},
			},
			ResumeToken: []byte("920"),
		},
		// Events 921-928: Similar XID errors (13, 31, 62, 45, 45, 45, 45, 119)
		{Event: buildCIHealthEvent("921", "13"), ResumeToken: []byte("921")},
		{Event: buildCIHealthEvent("922", "31"), ResumeToken: []byte("922")},
		{Event: buildCIHealthEvent("923", "62"), ResumeToken: []byte("923")},
		{Event: buildCIHealthEvent("924", "45"), ResumeToken: []byte("924")},
		{Event: buildCIHealthEvent("925", "45"), ResumeToken: []byte("925")},
		{Event: buildCIHealthEvent("926", "45"), ResumeToken: []byte("926")},
		{Event: buildCIHealthEvent("927", "45"), ResumeToken: []byte("927")},
		{Event: buildCIHealthEvent("928", "119"), ResumeToken: []byte("928")},
	}

	t.Log("📊 CI SCENARIO: TestRepeatedXIDRule running, waiting for 'RepeatedXidError' events")
	t.Log("📊 Meanwhile: 11 'MultipleRemediations' events inserted (IDs 918-928)")
	t.Log("📊 Pipeline filter rejects all 11 events (wrong checkName)")

	// Send all 11 events through the pipeline filter
	err = watcher.sendEventsToChannel(ctx, events)
	require.NoError(t, err)

	// Verify NO events were sent to channel (all filtered)
	select {
	case <-watcher.events:
		t.Fatal("❌ All events should be filtered (wrong checkName)")
	case <-time.After(100 * time.Millisecond):
		t.Log("✅ All 11 events correctly filtered (checkName mismatch)")
	}

	// 🔥 THE CRITICAL ASSERTION 🔥
	// BEFORE FIX (CI run 19611652848):
	//   - lastEventID stuck at 917
	//   - Next poll: SELECT WHERE id > 917 → fetches 918-928 again
	//   - Logs: "latencyMs":376990 (6.3 minutes of re-fetching!)
	//   - Test timeout after 642 seconds
	//
	// AFTER FIX (current):
	//   - lastEventID = 928
	//   - Next poll: SELECT WHERE id > 928 → fetches new events (929+)
	//   - No infinite loop, test completes quickly
	assert.Equal(t, int64(928), watcher.lastEventID,
		"❌ BUG DETECTED: lastEventID must be 928 to prevent CI timeout!\n"+
			"BEFORE FIX: lastEventID=917 → infinite re-fetch → 6min+ delays\n"+
			"AFTER FIX: lastEventID=928 → position advances → test passes")

	t.Log("✅ Position advanced correctly: 917 → 928")
	t.Log("✅ Next poll will query: WHERE id > 928 (not 917)")
	t.Log("✅ CI timeout bug PREVENTED!")

	// Simulate the next poll cycle to prove we skip the filtered events
	// Note: Now uses timestamp-based resume (3 args: tableName, timestamp, eventID)
	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs(tableName, sqlmock.AnyArg(), int64(928)). // ✅ Uses 928, NOT 917
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}))

	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	t.Log("✅ Next poll correctly used WHERE id > 928")
	t.Log("✅ This test recreates and PREVENTS the exact CI failure from run 19611652848")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// Helper function to build CI health events matching the exact structure from logs
func buildCIHealthEvent(id, errorCode string) map[string]interface{} {
	return map[string]interface{}{
		"_id":           map[string]interface{}{"_data": id},
		"clusterTime":   fmt.Sprintf("2025-11-23T14:33:48.%sZ", id), // Approximate timestamp
		"operationType": "insert",
		"fullDocument": map[string]interface{}{
			"id": uuid.New().String(),
			"healthevent": map[string]interface{}{
				"checkName":         "MultipleRemediations", // NOT "RepeatedXidError"
				"nodeName":          "nvsentinel-worker2",
				"componentClass":    "GPU",
				"errorCode":         []interface{}{errorCode},
				"isFatal":           true,
				"message":           fmt.Sprintf("kernel: NVRM: Xid error %s", errorCode),
				"recommendedAction": 5,
			},
		},
	}
}
