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
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/require"
)

var cleanupTestNow = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func newCleanupTest(t *testing.T) (*changelogCleaner, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); db.Close() })
	return &changelogCleaner{db: db, config: cleanupConfig{24 * time.Hour, time.Hour, 2, 3}}, mock
}

func expectCleanupLock(mock sqlmock.Sqlmock, ready bool) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).WithArgs(cleanupLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta(cleanupPrerequisitesQuery)).WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(ready))
}

func expectCleanupUnlock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).WithArgs(cleanupLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
}

func expectCleanupGuard(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT clock_timestamp").WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(cleanupTestNow))
	mock.ExpectQuery("SELECT horizon::text, observed_at FROM changelog_cleanup_state").
		WillReturnRows(sqlmock.NewRows([]string{"horizon", "observed_at"}).AddRow("100", cleanupTestNow.Add(-time.Hour)))
	mock.ExpectExec(regexp.QuoteMeta(saveCleanupGuardQuery)).WillReturnResult(sqlmock.NewResult(0, 1))
}

func cleanupTokenRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"client_name", "resume_token", "last_updated"})
}

func expectCleanupBound(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT client_name, resume_token, last_updated FROM resume_tokens").
		WillReturnRows(cleanupTokenRows().AddRow("consumer", `{"timestamp":"2026-10-30T00:00:00Z","eventID":9}`, cleanupTestNow))
}

func expectCleanupBatch(mock sqlmock.Sqlmock, count int64) {
	mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(healthEventsTable))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_current_xact_id()::text")).WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow("110"))
	mock.ExpectExec(regexp.QuoteMeta(cleanupDeleteQuery)).
		WithArgs(healthEventsTable, cleanupTestNow.Add(-24*time.Hour), cleanupTestNow.Add(-48*time.Hour), int64(9), 2, "100").
		WillReturnResult(sqlmock.NewResult(0, count))
	mock.ExpectCommit()
}

