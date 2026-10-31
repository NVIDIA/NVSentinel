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
	"github.com/stretchr/testify/require"
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

	// The empty poll establishes readiness even though LISTEN keeps retrying the refused port.
	require.Eventually(t, watcher.IsReady, 2*time.Second, 10*time.Millisecond)
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
