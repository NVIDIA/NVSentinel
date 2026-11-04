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

package postgresql

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// startTestListenLoop runs listenLoop on a LISTEN connection the test feeds, until the test
// ends. The listener has no connection behind it, so nothing may close or ping it.
func startTestListenLoop(t *testing.T, watcher *PostgreSQLChangeStreamWatcher) chan<- *pq.Notification {
	t.Helper()

	listener := &pq.Listener{Notify: make(chan *pq.Notification, 32)}
	watcher.listener = listener

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		watcher.listenLoop(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return listener.Notify
}

// testNotification is the NOTIFY the changelog trigger sends for an insert into health_events.
func testNotification(id int64) *pq.Notification {
	return &pq.Notification{
		Channel: notifyChannel,
		Extra:   fmt.Sprintf(`{"id":%d,"table":"health_events","operation":"INSERT"}`, id),
	}
}

// listenTestRows returns changelog rows for health_events, one second apart from base by id.
func listenTestRows(base time.Time, ids ...int64) *sqlmock.Rows {
	rows := changelogRows()

	for _, id := range ids {
		rows.AddRow(id, fmt.Sprintf("record-%d", id), "INSERT", nil, `{"document":{}}`,
			base.Add(time.Duration(id)*time.Second))
	}

	return rows
}

// A row whose transaction commits after the read position has passed it is never returned by a
// poll, so only its NOTIFY can deliver it (#1466, #1469). Through the LISTEN loop, the late row
// is delivered once and does not move the position back, and a NOTIFY for a row that was just
// read is not fetched again.
func TestListenLoop_LateNotifiedRow_IsDeliveredOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	watcher.recordRecentEventIDLocked(20, time.Now())

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(15)).
		WillReturnRows(listenTestRows(base, 15))

	// Any poll from the read position finds nothing new.
	for range 5 {
		mock.ExpectQuery(changelogQuery).
			WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
			WillReturnRows(changelogRows())
	}

	notify := startTestListenLoop(t, watcher)
	notify <- testNotification(20)
	notify <- testNotification(15)

	select {
	case event := <-watcher.events:
		assert.Equal(t, []byte("15"), event.ResumeToken)
	case <-time.After(5 * time.Second):
		t.Fatal("expected the late row to be delivered")
	}

	assert.Never(t, func() bool { return len(watcher.events) > 0 }, 500*time.Millisecond, 10*time.Millisecond,
		"nothing else may be delivered")

	watcher.mu.RLock()
	defer watcher.mu.RUnlock()

	assert.Equal(t, base.Add(20*time.Second), watcher.lastTimestamp, "the late row must not move the position back")
	assert.Equal(t, int64(20), watcher.lastEventID)
}

// A consumer that stops taking events must not stop the LISTEN connection from being read. When
// the loop stopped reading, lib/pq's buffers filled and PostgreSQL blocked in ClientWrite while
// delivering notifications, holding a snapshot open for as long as the consumer stayed behind
// (#2171).
func TestListenLoop_StalledConsumer_KeepsReadingNotifications(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base

	// The consumer took nothing, so the next delivery blocks.
	watcher.events = make(chan datastore.EventWithToken, 1)
	watcher.events <- datastore.EventWithToken{ResumeToken: []byte("0")}

	// Whatever reads the changelog first gets row 1 and then waits to deliver it.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(1)).
		WillReturnRows(listenTestRows(base, 1))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(listenTestRows(base, 1))

	notify := startTestListenLoop(t, watcher)

	// Far more notifications than the connection's buffers hold.
	sent := make(chan struct{})

	go func() {
		defer close(sent)

		for id := int64(1); id <= 1000; id++ {
			notify <- testNotification(id)
		}
	}()

	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the LISTEN loop stopped reading notifications while the consumer was stalled")
	}

	assert.Eventually(t, func() bool { return len(notify) == 0 }, 5*time.Second, 10*time.Millisecond,
		"every notification must be read")
}

func TestHandleNotification_RecordsIDForFetchLoop(t *testing.T) {
	tests := []struct {
		name        string
		extra       string
		full        bool
		wantIDs     []int64
		wantDropped int
		wantWake    bool
	}{
		{
			name:     "new row is recorded and wakes the fetch loop",
			extra:    `{"id":21,"table":"health_events","operation":"INSERT"}`,
			wantIDs:  []int64{21},
			wantWake: true,
		},
		{
			name:  "row read recently is skipped",
			extra: `{"id":20,"table":"health_events","operation":"INSERT"}`,
		},
		{
			name:  "row of another table is ignored",
			extra: `{"id":22,"table":"maintenance_events","operation":"INSERT"}`,
		},
		{
			name:        "full list drops the id and counts it",
			extra:       `{"id":23,"table":"health_events","operation":"INSERT"}`,
			full:        true,
			wantDropped: 1,
			wantWake:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
			watcher.lastEventID = 20
			watcher.recordRecentEventIDLocked(20, time.Now())

			if tt.full {
				for id := int64(1000); len(watcher.notifiedIDs) < notifiedIDLimit; id++ {
					watcher.addNotifiedID(id)
				}

				<-watcher.wake
			}

			require.NoError(t, watcher.handleNotification(context.Background(), &pq.Notification{Extra: tt.extra}))

			ids, dropped := watcher.takeNotifiedIDs()
			if tt.full {
				ids = ids[notifiedIDLimit:]
			}

			assert.Equal(t, tt.wantIDs, nilIfEmpty(ids))
			assert.Equal(t, tt.wantDropped, dropped)
			assert.Equal(t, tt.wantWake, len(watcher.wake) == 1)
			assert.NoError(t, mock.ExpectationsWereMet(), "the LISTEN loop must not query the database")
		})
	}
}