func TestChangelogCleaner_GuardLifecycle_SeedOrDefer(t *testing.T) {
	cases := []struct {
		name, horizon    string
		observed         time.Time
		readErr          error
		write, failWrite bool
	}{
		{name: "first run", readErr: sql.ErrNoRows, write: true},
		{name: "too early", horizon: "100", observed: cleanupTestNow.Add(-time.Minute)},
		{name: "replacement owner mature", horizon: "100", observed: cleanupTestNow.Add(-time.Hour), write: true},
		{name: "invalid xid", horizon: "bad", observed: cleanupTestNow.Add(-time.Hour), write: true},
		{name: "reserved xid", horizon: "2", observed: cleanupTestNow.Add(-time.Hour), write: true},
		{name: "future metadata", horizon: "100", observed: cleanupTestNow.Add(time.Hour), write: true},
		{name: "zero observation", horizon: "100", write: true},
		{name: "read failure", readErr: sql.ErrConnDone},
		{name: "write failure", horizon: "100", observed: cleanupTestNow.Add(-time.Hour), write: true, failWrite: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			mock.ExpectQuery("SELECT clock_timestamp").WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(cleanupTestNow))
			read := mock.ExpectQuery("SELECT horizon::text, observed_at FROM changelog_cleanup_state")
			if tt.readErr != nil {
				read.WillReturnError(tt.readErr)
			} else {
				read.WillReturnRows(sqlmock.NewRows([]string{"horizon", "observed_at"}).AddRow(tt.horizon, tt.observed))
			}
			if tt.write {
				write := mock.ExpectExec(regexp.QuoteMeta(saveCleanupGuardQuery))
				if tt.failWrite {
					write.WillReturnError(sql.ErrConnDone)
				} else {
					write.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if tt.name == "replacement owner mature" {
				mock.ExpectQuery("SELECT client_name, resume_token, last_updated FROM resume_tokens").WillReturnRows(cleanupTokenRows())
			}
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.Zero(t, n)
			if tt.failWrite || (tt.readErr != nil && !errors.Is(tt.readErr, sql.ErrNoRows)) {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestChangelogCleaner_Prerequisites_NoDelete(t *testing.T) {
	for _, mode := range []string{"held lock", "missing schema or index", "lock error", "prerequisite error", "clock error"} {
		t.Run(mode, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			if mode == "lock error" {
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnError(sql.ErrConnDone)
				mock.ExpectClose()
			} else {
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(mode != "held lock"))
				if mode != "held lock" {
					q := mock.ExpectQuery(regexp.QuoteMeta(cleanupPrerequisitesQuery))
					if mode == "prerequisite error" {
						q.WillReturnError(sql.ErrConnDone)
					} else {
						q.WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(mode == "clock error"))
					}
					if mode == "clock error" {
						mock.ExpectQuery("SELECT clock_timestamp").WillReturnError(sql.ErrConnDone)
					}
					expectCleanupUnlock(mock)
				}
			}
			n, err := c.run(t.Context())
			require.Zero(t, n)
			if mode == "held lock" || mode == "missing schema or index" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestChangelogCleaner_Tokens_UnreadableOrEmptyBlocksDeletion(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", `{"eventID":0}`, `{"timestamp":"bad","eventID":2}`, `{"eventID":-1}`, `{"timestamp":"0001-01-01T00:00:00Z","eventID":1}`, `{"eventID":123}`} {
		t.Run(raw, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			expectCleanupGuard(mock)
			rows := cleanupTokenRows()
			if raw != "" {
				rows.AddRow("broken-client", raw, cleanupTestNow)
			}
			mock.ExpectQuery("SELECT client_name, resume_token, last_updated FROM resume_tokens").WillReturnRows(rows)
			if raw == `{"eventID":123}` {
				mock.ExpectQuery("SELECT changed_at FROM datastore_changelog WHERE id").WithArgs(int64(123)).WillReturnError(sql.ErrNoRows)
			}
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.Zero(t, n)
			if raw == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "broken-client")
			}
		})
	}
}

func TestChangelogCleaner_Tokens_EverySavedPairIncludingStaleAndLegacyCounts(t *testing.T) {
	c, mock := newCleanupTest(t)
	expectCleanupLock(mock, true)
	expectCleanupGuard(mock)
	mock.ExpectQuery("SELECT client_name, resume_token, last_updated FROM resume_tokens").WillReturnRows(cleanupTokenRows().
		AddRow("small-id-new-time", `{"timestamp":"2026-10-31T00:00:00Z","eventID":1}`, cleanupTestNow).
		AddRow("large-id-old-time", `{"timestamp":"2026-10-30T00:00:00Z","eventID":1000}`, cleanupTestNow.Add(-100*time.Hour)).
		AddRow("legacy", `{"eventID":9}`, nil))
	mock.ExpectQuery("SELECT changed_at FROM datastore_changelog WHERE id").WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"changed_at"}).AddRow(cleanupTestNow.Add(-48 * time.Hour)))
	expectCleanupBatch(mock, 0)
	expectCleanupUnlock(mock)
	n, err := c.run(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestChangelogCleaner_BatchLimits_StopWithoutSpinning(t *testing.T) {
	for _, count := range []int64{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			expectCleanupGuard(mock)
			expectCleanupBound(mock)
			batches := 1
			if count == 2 {
				batches = 3
			}
			for range batches {
				expectCleanupBatch(mock, count)
			}
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.NoError(t, err)
			require.Equal(t, int64(batches)*count, n)
		})
	}
}

