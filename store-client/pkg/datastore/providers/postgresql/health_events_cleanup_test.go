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
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestLoadHealthEventsCleanupConfig_Retention_ValidateOrDisable(t *testing.T) {
	for _, raw := range []string{"", "0", "30d", "-24h", "23h59m", "broken", "999999999999999999999h", "24h", "720h"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DATASTORE_HEALTH_EVENTS_RETENTION", raw)
			c := loadHealthEventsCleanupConfig(nil)
			if raw != "24h" && raw != "720h" {
				require.Zero(t, c.retention)
				return
			}
			expected, err := time.ParseDuration(raw)
			require.NoError(t, err)
			require.Equal(t, expected, c.retention)
			require.Equal(t, time.Hour, c.interval)
			require.Equal(t, 10000, c.batchSize)
			require.Equal(t, 100, c.maxBatches)
		})
	}
}

func TestLoadHealthEventsCleanupConfig_OptionsOverrideEnvironment(t *testing.T) {
	t.Setenv("DATASTORE_HEALTH_EVENTS_RETENTION", "24h")
	t.Setenv("DATASTORE_HEALTH_EVENTS_CLEANUP_INTERVAL", "2h")
	c := loadHealthEventsCleanupConfig(map[string]string{
		"healthEventsRetention":         "720h",
		"healthEventsCleanupInterval":   "10m",
		"healthEventsCleanupBatchSize":  "7",
		"healthEventsCleanupMaxBatches": "3",
	})
	require.Equal(t, healthEventsCleanupConfig{720 * time.Hour, 10 * time.Minute, 7, 3}, c)
}

func TestLoadHealthEventsCleanupConfig_InvalidLimits_Disable(t *testing.T) {
	for name, options := range map[string]map[string]string{
		"interval below a minute": {"healthEventsCleanupInterval": "30s"},
		"malformed interval":      {"healthEventsCleanupInterval": "hourly"},
		"zero batch size":         {"healthEventsCleanupBatchSize": "0"},
		"negative batch size":     {"healthEventsCleanupBatchSize": "-5"},
		"malformed max batches":   {"healthEventsCleanupMaxBatches": "many"},
	} {
		t.Run(name, func(t *testing.T) {
			options["healthEventsRetention"] = "720h"
			require.Zero(t, loadHealthEventsCleanupConfig(options))
		})
	}
}

func TestStartHealthEventsCleaner_Disabled_StartsNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	cleaner := startHealthEventsCleaner(t.Context(), db, nil)
	require.Nil(t, cleaner)
	require.NoError(t, cleaner.stop(t.Context()))
	require.NoError(t, mock.ExpectationsWereMet())
}

var eventsCleanupTestNow = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func newEventsCleanupTest(t *testing.T) (*healthEventsCleaner, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); db.Close() })

	return &healthEventsCleaner{db: db, config: healthEventsCleanupConfig{720 * time.Hour, time.Hour, 2, 3}}, mock
}

// expectEventsCleanupStart expects the lock, the prerequisite check and, when ready, the clock read.
func expectEventsCleanupStart(mock sqlmock.Sqlmock, ready bool) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).WithArgs(healthEventsCleanupLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta(healthEventsCleanupPrerequisitesQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(ready))

	if ready {
		mock.ExpectQuery("SELECT clock_timestamp").
			WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(eventsCleanupTestNow))
	}
}

func expectEventsCleanupUnlock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).WithArgs(healthEventsCleanupLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
}

