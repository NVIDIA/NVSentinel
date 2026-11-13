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
	"database/sql"
	"net"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

func TestHybridChangeStream_PollsWhileListenUnavailable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	watcher := NewPostgreSQLChangeStreamWatcher(
		db,
		"test-client",
		healthEventsTable,
		"postgres://127.0.0.1:"+strconv.Itoa(port)+"/nvsentinel?sslmode=disable",
		ModeHybrid,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT resume_token FROM resume_tokens WHERE client_name = $1")).
		WithArgs("test-client").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())
	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())
	watcher.Start(ctx)

	// Once the failed LISTEN attempt falls back to polling, the empty poll establishes readiness.
	require.Eventually(t, watcher.IsReady, 10*time.Second, 10*time.Millisecond)
	// A second poll proves the fallback stays active while Listen is blocked.
	require.Eventually(t, func() bool {
		return mock.ExpectationsWereMet() == nil
	}, 7*time.Second, 25*time.Millisecond)

	cancel()
	require.NoError(t, watcher.Close(context.Background()))
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-watcher.Events():
			return !ok
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestFetchNewChanges_ReadinessRequiresSuccessfulPoll(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	require.False(t, watcher.IsReady())

	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnError(sql.ErrConnDone)
	require.Error(t, watcher.fetchNewChanges(context.Background()))
	require.False(t, watcher.IsReady())

	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())
	require.NoError(t, watcher.fetchNewChanges(context.Background()))
	require.True(t, watcher.IsReady())

	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnError(sql.ErrConnDone)
	require.Error(t, watcher.fetchNewChanges(context.Background()))
	require.False(t, watcher.IsReady())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFetch_ReadFailureOverridesActiveListener(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(strconv.FormatBool(byID), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
			watcher.listenerConnected, watcher.listenerSubscribed = true, true
			require.True(t, watcher.IsReady())
			fetch := func() error { return watcher.fetchNewChanges(context.Background()) }
			query := changelogQuery
			if byID {
				watcher.addNotifiedID(1)
				fetch = func() error { return watcher.fetchNotifiedRows(context.Background()) }
				query = "SELECT id, record_id, operation, old_values, new_values, changed_at"
			}
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnError(sql.ErrConnDone)
			require.Error(t, fetch())
			require.False(t, watcher.IsReady(), "LISTEN cannot compensate for an unreadable query pool")
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(changelogRows())
			require.NoError(t, fetch())
			require.True(t, watcher.IsReady())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestReadinessDoesNotWaitForLongCatchUp(t *testing.T) {
	watcher := NewPostgreSQLChangeStreamWatcher(nil, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.listenerConnected = true
	watcher.listenerSubscribed = true
	watcher.catchUpPending = true

	require.True(t, watcher.IsReady(), "an active listener can report readiness while ordered catch-up drains")
}

func TestAdaptiveFallbackPoller_PollsWhileListenerSetupStalls(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "postgres://stalled", ModeHybrid)
	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())
	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.adaptiveFallbackPoller(ctx, stopCh)
	}()

	// No listener subscription arrives, representing a stalled handshake. The fallback waits
	// for its first five-second tick, then establishes readiness while LISTEN remains blocked.
	require.Never(t, watcher.IsReady, 4*time.Second, 10*time.Millisecond,
		"fallback must not race LISTEN setup with an immediate query")
	require.Eventually(t, watcher.IsReady, 2*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, 7*time.Second, 25*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("adaptive fallback poller did not stop after cancellation")
	}
}