func TestChangelogCleaner_TransactionErrors_RollbackAndEndRun(t *testing.T) {
	for _, phase := range []string{"begin", "anchor", "delete 40001", "delete error", "rows affected", "commit"} {
		t.Run(phase, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			expectCleanupGuard(mock)
			expectCleanupBound(mock)
			failure := &pq.Error{Code: "40001"}
			mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(healthEventsTable))
			begin := mock.ExpectBegin()
			if phase == "begin" {
				begin.WillReturnError(failure)
			} else {
				anchor := mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_current_xact_id()::text"))
				if phase == "anchor" {
					anchor.WillReturnError(failure)
				} else {
					anchor.WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow("110"))
					del := mock.ExpectExec(regexp.QuoteMeta(cleanupDeleteQuery))
					switch phase {
					case "delete 40001":
						del.WillReturnError(failure)
					case "delete error":
						del.WillReturnError(sql.ErrConnDone)
					case "rows affected":
						del.WillReturnResult(sqlmock.NewErrorResult(failure))
					default:
						del.WillReturnResult(sqlmock.NewResult(0, 2))
					}
				}
				if phase == "commit" {
					mock.ExpectCommit().WillReturnError(failure)
				} else {
					mock.ExpectRollback()
				}
			}
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.Error(t, err)
			require.Zero(t, n)
		})
	}
}

func TestChangelogCleaner_XidWindow_ValidateEveryAnchoredBatch(t *testing.T) {
	for _, anchor := range []string{"99", "2147483748", "bad", "110"} {
		t.Run(anchor, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			expectCleanupGuard(mock)
			expectCleanupBound(mock)
			mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(healthEventsTable))
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_current_xact_id()::text")).
				WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow(anchor))
			if anchor == "110" {
				mock.ExpectExec(regexp.QuoteMeta(cleanupDeleteQuery)).WillReturnResult(sqlmock.NewResult(0, 2))
				mock.ExpectCommit()
				mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(healthEventsTable))
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_current_xact_id()::text")).
					WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow("2147483748"))
			}
			mock.ExpectRollback()
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.Error(t, err)
			if anchor == "110" {
				require.Equal(t, int64(2), n)
			} else {
				require.Zero(t, n)
			}
		})
	}
}

func TestChangelogCleaner_DeleteSQL_EnforcesProtocol(t *testing.T) {
	require.Contains(t, cleanupDeleteQuery, "WITH candidates AS MATERIALIZED")
	require.Contains(t, cleanupDeleteQuery, "ORDER BY changed_at, id LIMIT $5")
	require.Contains(t, cleanupDeleteQuery, "row_version.changed_at < $2")
	require.Contains(t, cleanupDeleteQuery, "(row_version.changed_at, row_version.id) < ($3, $4)")
	require.Contains(t, cleanupDeleteQuery, "age(row_version.xmin) > age($6::xid8::xid)")
	require.Contains(t, cleanupDeleteQuery, "OR age(row_version.xmin) < 0")
	require.NotContains(t, cleanupDeleteQuery, "processed")
	require.NotEqual(t, setupLockKey, cleanupLockKey)
}

func TestChangelogCleaner_UnlockFailure_DiscardsSession(t *testing.T) {
	c, mock := newCleanupTest(t)
	expectCleanupLock(mock, false)
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnError(sql.ErrConnDone)
	mock.ExpectClose()
	n, err := c.run(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 0, c.db.Stats().OpenConnections)
}

