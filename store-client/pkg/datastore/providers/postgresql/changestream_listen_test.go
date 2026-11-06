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
	"sync"
	"sync/atomic"
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

// changelogLookupQuery matches the lookup of notified rows: one query for all of them that reads
// only rows at or behind the read position, in the order the poll reads them.
const changelogLookupQuery = `WHERE table_name = \$1 AND id = ANY\(\$2\) AND \(changed_at, id\) <= \(\$3, \$4\) ` +
	`ORDER BY changed_at ASC, id ASC`

// lookupIDs is the id argument of a lookup of the given notified ids, in the order they arrived.
func lookupIDs(ids ...int64) any {
	return pq.Array(ids)
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

	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15), base.Add(20*time.Second), int64(20)).
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

	// The fetch loop reads the changelog with a poll first, gets row 1 and then waits to
	// deliver it.
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
		readID      int64
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
			// The poll read the row before its NOTIFY arrived, so the position already covers
			// it. Looking it up would deliver it twice, and there is nothing to wake for.
			name:   "row past the position that a poll read is skipped",
			extra:  `{"id":21,"table":"health_events","operation":"INSERT"}`,
			readID: 21,
		},
		{
			name:  "row of another table is ignored",
			extra: `{"id":22,"table":"maintenance_events","operation":"INSERT"}`,
		},
		{
			// The ids already waiting woke the fetch loop when they were recorded, so the
			// pass that takes them is due without another wake.
			name:        "full list drops the id and counts it",
			extra:       `{"id":23,"table":"health_events","operation":"INSERT"}`,
			full:        true,
			wantDropped: 1,
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

			if tt.readID != 0 {
				watcher.recordRecentEventIDLocked(tt.readID, time.Now())
			}

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

// A lookup delivers a notified row only when it is at or behind the read position. Moving the
// position forward to a notified row would step over a row that committed meanwhile and that no
// read has returned, and if that row's id were dropped, nothing would deliver it. The poll reads
// the rows past the position in order.
func TestFetchNotifiedRows_RowPastPosition_IsLeftToThePoll(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20

	// Row 15 committed after the position passed it. Rows 21 and 30 committed after the last
	// poll, and only the NOTIFY for row 30 has been handled. No id was dropped.
	watcher.addNotifiedID(15)
	watcher.addNotifiedID(30)

	// The lookup asks only for rows at or behind the position, so PostgreSQL returns row 15 and
	// not row 30.
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15, 30), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 21, 30))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(30*time.Second), int64(30)).
		WillReturnRows(changelogRows())

	require.NoError(t, watcher.fetchNotifiedRows(context.Background()))

	assert.Equal(t, []string{"15", "21", "30"}, drainTokens(watcher),
		"the late row comes from its lookup, the rows past the position from the poll in order")
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
	// Row 151 is past the read position until the polls reach it, so its lookup finds nothing.
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(151), base.Add(100*time.Second), int64(100)).
		WillReturnRows(changelogRows())

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
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15), base.Add(20*time.Second), int64(20)).
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

// A late row is only ever delivered by the lookup of its notified id. When that lookup fails,
// the id has to stay pending so the pass that runs again after the failure delivers it.
func TestFetchLoop_FailedLookup_DeliversTheRowOnTheRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	watcher.retryDelay = 10 * time.Millisecond

	// Rows 15 and 16 committed after the position passed them. Only their lookups deliver them.
	watcher.addNotifiedID(15)
	watcher.addNotifiedID(16)
	<-watcher.wake

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15, 16), base.Add(20*time.Second), int64(20)).
		WillReturnError(errors.New("connection reset by peer"))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15, 16), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15, 16))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())

	stopFetchLoop := watcher.startFetchLoop(context.Background())
	defer stopFetchLoop()

	assert.Equal(t, tokensFor([]int64{15, 16}), takeTokens(t, watcher, 2, 5*time.Second),
		"both late rows must be delivered by the pass after the failed one, in order")
	assert.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, watcher.events, "nothing else may be delivered")
}