func TestFetchNewChanges_HeadReadFailureClearsReadiness(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	watcher.pipeline = client.NewPostgreSQLPipelineBuilder().BuildAllHealthEventInsertsPipeline()
	watcher.lastNotifyTime = time.Now()
	watcher.listenerConnected = true
	watcher.listenerSubscribed = true

	mock.ExpectQuery("SELECT changed_at, id FROM datastore_changelog").WillReturnError(sql.ErrConnDone)
	require.Error(t, watcher.fetchNewChangesFor(context.Background(), fetchLoopReader))
	require.False(t, watcher.IsReady(), "a head query failure means a fetch cannot be considered healthy")

	mock.ExpectQuery(regexp.QuoteMeta(changelogQuery)).WillReturnRows(changelogRows())
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	interval := 30 * time.Second
	watcher.pollAccordingToNotificationHealth(context.Background(), ticker, &interval)
	require.True(t, watcher.IsReady(), "a later successful poll restores readiness")
	require.Equal(t, 5*time.Second, interval, "a failed listener-side read must force fallback polling")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessNotification_ReconnectCatchesUpBeforeNewNotification(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	when := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	watcher.lastTimestamp = when
	watcher.lastEventID = 1
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when, int64(1)).
		WillReturnRows(healthEventRow(changelogRows(), 2, when.Add(time.Second)))
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when.Add(time.Second), int64(2)).
		WillReturnRows(changelogRows())
	watcher.processNotification(context.Background(), nil)
	require.True(t, watcher.catchUpPending, "the listener marks the subscription gap for the fetch loop")
	require.NoError(t, watcher.readChangelog(context.Background()))
	require.Len(t, watcher.events, 1, "the fetch loop recovers the missed row")
	require.Equal(t, []byte("2"), (<-watcher.events).ResumeToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListenerReconnect_DoesNotPretendToReceiveNotification(t *testing.T) {
	watcher := NewPostgreSQLChangeStreamWatcher(nil, "test-client", healthEventsTable, "", ModeHybrid)
	before := time.Now().Add(-time.Minute)
	watcher.lastNotifyTime = before
	watcher.listenerEventCallback()(pq.ListenerEventReconnected, nil)
	require.Equal(t, before, watcher.lastNotifyTime)
}

func TestProcessNotification_FailedCatchUpCannotSkipMissedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeHybrid)
	when := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	watcher.lastTimestamp, watcher.lastEventID = when, 1
	mock.ExpectQuery(changelogQuery).WillReturnError(sql.ErrConnDone)
	watcher.processNotification(context.Background(), nil)
	require.Error(t, watcher.catchUpAfterSubscribe(context.Background()))
	require.False(t, watcher.IsReady())
	require.True(t, watcher.catchUpPending)
	// The adaptive poll can retry and drain two batches without another NOTIFY.
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when, int64(1)).
		WillReturnRows(healthEventRow(changelogRows(), 2, when.Add(time.Second)))
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when.Add(time.Second), int64(2)).
		WillReturnRows(healthEventRow(changelogRows(), 3, when.Add(2*time.Second)))
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when.Add(2*time.Second), int64(3)).
		WillReturnRows(changelogRows())
	interval := 5 * time.Second
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	watcher.pollAccordingToNotificationHealth(context.Background(), ticker, &interval)
	require.False(t, watcher.catchUpPending)

	// A subsequent notification is queued, and the normal poll delivers its row in order.
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when.Add(2*time.Second), int64(3)).
		WillReturnRows(healthEventRow(changelogRows(), 4, when.Add(3*time.Second)))
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, when.Add(3*time.Second), int64(4)).
		WillReturnRows(changelogRows())
	watcher.processNotification(context.Background(), &pq.Notification{Extra: `{"id":4,"table":"health_events","operation":"INSERT"}`})
	require.NoError(t, watcher.readChangelog(context.Background()))
	require.Len(t, watcher.events, 3)
	for _, id := range []string{"2", "3", "4"} {
		require.Equal(t, []byte(id), (<-watcher.events).ResumeToken)
	}
	require.True(t, watcher.IsReady())
	require.False(t, watcher.catchUpPending)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListenLoop_StartupCatchesUpWithoutNotification(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModeNotify)
	watcher.listener = &pq.Listener{Notify: make(chan *pq.Notification)}
	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); watcher.listenLoop(ctx) }()
	require.Eventually(t, func() bool {
		watcher.mu.RLock()
		pending := watcher.catchUpPending
		watcher.mu.RUnlock()

		return !pending && mock.ExpectationsWereMet() == nil
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener did not stop")
	}
	require.NoError(t, mock.ExpectationsWereMet())
}