// expectEventsBatch expects one batch that starts after the given position and deletes the events
// at the given seconds after eventsCleanupTestNow-60d, with ids e<seconds>, and then their changelog
// rows. It returns the position of the last event, where the next batch starts.
func expectEventsBatch(t *testing.T, mock sqlmock.Sqlmock, after healthEventsPosition, seconds ...int) healthEventsPosition {
	t.Helper()
	mock.ExpectBegin()

	base := eventsCleanupTestNow.Add(-60 * 24 * time.Hour)
	rows := sqlmock.NewRows([]string{"id", "created_at"})
	ids := make([]string, 0, len(seconds))
	reached := after

	for _, second := range seconds {
		event := healthEventsPosition{createdAt: base.Add(time.Duration(second) * time.Second), id: fmt.Sprintf("e%03d", second)}
		rows.AddRow(event.id, event.createdAt)
		ids = append(ids, event.id)

		if reached.before(event) {
			reached = event
		}
	}

	mock.ExpectQuery(regexp.QuoteMeta(healthEventsDeleteQuery)).
		WithArgs(eventsCleanupTestNow.Add(-720*time.Hour), 2, after.createdAt, after.id).WillReturnRows(rows)

	if len(ids) == 0 {
		mock.ExpectRollback()
		return after
	}

	encoded, err := pq.Array(ids).Value()
	require.NoError(t, err)
	mock.ExpectExec(regexp.QuoteMeta(healthEventsDeleteChangesQuery)).WithArgs(encoded).
		WillReturnResult(sqlmock.NewResult(0, int64(len(ids))))
	mock.ExpectCommit()

	return reached
}