func TestChangelogCleaner_Cancellation_StopsBeforeBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	// Cancel synchronously as the token query is matched. The unlock uses a fresh context.
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
		if err == nil && actual == "SELECT client_name, resume_token, last_updated FROM resume_tokens" {
			cancel()
		}
		return err
	})
	// A separate mock installs the deterministic cancellation matcher.
	db, m, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	c := &changelogCleaner{db: db, config: cleanupConfig{24 * time.Hour, time.Hour, 2, 3}}
	expectCleanupLock(m, true)
	expectCleanupGuard(m)
	expectCleanupBound(m)
	expectCleanupUnlock(m)
	_, err = c.run(ctx)
	require.Error(t, err)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestChangelogCleaner_Owners_StartAndStopBeforePoolClose(t *testing.T) {
	for _, owner := range []string{"store", "factory"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", owner, enabled), func(t *testing.T) {
				retention := "0"
				if enabled {
					retention = "24h"
				}
				t.Setenv("DATASTORE_CHANGELOG_RETENTION", retention)
				reached := make(chan struct{})
				matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
					err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
					if err == nil && actual == "SELECT pg_advisory_unlock($1)" {
						close(reached)
					}
					return err
				})
				db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
				require.NoError(t, err)
				defer db.Close()
				if enabled {
					expectCleanupLock(mock, false)
					expectCleanupUnlock(mock)
				}
				mock.ExpectClose()
				var cleaner *changelogCleaner
				var closeOwner func(context.Context) error
				if owner == "factory" {
					c := NewOwnedPostgreSQLDatabaseClient(t.Context(), db, "health_events").(*PostgreSQLDatabaseClient)
					cleaner = c.cleaner
					closeOwner = c.Close
				} else {
					c := &PostgreSQLDataStore{db: db, cleaner: startChangelogCleaner(t.Context(), db, nil)}
					cleaner = c.cleaner
					closeOwner = c.Close
					require.NoError(t, c.GetDatabaseClient().Close(t.Context()))
				}
				if enabled {
					require.NotNil(t, cleaner)
					select {
					case <-reached:
					case <-time.After(5 * time.Second):
						t.Fatal("cleaner did not run")
					}
				} else {
					require.Nil(t, cleaner)
				}
				require.NoError(t, closeOwner(t.Context()))
				if enabled {
					select {
					case <-cleaner.done:
					default:
						t.Fatal("pool closed before cleaner stopped")
					}
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestChangelogCleaner_OwnerCloseCancelled_PreservesPoolUntilStopped(t *testing.T) {
	c, mock := newCleanupTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cleaner := &changelogCleaner{cancel: func() {}, done: make(chan struct{})}
	store := &PostgreSQLDataStore{db: c.db, cleaner: cleaner}
	require.ErrorIs(t, store.Close(ctx), context.Canceled)
	close(cleaner.done)
	mock.ExpectClose()
	require.NoError(t, store.Close(t.Context()))
}

func TestChangelogCleaner_NoEligibleRows_NoDelete(t *testing.T) {
	c, mock := newCleanupTest(t)
	expectCleanupLock(mock, true)
	expectCleanupGuard(mock)
	expectCleanupBound(mock)
	mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}))
	expectCleanupUnlock(mock)
	n, err := c.run(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestChangelogCleaner_TokenReadErrors_BlockDeletion(t *testing.T) {
	for _, phase := range []string{"query", "scan", "iteration", "legacy lookup", "close"} {
		t.Run(phase, func(t *testing.T) {
			c, mock := newCleanupTest(t)
			expectCleanupLock(mock, true)
			expectCleanupGuard(mock)
			q := mock.ExpectQuery("SELECT client_name, resume_token, last_updated FROM resume_tokens")
			switch phase {
			case "query":
				q.WillReturnError(sql.ErrConnDone)
			case "scan":
				q.WillReturnRows(cleanupTokenRows().AddRow(nil, `{}`, nil))
			case "iteration":
				q.WillReturnRows(cleanupTokenRows().AddRow("broken", `{}`, nil).RowError(0, sql.ErrConnDone))
			case "close":
				q.WillReturnRows(cleanupTokenRows().CloseError(sql.ErrConnDone))
			case "legacy lookup":
				q.WillReturnRows(cleanupTokenRows().AddRow("legacy", `{"eventID":5}`, nil))
				mock.ExpectQuery("SELECT changed_at FROM datastore_changelog WHERE id").WithArgs(int64(5)).WillReturnError(sql.ErrConnDone)
			}
			expectCleanupUnlock(mock)
			n, err := c.run(t.Context())
			require.Error(t, err)
			require.Zero(t, n)
		})
	}
}

func TestProvisionCleanupState_OptionalSchema_FailureDoesNotStopIngestion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			require.NoError(t, err)
			defer db.Close()
			q := mock.ExpectExec(regexp.QuoteMeta(createCleanupStateStatement))
			if fail {
				q.WillReturnError(sql.ErrConnDone)
			} else {
				q.WillReturnResult(sqlmock.NewResult(0, 0))
			}
			provisionCleanupState(t.Context(), db)
			mock.ExpectPing()
			require.NoError(t, db.PingContext(t.Context()))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	require.Contains(t, createCleanupStateStatement, "horizon xid8 NOT NULL")
	require.Contains(t, createCleanupStateStatement, "CHECK (singleton)")
}

