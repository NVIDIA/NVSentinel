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
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
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

// idsInclude matches the id argument of a lookup that asks for the given id.
type idsInclude int64

func (id idsInclude) Match(v driver.Value) bool {
	ids, ok := v.(string)
	if !ok {
		return false
	}

	for _, field := range strings.Split(strings.Trim(ids, "{}"), ",") {
		if field == strconv.FormatInt(int64(id), 10) {
			return true
		}
	}

	return false
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

// Ids do not follow changed_at: a writer that started first can get its changelog id last, so the
// poll reads row 151 before rows 1 to 3 and leaves the position at row 3. Through the LISTEN loop, a
// NOTIFY for a row the poll has read is not looked up, whatever its id. A late row whose id is
// above the position's but whose changed_at is behind it is looked up with the position the poll
// left, delivered once, and does not move the position back.
func TestListenLoop_RowsOutOfIDOrder_AreDeliveredOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base

	rows := healthEventRow(changelogRows(), 151, base.Add(500*time.Millisecond))

	for id := int64(1); id <= 3; id++ {
		healthEventRow(rows, id, base.Add(time.Duration(id)*time.Second))
	}

	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base, int64(0)).
		WillReturnRows(rows)

	// One poll ends the first pass and one the pass the late row's NOTIFY wakes.
	for range 2 {
		mock.ExpectQuery(changelogQuery).
			WithArgs(healthEventsTable, base.Add(3*time.Second), int64(3)).
			WillReturnRows(changelogRows())
	}

	// Row 160 started before row 3 and committed after the poll had passed it.
	const lateID = 160

	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, lookupIDs(lateID), base.Add(3*time.Second), int64(3)).
		WillReturnRows(healthEventRow(changelogRows(), lateID, base.Add(2500*time.Millisecond)))

	notify := startTestListenLoop(t, watcher)

	assert.Equal(t, []string{"151", "1", "2", "3"}, takeTokens(t, watcher, 4, 5*time.Second))

	// The NOTIFYs of the rows the poll read come after it, row 151's last, then the late row's.
	for _, id := range []int64{1, 2, 3, 151, lateID} {
		notify <- testNotification(id)
	}

	assert.Equal(t, tokensFor([]int64{lateID}), takeTokens(t, watcher, 1, 5*time.Second),
		"the late row must be delivered")
	assert.Never(t, func() bool { return len(watcher.events) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"no row may be delivered twice")
	assert.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 5*time.Second, 10*time.Millisecond)

	watcher.mu.RLock()
	defer watcher.mu.RUnlock()

	assert.Equal(t, base.Add(3*time.Second), watcher.lastTimestamp, "the late row must not move the position back")
	assert.Equal(t, int64(3), watcher.lastEventID)
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
// read has returned, and if that row's id were dropped, nothing would deliver it. The polls read
// the rows past the position in order.
func TestFetchNotifiedRows_RowPastPosition_IsLeftToThePoll(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
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

	require.NoError(t, watcher.fetchNotifiedRows(ctx))
	assert.Equal(t, []string{"15"}, drainTokens(watcher), "the late row comes from its lookup")
	assert.Equal(t, base.Add(20*time.Second), watcher.lastTimestamp, "the lookup must not move the position")
	assert.Equal(t, int64(20), watcher.lastEventID)

	// The next pass polls from the position and reads both rows in order.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, 21, 30))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(30*time.Second), int64(30)).
		WillReturnRows(changelogRows())

	require.NoError(t, watcher.readChangelog(ctx))
	assert.Equal(t, []string{"21", "30"}, drainTokens(watcher), "the rows past the position come from the poll in order")
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

// A late row is only delivered by the lookup of its notified id. During a long catch-up the polls
// keep moving the read position while notifications keep coming, and at most notifiedIDLimit ids
// wait between two lookups. When the ids were only looked up after the catch-up, the list filled up
// and a late row notified meanwhile was dropped for good. The pass looks the ids up after every
// poll, so the list empties after each one and the late row is delivered.
func TestListenLoop_LateRowNotifiedDuringLongCatchUp_IsDelivered(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.lastTimestamp = base.Add(20 * time.Second)
	watcher.lastEventID = 20
	// Unbuffered, so each poll waits in its delivery until the test takes the rows.
	watcher.events = make(chan datastore.EventWithToken)

	const lateID = 9001

	// The catch-up reads two full batches, then finds nothing more.
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(20*time.Second), int64(20)).
		WillReturnRows(listenTestRows(base, rowRange(21, 120)...))
	mock.ExpectQuery(changelogQuery).
		WithArgs(healthEventsTable, base.Add(120*time.Second), int64(120)).
		WillReturnRows(listenTestRows(base, rowRange(121, 220)...))

	for range 5 {
		mock.ExpectQuery(changelogQuery).
			WithArgs(healthEventsTable, base.Add(220*time.Second), int64(220)).
			WillReturnRows(changelogRows())
	}

	// The late row's changed_at is behind the position, so a lookup that asks for it finds it.
	// The other notified rows are past the position, and their lookups find nothing.
	mock.ExpectQuery(changelogLookupQuery).
		WithArgs(healthEventsTable, idsInclude(lateID), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(healthEventRow(changelogRows(), lateID, base.Add(15*time.Second)))

	for range 5 {
		mock.ExpectQuery(changelogLookupQuery).
			WithArgs(healthEventsTable, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnRows(changelogRows())
	}

	notify := startTestListenLoop(t, watcher)

	// notifyAll sends the notifications and waits until the LISTEN loop has handled them. The
	// NOTIFY for another table is ignored, and once it is off the channel the ones before it
	// have been handled.
	notifyAll := func(ids []int64) {
		for _, id := range ids {
			notify <- testNotification(id)
		}

		notify <- &pq.Notification{Extra: `{"id":1,"table":"maintenance_events","operation":"INSERT"}`}

		require.Eventually(t, func() bool { return len(notify) == 0 }, 5*time.Second, time.Millisecond)
	}

	// While the first poll delivers, 600 rows past the position are notified.
	notifyAll(rowRange(1001, 1600))
	assert.Equal(t, tokensFor(rowRange(21, 120)), takeTokens(t, watcher, 100, 5*time.Second))

	// The second poll delivers once the position is at its first row. Another 600 rows past the
	// position are notified and then the late row, 1201 ids since the catch-up started.
	require.Eventually(t, func() bool {
		watcher.mu.RLock()
		defer watcher.mu.RUnlock()

		return watcher.lastEventID == 121
	}, 5*time.Second, time.Millisecond)
	notifyAll(append(rowRange(1601, 2200), lateID))
	assert.Equal(t, tokensFor(rowRange(121, 220)), takeTokens(t, watcher, 100, 5*time.Second))

	assert.Equal(t, tokensFor([]int64{lateID}), takeTokens(t, watcher, 1, 5*time.Second),
		"the late row must be delivered")
	assert.Never(t, func() bool { return len(watcher.events) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"nothing else may be delivered")

	watcher.mu.RLock()
	defer watcher.mu.RUnlock()

	assert.Equal(t, base.Add(220*time.Second), watcher.lastTimestamp, "the late row must not move the position back")
	assert.Equal(t, int64(220), watcher.lastEventID)
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

// newestRowQuery matches the read of the newest changelog row a filtered watcher may move up to.
const newestRowQuery = `SELECT changed_at, id FROM datastore_changelog WHERE table_name = \$1 ` +
	`ORDER BY changed_at DESC, id DESC LIMIT 1`

// filteredPollArgs makes the watcher filter on the server side and returns its poll arguments from
// position (base+20s, 20).
func filteredPollArgs(watcher *PostgreSQLChangeStreamWatcher, base time.Time) []driver.Value {
	watcher.pipeline = client.NewPostgreSQLPipelineBuilder().BuildAllHealthEventInsertsPipeline()

	return []driver.Value{healthEventsTable, base.Add(20 * time.Second), int64(20), "INSERT"}
}

// expectNewestRow lets the watcher read id as the newest changelog row.
func expectNewestRow(mock sqlmock.Sqlmock, base time.Time, id int64) {
	mock.ExpectQuery(newestRowQuery).
		WithArgs(healthEventsTable).
		WillReturnRows(sqlmock.NewRows([]string{"changed_at", "id"}).AddRow(base.Add(time.Duration(id)*time.Second), id))
}

// A watcher whose server-side filter rejects most rows only moves its position on the rows a poll
// returns, so it would stay at its last match and every poll would scan every row since. A short
// batch of the fetch loop's poll moves it up to the newest row, read before the poll, while the
// LISTEN connection delivers notifications. Nothing else moves it: pollForChanges has no LISTEN
// connection and the fallback poller polls because NOTIFY has gone silent, so no lookup would
// deliver a row that commits behind the moved position. The newest row is offered to every
// filtered case, so a poll that reads it when it may not is seen.
func TestFetchNewChangesFor_ShortFilteredBatch_MovesToNewestRowOnlyForTheFetchLoop(t *testing.T) {
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		reader       changelogReader
		unfiltered   bool
		notifyAgo    time.Duration // since the last NOTIFY, none when zero
		dropped      int           // notified ids dropped since the last lookup
		fullList     bool          // notifiedIDLimit ids are waiting
		newest       int64
		rows         []int64
		wantHeadRead bool
		wantID       int64
	}{
		{name: "fetch loop, no row accepted", reader: fetchLoopReader, notifyAgo: time.Second, newest: 500,
			wantHeadRead: true, wantID: 500},
		{name: "fetch loop, short batch", reader: fetchLoopReader, notifyAgo: time.Second, newest: 500,
			rows: []int64{21, 22}, wantHeadRead: true, wantID: 500},
		{name: "fetch loop, full batch", reader: fetchLoopReader, notifyAgo: time.Second, newest: 500,
			rows: rowRange(21, 20+changelogBatchSize), wantHeadRead: true, wantID: 20 + changelogBatchSize},
		{name: "fetch loop, newest row behind the position", reader: fetchLoopReader, notifyAgo: time.Second,
			newest: 10, wantHeadRead: true, wantID: 20},
		{name: "fetch loop, no filter", reader: fetchLoopReader, unfiltered: true, notifyAgo: time.Second,
			rows: []int64{21, 22}, wantID: 22},
		{name: "fetch loop, NOTIFY silent", reader: fetchLoopReader, notifyAgo: notifySilentAfter + time.Second,
			newest: 500, wantID: 20},
		{name: "fetch loop, no NOTIFY yet", reader: fetchLoopReader, newest: 500, wantID: 20},
		// A row the move steps over may come only from its lookup, and its id may be dropped.
		{name: "fetch loop, notified ids dropped", reader: fetchLoopReader, notifyAgo: time.Second, dropped: 1,
			newest: 500, wantHeadRead: true, wantID: 20},
		{name: "fetch loop, list of notified ids full", reader: fetchLoopReader, notifyAgo: time.Second,
			fullList: true, newest: 500, wantHeadRead: true, wantID: 20},
		{name: "polling mode", reader: pollingReader, notifyAgo: time.Second, newest: 500, wantID: 20},
		{name: "fallback poller", reader: fallbackReader, notifyAgo: time.Second, newest: 500, wantID: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mock.MatchExpectationsInOrder(false)

			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
			watcher.lastTimestamp = base.Add(20 * time.Second)
			watcher.lastEventID = 20
			watcher.highestReadEventID = 20
			watcher.events = make(chan datastore.EventWithToken, changelogBatchSize)

			if tt.notifyAgo > 0 {
				watcher.lastNotifyTime = time.Now().Add(-tt.notifyAgo)
			}

			watcher.droppedIDs = tt.dropped

			if tt.fullList {
				for id := int64(1000); len(watcher.notifiedIDs) < notifiedIDLimit; id++ {
					watcher.addNotifiedID(id)
				}
			}

			pollArgs := []driver.Value{healthEventsTable, base.Add(20 * time.Second), int64(20)}

			if !tt.unfiltered {
				pollArgs = filteredPollArgs(watcher, base)
				expectNewestRow(mock, base, tt.newest)
			}

			mock.ExpectQuery(changelogQuery).
				WithArgs(pollArgs...).
				WillReturnRows(listenTestRows(base, tt.rows...))

			require.NoError(t, watcher.fetchNewChangesFor(context.Background(), tt.reader))

			assert.Equal(t, base.Add(time.Duration(tt.wantID)*time.Second), watcher.lastTimestamp)
			assert.Equal(t, tt.wantID, watcher.lastEventID)
			assert.GreaterOrEqual(t, watcher.highestReadEventID, watcher.lastEventID)

			// Only the offered newest row can be left unread.
			if tt.wantHeadRead || tt.unfiltered {
				assert.NoError(t, mock.ExpectationsWereMet(), "the newest row must be read")
			} else {
				assert.Error(t, mock.ExpectationsWereMet(), "the newest row must not be read")
			}
		})
	}
}

// pollForChanges, which polling mode and a hybrid watcher without a LISTEN connection run, never
// moves a filtered watcher up to the newest row, even with a NOTIFY seen recently.
func TestPollForChanges_FilteredWatcher_NeverMovesToNewestRow(t *testing.T) {
	for _, mode := range []ChangeStreamMode{ModePolling, ModeHybrid} {
		t.Run(string(mode), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mock.MatchExpectationsInOrder(false)

			base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", mode)
			watcher.lastTimestamp = base.Add(20 * time.Second)
			watcher.lastEventID = 20
			watcher.highestReadEventID = 20
			watcher.lastNotifyTime = time.Now()

			pollArgs := filteredPollArgs(watcher, base)
			expectNewestRow(mock, base, 500)

			for range 20 {
				mock.ExpectQuery(changelogQuery).WithArgs(pollArgs...).WillReturnRows(changelogRows())
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})

			go func() {
				defer close(done)

				watcher.pollForChanges(ctx)
			}()

			// Every empty poll records the time, so a new time means one more poll.
			var first time.Time

			require.Eventually(t, func() bool {
				first, _ = watcher.LagState()

				return !first.IsZero()
			}, 5*time.Second, time.Millisecond, "pollForChanges must poll")
			require.Eventually(t, func() bool {
				last, _ := watcher.LagState()

				return last.After(first)
			}, 5*time.Second, time.Millisecond, "pollForChanges must poll again")

			cancel()
			<-done

			watcher.mu.RLock()
			defer watcher.mu.RUnlock()

			assert.Equal(t, base.Add(20*time.Second), watcher.lastTimestamp, "polling must not move to the newest row")
			assert.Equal(t, int64(20), watcher.lastEventID)
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