func TestHealthEventsCleaner_Prerequisites_NoDelete(t *testing.T) {
	for _, mode := range []string{"held lock", "missing schema or index", "lock error", "prerequisite error", "clock error"} {
		t.Run(mode, func(t *testing.T) {
			c, mock := newEventsCleanupTest(t)
			switch mode {
			case "lock error":
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnError(sql.ErrConnDone)
				mock.ExpectClose()
			case "held lock":
				mock.ExpectQuery("SELECT pg_try_advisory_lock").
					WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
			default:
				mock.ExpectQuery("SELECT pg_try_advisory_lock").
					WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				q := mock.ExpectQuery(regexp.QuoteMeta(healthEventsCleanupPrerequisitesQuery))
				if mode == "prerequisite error" {
					q.WillReturnError(sql.ErrConnDone)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(mode == "clock error"))
				}
				if mode == "clock error" {
					mock.ExpectQuery("SELECT clock_timestamp").WillReturnError(sql.ErrConnDone)
				}
				expectEventsCleanupUnlock(mock)
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

// The batches of a run continue from the last event the previous batch deleted, so a run passes
// each kept event once. A batch's rows come back in no particular order.
func TestHealthEventsCleaner_Batches_ContinueAfterTheLastEventAndStopOnShortBatchOrLimit(t *testing.T) {
	for name, batches := range map[string][][]int{
		"nothing expired":    {{}},
		"one short batch":    {{1}},
		"full then short":    {{2, 1}, {5}},
		"stops at the limit": {{1, 2}, {4, 3}, {9, 6}},
	} {
		t.Run(name, func(t *testing.T) {
			c, mock := newEventsCleanupTest(t)
			expectEventsCleanupStart(mock, true)

			var want int64

			after := healthEventsStart
			for _, seconds := range batches {
				after = expectEventsBatch(t, mock, after, seconds...)
				want += int64(len(seconds))
			}

			expectEventsCleanupUnlock(mock)

			n, err := c.run(t.Context())
			require.NoError(t, err)
			require.Equal(t, want, n)
		})
	}
}

func TestHealthEventsCleaner_TransactionErrors_RollbackAndEndRun(t *testing.T) {
	for _, phase := range []string{"begin", "event delete", "rows", "changelog delete", "commit"} {
		t.Run(phase, func(t *testing.T) {
			c, mock := newEventsCleanupTest(t)
			expectEventsCleanupStart(mock, true)

			begin := mock.ExpectBegin()
			if phase == "begin" {
				begin.WillReturnError(sql.ErrConnDone)
			} else {
				del := mock.ExpectQuery(regexp.QuoteMeta(healthEventsDeleteQuery))
				switch phase {
				case "event delete":
					del.WillReturnError(sql.ErrConnDone)
				case "rows":
					del.WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
						AddRow("a", eventsCleanupTestNow).RowError(0, sql.ErrConnDone))
				default:
					del.WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
						AddRow("a", eventsCleanupTestNow).AddRow("b", eventsCleanupTestNow))
					changes := mock.ExpectExec(regexp.QuoteMeta(healthEventsDeleteChangesQuery))
					if phase == "changelog delete" {
						changes.WillReturnError(sql.ErrConnDone)
					} else {
						changes.WillReturnResult(sqlmock.NewResult(0, 2))
					}
				}

				if phase == "commit" {
					mock.ExpectCommit().WillReturnError(sql.ErrConnDone)
				} else {
					mock.ExpectRollback()
				}
			}

			expectEventsCleanupUnlock(mock)

			n, err := c.run(t.Context())
			require.Error(t, err)
			require.Zero(t, n)
		})
	}
}

func TestHealthEventsCleaner_Cancellation_StopsBeforeBatch(t *testing.T) {
	c, _ := newEventsCleanupTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	n, err := c.deleteBatches(ctx, mustConn(t, c.db), eventsCleanupTestNow.Add(-720*time.Hour))
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, n)
}

func TestHealthEventsPosition_OrdersByInsertTimeThenID(t *testing.T) {
	earlier := healthEventsPosition{createdAt: eventsCleanupTestNow, id: "ffffffff-0000-0000-0000-000000000000"}
	later := healthEventsPosition{createdAt: eventsCleanupTestNow.Add(time.Microsecond), id: "00000000-0000-0000-0000-000000000001"}
	sameTime := healthEventsPosition{createdAt: eventsCleanupTestNow, id: "ffffffff-0000-0000-0000-000000000001"}

	require.True(t, healthEventsStart.before(earlier))
	require.True(t, earlier.before(later))
	require.True(t, earlier.before(sameTime))
	require.False(t, sameTime.before(earlier))
	require.False(t, earlier.before(earlier))
}

func mustConn(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return conn
}

func TestHealthEventsCleaner_DeleteSQL_ExpiresByInsertTimeAndDropsOwnChanges(t *testing.T) {
	require.Contains(t, healthEventsDeleteQuery, "WHERE id = ANY (ARRAY(")
	require.Contains(t, healthEventsDeleteQuery, "WHERE created_at < $1 AND (created_at, id) > ($3, $4::uuid)")
	require.Contains(t, healthEventsDeleteQuery, "ORDER BY created_at, id LIMIT $2")
	// Inside the candidates and again on the row deleted, as a concurrent writer may update it.
	require.Equal(t, 2, strings.Count(healthEventsDeleteQuery, "AND NOT "+healthEventsStillToDrain))
	require.NotContains(t, healthEventsStillToDrain, "= NULL")
	require.Contains(t, healthEventsDeleteChangesQuery, "table_name = 'health_events'")
	require.Contains(t, healthEventsDeleteChangesQuery, "operation = 'DELETE'")
	require.Contains(t, healthEventsDeleteChangesQuery, "record_id = ANY($1::uuid[])")
	require.NotEqual(t, setupLockKey, healthEventsCleanupLockKey)
	require.NotEqual(t, cleanupLockKey, healthEventsCleanupLockKey)
}

func TestHealthEventsCleaner_StoreClose_StopsCleanerBeforePoolClose(t *testing.T) {
	t.Setenv("DATASTORE_HEALTH_EVENTS_RETENTION", "720h")
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

	expectEventsCleanupStart(mock, false)
	expectEventsCleanupUnlock(mock)
	mock.ExpectClose()

	store := &PostgreSQLDataStore{db: db, eventsCleaner: startHealthEventsCleaner(t.Context(), db, nil)}
	require.NotNil(t, store.eventsCleaner)

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("health events cleaner did not run")
	}

	require.NoError(t, store.Close(t.Context()))

	select {
	case <-store.eventsCleaner.done:
	default:
		t.Fatal("pool closed before the health events cleaner stopped")
	}

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHealthEventsCleaner_StoreCloseTimeout_StillClosesPool(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer db.Close()

	stuck := &healthEventsCleaner{cancel: func() {}, done: make(chan struct{})}
	store := &PostgreSQLDataStore{db: db, eventsCleaner: stuck}

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()

	mock.ExpectClose()
	require.ErrorIs(t, store.Close(ctx), context.DeadlineExceeded)
	require.ErrorContains(t, db.PingContext(t.Context()), "database is closed")
	require.NoError(t, mock.ExpectationsWereMet())
	close(stuck.done)
}