func nilIfEmpty(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}

	return ids
}

// After ids were dropped, a pass must not move the read position forward to a notified row: it
// could step over a dropped row that no poll would return then. It delivers the rows at or behind
// the position, and the poll reads the rest in order.
func TestFetchNotifiedRows_AfterDroppedIDs_DeliversOnlyRowsBehindPosition(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20

	watcher.addNotifiedID(15)
	watcher.addNotifiedID(30)
	watcher.droppedIDs = 1

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(15)).
		WillReturnRows(listenTestRows(base, 15))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(30)).
		WillReturnRows(listenTestRows(base, 30))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 30))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(30*time.Second), int64(30)).
		WillReturnRows(changelogRows())

	require.NoError(t, watcher.fetchNotifiedRows(context.Background()))

	assert.Equal(t, []byte("15"), (<-watcher.events).ResumeToken, "the late row comes from its lookup")
	assert.Equal(t, []byte("30"), (<-watcher.events).ResumeToken, "the row past the position comes from the poll")
	assert.Empty(t, watcher.events)
	assert.Equal(t, base.Add(30*time.Second), watcher.lastTimestamp)
	assert.Equal(t, int64(30), watcher.lastEventID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Two reads from the same position deliver the same rows twice. The fallback poller and the
// fetch loop both read the changelog, so a read that starts while another one is delivering has
// to wait for it and then read from where it stopped.
func TestFetchNewChanges_OverlappingReads_DeliverEachRowOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	ctx := context.Background()
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base
	// Unbuffered, so each delivery waits for the test to take it.
	watcher.events = make(chan datastore.EventWithToken)

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base, int64(0)).
		WillReturnRows(listenTestRows(base, 1, 2, 3))
	// A second read that starts while the first one is delivering row 1 reads from row 1.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(time.Second), int64(1)).
		WillReturnRows(listenTestRows(base, 2, 3))
	// A second read that waits for the first one reads from row 3.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(3*time.Second), int64(3)).
		WillReturnRows(changelogRows())

	done := make(chan error, 2)

	go func() { done <- watcher.fetchNewChanges(ctx) }()

	require.Eventually(t, func() bool {
		watcher.mu.RLock()
		defer watcher.mu.RUnlock()

		return watcher.lastEventID == 1
	}, 5*time.Second, time.Millisecond, "the first read must reach row 1")

	go func() { done <- watcher.fetchNewChanges(ctx) }()

	require.Never(t, func() bool {
		watcher.mu.RLock()
		defer watcher.mu.RUnlock()

		return watcher.lastEventID != 1
	}, 300*time.Millisecond, time.Millisecond, "a read must wait while another one is delivering")

	var tokens []string

	for finished := 0; finished < 2; {
		select {
		case event := <-watcher.events:
			tokens = append(tokens, string(event.ResumeToken))
		case err := <-done:
			require.NoError(t, err)

			finished++
		case <-time.After(5 * time.Second):
			t.Fatalf("reads did not finish, delivered %v", tokens)
		}
	}

	assert.Equal(t, []string{"1", "2", "3"}, tokens)
}

// rowRange returns the ids from first to last.
func rowRange(first, last int64) []int64 {
	ids := make([]int64, 0, last-first+1)

	for id := first; id <= last; id++ {
		ids = append(ids, id)
	}

	return ids
}

// takeTokens takes n events from the watcher and returns their tokens, or fails after timeout.
func takeTokens(t *testing.T, watcher *PostgreSQLChangeStreamWatcher, n int, timeout time.Duration) []string {
	t.Helper()

	tokens := make([]string, 0, n)
	deadline := time.After(timeout)

	for len(tokens) < n {
		select {
		case event := <-watcher.events:
			tokens = append(tokens, string(event.ResumeToken))
		case <-deadline:
			t.Fatalf("got %d of %d events", len(tokens), n)
		}
	}

	return tokens
}

func tokensFor(ids []int64) []string {
	tokens := make([]string, 0, len(ids))

	for _, id := range ids {
		tokens = append(tokens, fmt.Sprint(id))
	}

	return tokens
}