// A failed lookup keeps its ids pending, in front of ids notified since, lists an id notified
// again once, and keeps the dropped count.
func TestFetchNotifiedRows_FailedLookup_KeepsItsIDsPending(t *testing.T) {
	hold := newHoldFirstQuery()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(hold))
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20

	watcher.addNotifiedID(15)
	watcher.addNotifiedID(16)
	watcher.addNotifiedID(17)
	watcher.droppedIDs = 2

	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15, 16, 17), base.Add(20*time.Second), int64(20)).
		WillReturnError(errors.New("connection reset by peer"))

	done := make(chan error, 1)

	go func() { done <- watcher.fetchNotifiedRows(context.Background()) }()

	// While the lookup runs, row 16 is notified a second time and row 18 for the first time.
	<-hold.started
	watcher.addNotifiedID(16)
	watcher.addNotifiedID(18)
	close(hold.release)

	err = <-done
	require.Error(t, err)
	assert.ErrorContains(t, err, "3 notified changelog rows")
	assert.ErrorContains(t, err, "connection reset by peer")

	ids, dropped := watcher.takeNotifiedIDs()
	assert.Equal(t, []int64{15, 17, 16, 18}, ids,
		"the failed lookup's ids go in front, row 16 is already listed again, then the new one")
	assert.Equal(t, 2, dropped, "the dropped count is kept for the retry")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A pass looks every notified row up in one query, not one query per id, and delivers what it
// returns in the order the poll reads the rows, whatever order the notifications came in.
func TestFetchNotifiedRows_NotifiedIDs_AreLookedUpInOneQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20

	// Rows 15, 16 and 17 committed after the position passed them, and their notifications came
	// in another order.
	for _, id := range []int64{17, 15, 16} {
		watcher.addNotifiedID(id)
	}

	// sqlmock fails any query it is not told to expect, such as a second lookup.
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(17, 15, 16), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15, 16, 17))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(changelogRows())

	require.NoError(t, watcher.fetchNotifiedRows(context.Background()))

	assert.Equal(t, []string{"15", "16", "17"}, drainTokens(watcher))
	assert.Equal(t, base.Add(20*time.Second), watcher.lastTimestamp, "late rows must not move the position back")
	assert.Equal(t, int64(20), watcher.lastEventID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Close can land while a lookup runs. That lookup is the last query of the pass: nothing is looked
// up after Close, so no row a later lookup returns can be sent on the closed events channel.
func TestFetchNotifiedRows_ClosedDuringLookup_StartsNoOtherQuery(t *testing.T) {
	hold := newHoldFirstQuery()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(hold))
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	// The lookup that was running delivers its rows after Close. No event matches the filter, so
	// none is sent on the closed channel.
	watcher.pipelineFilter = rejectAllFilter()

	for _, id := range []int64{15, 16, 17} {
		watcher.addNotifiedID(id)
	}

	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15, 16, 17), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15, 16, 17))

	done := make(chan error, 1)

	go func() { done <- watcher.fetchNotifiedRows(context.Background()) }()

	<-hold.started
	require.NoError(t, watcher.Close(context.Background()))
	hold.closed.Store(true)
	close(hold.release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup must return after Close")
	}

	assert.False(t, hold.queriedAfterClose.Load(), "no query may start after Close")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// The pending list stays bounded when a failed pass puts ids back. The ids that do not fit are