func TestNewPostgreSQLStore_Cleanup_StartsOnlyAfterSetup(t *testing.T) {
	t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
	reached := make(chan struct{})
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
		if err == nil && actual == cleanupPrerequisitesQuery {
			close(reached)
		}
		return err
	})
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true), sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).WithArgs(setupLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	for range 5 {
		mock.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec(regexp.QuoteMeta(createCleanupStateStatement)).WillReturnResult(sqlmock.NewResult(0, 0))
	for range 2 {
		mock.ExpectExec("ALTER").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	for range 18 {
		mock.ExpectExec("CREATE INDEX").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	definition := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
		"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))) " +
		"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	predicate := "((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	mock.ExpectQuery("SELECT i.indisunique").WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
		WillReturnRows(sqlmock.NewRows([]string{"indisunique", "indisvalid", "indnatts", "indexdef", "predicate"}).AddRow(true, true, 1, definition, predicate))
	mock.ExpectQuery("SELECT i.indisvalid").WithArgs(changelogResumeIndexName, changelogTable).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))
	mock.ExpectExec("CREATE OR REPLACE FUNCTION").WillReturnResult(sqlmock.NewResult(0, 0))
	for range 2 {
		mock.ExpectExec("DO").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).WithArgs(setupLockKey).WillReturnResult(sqlmock.NewResult(0, 1))
	expectCleanupLock(mock, false)
	expectCleanupUnlock(mock)
	mock.ExpectClose()
	store, err := newPostgreSQLStore(t.Context(), datastore.DataStoreConfig{
		Connection: datastore.ConnectionConfig{Host: "localhost", Port: 5432, Database: "test", Username: "test"},
		Options:    map[string]string{"maxConnections": "1"},
	}, func(string) (*sql.DB, error) { return db, nil })
	require.NoError(t, err)
	owner := store.(*PostgreSQLDataStore)
	require.NotNil(t, owner.cleaner)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("store cleaner did not run")
	}
	borrowed := owner.GetDatabaseClient().(*PostgreSQLDatabaseClient)
	require.True(t, borrowed.borrowed)
	require.Nil(t, borrowed.cleaner)
	require.NoError(t, borrowed.Close(t.Context()))
	require.NoError(t, store.Close(t.Context()))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChangelogCleaner_OwnerShutdown_CancelsActiveBatchAndWaitsForRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()
		expectCleanupLock(mock, true)
		expectCleanupGuard(mock)
		expectCleanupBound(mock)
		mock.ExpectQuery(regexp.QuoteMeta(cleanupNextTableQuery)).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(healthEventsTable))
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_current_xact_id()::text")).WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow("110"))
		mock.ExpectExec(regexp.QuoteMeta(cleanupDeleteQuery)).WillDelayFor(time.Hour).WillReturnResult(sqlmock.NewResult(0, 2))
		// sqlmock lacks driver.SessionResetter, so database/sql discards the session
		// after cancellation rollback. Closing that session releases its advisory lock.
		mock.ExpectRollback()
		mock.ExpectClose()
		owner := NewOwnedPostgreSQLDatabaseClient(t.Context(), db, "health_events").(*PostgreSQLDatabaseClient)
		synctest.Wait()
		require.NoError(t, owner.Close(t.Context()))
		synctest.Wait()
		require.Zero(t, db.Stats().OpenConnections)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