// Rows committed before LISTEN was acknowledged are announced by no notification. They are read
// as soon as the LISTEN loop starts, and a full batch is followed by the next poll right away
// instead of the next fallback tick (#2171).
func TestListenLoop_Startup_ReadsBacklogWithoutWaiting(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base, int64(0)).
		WillReturnRows(listenTestRows(base, rowRange(1, 100)...))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(100*time.Second), int64(100)).
		WillReturnRows(listenTestRows(base, rowRange(101, 150)...))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(150*time.Second), int64(150)).
		WillReturnRows(changelogRows())

	startTestListenLoop(t, watcher)

	assert.Equal(t, tokensFor(rowRange(1, 150)), takeTokens(t, watcher, 150, 5*time.Second))
	assert.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 5*time.Second, 10*time.Millisecond)
}

// A poll that reads a full batch, such as the fallback poller's, wakes the fetch loop, which reads
// the rest. A short batch does not, and neither does a full batch that left the read position
// where it was: it would only come back, and waking on it would redeliver it in a loop.
func TestFetchNewChanges_FullBatch_WakesFetchLoop(t *testing.T) {
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		position int64
		rows     []int64
		wantWake bool
	}{
		{name: "full batch", rows: rowRange(1, changelogBatchSize), wantWake: true},
		{name: "short batch", rows: rowRange(1, changelogBatchSize-1)},
		{name: "full batch that does not move the position", position: 200, rows: rowRange(1, changelogBatchSize)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
			watcher.lastTimestamp = base.Add(time.Duration(tt.position) * time.Second)
			watcher.lastEventID = tt.position
			watcher.events = make(chan datastore.EventWithToken, changelogBatchSize)

			mock.ExpectQuery(changelogQuery).
				WithArgs(healthEventsTable, watcher.lastTimestamp, tt.position).
				WillReturnRows(listenTestRows(base, tt.rows...))

			require.NoError(t, watcher.fetchNewChanges(context.Background()))
			assert.Equal(t, tt.wantWake, len(watcher.wake) == 1)
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// A notification for a new row must not move the read position past older rows that nothing has
// read yet, such as a backlog the watcher restarted behind. The rows are read in order first.
func TestListenLoop_NotifiedRowPastBacklog_DoesNotSkipBacklog(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base, int64(0)).
		WillReturnRows(listenTestRows(base, rowRange(1, 100)...))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(100*time.Second), int64(100)).
		WillReturnRows(listenTestRows(base, rowRange(101, 151)...))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(151*time.Second), int64(151)).
		WillReturnRows(changelogRows())
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(151)).
		WillReturnRows(listenTestRows(base, 151))

	listener := &pq.Listener{Notify: make(chan *pq.Notification, 32)}
	listener.Notify <- testNotification(151)
	watcher.listener = listener

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		watcher.listenLoop(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	assert.Equal(t, tokensFor(rowRange(1, 151)), takeTokens(t, watcher, 151, 5*time.Second))
	assert.Never(t, func() bool { return len(watcher.events) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"no row may be delivered twice")
}

// A late row behind the read position is only delivered by the lookup of its notified id. When the
// pass that would look it up fails, nothing may wake the fetch loop again on a quiet table, so the
// loop makes another pass after retryDelay instead of waiting for an unrelated notification.
func TestFetchLoop_FailedPass_RetriesWithoutNotification(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	watcher.retryDelay = 10 * time.Millisecond

	// Row 15 committed after the position passed it. Its notification woke the loop for the
	// pass that fails, and no other notification comes.
	watcher.addNotifiedID(15)
	<-watcher.wake

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnError(errors.New("connection reset by peer"))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, int64(15)).
		WillReturnRows(listenTestRows(base, 15))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())

	stopFetchLoop := watcher.startFetchLoop(context.Background())
	defer stopFetchLoop()

	select {
	case event := <-watcher.events:
		assert.Equal(t, []byte("15"), event.ResumeToken)
	case <-time.After(5 * time.Second):
		t.Fatal("the late row must be delivered by another pass after the failed one")
	}

	assert.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, watcher.events, "nothing else may be delivered")
}

// Once the watcher is closed or its context is cancelled, the fetch loop returns at once and makes
// no other pass, even with a retry pending after a failed one.
func TestFetchLoop_FailedPass_DoesNotRetryAfterStop(t *testing.T) {
	tests := []struct {
		name         string
		closeWatcher bool
	}{
		{name: "Close", closeWatcher: true},
		{name: "context cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
			watcher.lastTimestamp = base.Add(20 * time.Second)
			watcher.lastEventID = 20
			watcher.retryDelay = 50 * time.Millisecond

			watcher.addNotifiedID(15)
			<-watcher.wake

			mock.ExpectQuery(changelogQuery).
				WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
				WillReturnError(errors.New("connection reset by peer"))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})

			go func() {
				defer close(done)

				watcher.fetchLoop(ctx)
			}()

			require.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 5*time.Second, time.Millisecond,
				"the first pass must fail")

			if tt.closeWatcher {
				require.NoError(t, watcher.Close(context.Background()))
			} else {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the fetch loop must return instead of waiting to retry")
			}

			ids, _ := watcher.takeNotifiedIDs()
			assert.Equal(t, []int64{15}, ids, "no pass may look up the notified row after the stop")
		})
	}
}
