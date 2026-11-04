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
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