// counted as dropped along with the pass's own count, and leave the unread set.
func TestKeepNotifiedIDs_OverTheLimit_CutsAndCountsTheCutAsDropped(t *testing.T) {
	watcher := NewPostgreSQLChangeStreamWatcher(nil, "test-client", healthEventsTable, "", ModeHybrid)

	for id := int64(1); id <= notifiedIDLimit; id++ {
		watcher.addNotifiedID(id)
	}

	ids, dropped := watcher.takeNotifiedIDs()
	require.Len(t, ids, notifiedIDLimit)
	require.Zero(t, dropped)

	// Ten more ids are notified during the pass, and the pass fails at its third id.
	for id := int64(notifiedIDLimit + 1); id <= notifiedIDLimit+10; id++ {
		watcher.addNotifiedID(id)
	}

	watcher.keepNotifiedIDs(ids[2:], 4)

	want := append(append([]int64{}, ids[2:]...), notifiedIDLimit+1, notifiedIDLimit+2)

	watcher.mu.Lock()
	assert.Len(t, watcher.unreadIDs, notifiedIDLimit)

	for _, id := range want {
		_, unread := watcher.unreadIDs[id]
		assert.True(t, unread, "id %d must stay unread", id)
	}

	watcher.mu.Unlock()

	got, gotDropped := watcher.takeNotifiedIDs()
	assert.Equal(t, want, got, "the oldest ids are kept, the newest eight are cut")
	assert.Equal(t, 4+8, gotDropped, "the pass's dropped count plus the cut")
}

// rejectAllFilter is a pipeline filter no event matches. Delivery runs the position update before
// the filter, so a lookup that wrongly runs still shows in the watcher's state and nothing is sent.
func rejectAllFilter() *PipelineFilter {
	return &PipelineFilter{stages: []filterStage{{matchConditions: map[string]any{"operationType": "none"}}}}
}

// Close closes the events channel without waiting for the fetch loop. A pass that starts after
// Close must look nothing up and deliver nothing, the same as fetchNewChanges already does.
func TestFetchNotifiedRows_AfterClose_LooksNothingUp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	watcher.pipelineFilter = rejectAllFilter()

	watcher.addNotifiedID(15)
	watcher.droppedIDs = 1

	// The row is there so that a lookup that wrongly runs is seen: it would take the id off the
	// pending list.
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15))

	require.NoError(t, watcher.Close(context.Background()))
	require.NoError(t, watcher.fetchNotifiedRows(context.Background()))

	ids, dropped := watcher.takeNotifiedIDs()
	assert.Equal(t, []int64{15}, ids, "nothing may be looked up after Close")
	assert.Equal(t, 1, dropped)
}

// holdFirstQuery matches queries the way sqlmock does by default and holds the first query it
// sees until the test releases it, so the test can act while that query is in flight. Once the
// test sets closed, it records any query that starts after that.
type holdFirstQuery struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	closed, queriedAfterClose atomic.Bool
}

func newHoldFirstQuery() *holdFirstQuery {
	return &holdFirstQuery{started: make(chan struct{}), release: make(chan struct{})}
}

func (h *holdFirstQuery) Match(expectedSQL, actualSQL string) error {
	if h.closed.Load() {
		h.queriedAfterClose.Store(true)
	}

	h.once.Do(func() {
		close(h.started)
		<-h.release
	})

	return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
}

// The watcher is closed while a failed pass's retry is pending. Whether the loop returns on the
// stop or runs one more pass first, that pass must look nothing up.
func TestFetchLoop_ClosedWhileRetryPending_LooksNothingUp(t *testing.T) {
	hold := newHoldFirstQuery()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(hold))
	require.NoError(t, err)
	defer db.Close()

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	watcher.retryDelay = 0
	watcher.pipelineFilter = rejectAllFilter()

	watcher.addNotifiedID(15)
	<-watcher.wake

	// The first poll is held until Close has happened, then fails, so the retry is ready at the
	// same time as the stop. The row is there so that a lookup that wrongly runs is seen.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnError(errors.New("connection reset by peer"))
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(15), base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 15))

	done := make(chan struct{})

	go func() {
		defer close(done)

		watcher.fetchLoop(context.Background())
	}()

	<-hold.started
	require.NoError(t, watcher.Close(context.Background()))
	close(hold.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch loop must return after Close")
	}

	ids, _ := watcher.takeNotifiedIDs()
	assert.Equal(t, []int64{15}, ids, "no pass may look up the notified row after Close")
}
