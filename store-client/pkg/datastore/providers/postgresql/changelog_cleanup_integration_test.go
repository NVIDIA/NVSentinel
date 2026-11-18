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

//go:build integration

package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// NVSENTINEL_TEST_POSTGRES_DSN must point to a disposable PostgreSQL database.
// Each invocation owns a unique schema and never drops existing application tables.
func TestChangelogCleaner_Integration_Safety(t *testing.T) {
	dsn := os.Getenv("NVSENTINEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NVSENTINEL_TEST_POSTGRES_DSN to run PostgreSQL safety tests")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		var err error
		dsn, err = pq.ParseURL(dsn)
		require.NoError(t, err)
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()
	schema := fmt.Sprintf("cleaner_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
		require.NoError(t, err)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn+" search_path="+schema+",public")
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(10)
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	require.NoError(t, createTables(ctx, db))
	var ready bool
	require.NoError(t, db.QueryRowContext(ctx, cleanupPrerequisitesQuery).Scan(&ready))
	require.True(t, ready)
	cleaner := &changelogCleaner{db: db, config: cleanupConfig{24 * time.Hour, time.Hour, 2, 3}}
	reset := func(t *testing.T) {
		t.Helper()
		exec(t, `TRUNCATE health_events,maintenance_events,datastore_changelog,resume_tokens,changelog_cleanup_state RESTART IDENTITY`)
		cleaner.config.batchSize = 2
		cleaner.config.maxBatches = 3
	}
	token := func(t *testing.T, raw string) {
		t.Helper()
		exec(t, `INSERT INTO resume_tokens(client_name,resume_token) VALUES ('consumer',$1) ON CONFLICT(client_name) DO UPDATE SET resume_token=excluded.resume_token`, raw)
	}
	seed := func(t *testing.T) { n, err := cleaner.run(ctx); require.NoError(t, err); require.Zero(t, n) }
	mature := func(t *testing.T) {
		exec(t, `UPDATE changelog_cleanup_state SET observed_at=clock_timestamp()-interval '2 hours'`)
	}
	remaining := func(t *testing.T, want []int64) {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id FROM datastore_changelog ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		got := []int64{}
		for rows.Next() {
			var id int64
			require.NoError(t, rows.Scan(&id))
			got = append(got, id)
		}
		require.NoError(t, rows.Err())
		require.Equal(t, want, got)
	}
	run := func(t *testing.T, want int64) {
		n, err := cleaner.run(ctx)
		require.NoError(t, err)
		require.Equal(t, want, n)
	}

	// Synthetic timestamps exercise boundaries without changing the production schema.
	insert := func(t *testing.T, values string) {
		t.Helper()
		exec(t, `INSERT INTO datastore_changelog(id,table_name,changed_at,record_id,operation,new_values)
 SELECT v.id,v.table_name,v.changed_at::timestamptz,uuid_generate_v4(),'INSERT','{}'::jsonb FROM (`+values+`) v(id,table_name,changed_at)`)
	}

	t.Run("production-triggers-and-bigserial", func(t *testing.T) {
		reset(t)
		exec(t, `INSERT INTO health_events(node_name,document) VALUES ('healthy','{}')`)
		exec(t, `UPDATE health_events SET document='{"updated":true}'`)
		exec(t, `DELETE FROM health_events`)
		var operations string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(operation,',' ORDER BY id) FROM datastore_changelog`).Scan(&operations))
		require.Equal(t, "INSERT,UPDATE,DELETE", operations)
		remaining(t, []int64{1, 2, 3})
	})

	t.Run("age-and-pair-bound", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":9}`)
		insert(t, `VALUES
   (100,'health_events','2000-01-01'),(1,'health_events','2002-01-01'),
   (8,'health_events','2001-01-01'),(9,'health_events','2001-01-01')`)
		seed(t)
		remaining(t, []int64{1, 8, 9, 100})
		mature(t)
		run(t, 2)
		remaining(t, []int64{1, 9})
		// A lower id with a new changed_at stays even with a consumer beyond it.
		insert(t, `VALUES(2,'health_events',clock_timestamp())`)
		exec(t, `UPDATE resume_tokens SET resume_token=jsonb_build_object('timestamp',clock_timestamp(),'eventID',1000)`)
		mature(t)
		run(t, 2)
		remaining(t, []int64{2})
	})
	t.Run("empty-and-malformed-and-legacy-missing", func(t *testing.T) {
		reset(t)
		insert(t, `VALUES(1,'health_events','2000-01-01')`)
		seed(t)
		mature(t)
		run(t, 0)
		remaining(t, []int64{1})
		for _, raw := range []string{`{}`, `{"eventID":999}`} {
			token(t, raw)
			mature(t)
			n, err := cleaner.run(ctx)
			require.Error(t, err)
			require.Zero(t, n)
			remaining(t, []int64{1})
		}
	})
	t.Run("incomplete-consumer-set-gate-defers", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":999}`)
		insert(t, `VALUES(1,'health_events','2000-01-01')`)
		t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
		t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "")
		owner := NewOwnedPostgreSQLDatabaseClient(ctx, db, healthEventsTable).(*PostgreSQLDatabaseClient)
		require.Nil(t, owner.cleaner)
		remaining(t, []int64{1})
		// Deliberately do not Close this wrapper: the fixture owns its shared pool.
	})
	t.Run("pending-deliveries-preserved", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"1999-01-01T00:00:00Z","eventID":0}`)
		insert(t, `VALUES(1,'health_events','2000-01-01'),(2,'health_events','2000-01-02'),(3,'health_events','2000-01-03')`)
		watcher := NewPostgreSQLChangeStreamWatcher(db, "consumer", healthEventsTable, "", ModePolling)
		require.NoError(t, watcher.initializeResumePosition(ctx))
		require.NoError(t, watcher.fetchNewChanges(ctx))
		require.Len(t, watcher.pending, 3)
		first := <-watcher.events
		require.Equal(t, "1", string(first.ResumeToken))
		require.NoError(t, watcher.MarkProcessed(ctx, first.ResumeToken))
		require.Len(t, watcher.pending, 2)
		seed(t)
		mature(t)
		run(t, 1)
		remaining(t, []int64{2, 3})
		restarted := NewPostgreSQLChangeStreamWatcher(db, "consumer", healthEventsTable, "", ModePolling)
		require.NoError(t, restarted.initializeResumePosition(ctx))
		require.NoError(t, restarted.fetchNewChanges(ctx))
		require.Len(t, restarted.pending, 2)
	})
	t.Run("long-writer-previous-xmin", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":999}`)
		writer, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer writer.Rollback()
		_, err = writer.ExecContext(ctx, `INSERT INTO health_events(node_name,document) VALUES ('long-writer','{}')`)
		require.NoError(t, err)
		_, err = writer.ExecContext(ctx, `UPDATE datastore_changelog SET changed_at='2000-01-01'`)
		require.NoError(t, err)
		seed(t)
		require.NoError(t, writer.Commit())
		mature(t)
		run(t, 0)
		remaining(t, []int64{1})
		// It is still available through the existing NOTIFY catch-up query below a saved read head.
		watcher := NewPostgreSQLChangeStreamWatcher(db, "observer", healthEventsTable, "", ModePolling)
		watcher.lastTimestamp = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		watcher.lastEventID = 999
		deliveries, err := watcher.queryNotifiedRows(ctx, []int64{1})
		require.NoError(t, err)
		require.Len(t, deliveries, 1)
		mature(t)
		run(t, 1)
		remaining(t, []int64{})
	})
	t.Run("limits-and-short-batch", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":999}`)
		exec(t, `INSERT INTO datastore_changelog(table_name,changed_at,record_id,operation,new_values) SELECT 'health_events','2000-01-01'::timestamptz,uuid_generate_v4(),'INSERT','{}'::jsonb FROM generate_series(1,9)`)
		seed(t)
		mature(t)
		cleaner.config.maxBatches = 2
		run(t, 4)
		remaining(t, []int64{5, 6, 7, 8, 9})
		mature(t)
		cleaner.config.maxBatches = 10
		run(t, 5)
		remaining(t, []int64{})
	})
	t.Run("maintenance-progress-after-short-health-batch", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":999}`)
		insert(t, `VALUES(1,'health_events','2000-01-01'),(2,'maintenance_events','2000-01-02')`)
		seed(t)
		mature(t)
		run(t, 1)
		remaining(t, []int64{2})
		mature(t)
		run(t, 1)
		remaining(t, []int64{})
	})
	t.Run("concurrent-update-40001-rolls-back-entire-batch", func(t *testing.T) {
		reset(t)
		token(t, `{"timestamp":"2001-01-01T00:00:00Z","eventID":999}`)
		insert(t, `VALUES(1,'health_events','2000-01-01'),(2,'health_events','2000-01-02')`)
		seed(t)
		mature(t)
		updater, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer updater.Rollback()
		_, err = updater.ExecContext(ctx, `UPDATE datastore_changelog SET processed=true WHERE id=2`)
		require.NoError(t, err)
		type result struct {
			n   int64
			err error
		}
		finished := make(chan result, 1)
		go func() { n, err := cleaner.run(ctx); finished <- result{n, err} }()
		// Wait for the actual delete to block on the updater. No timing sleep or injected SQL error.
		require.Eventually(t, func() bool {
			// Eventually runs this in another goroutine, where require must not call FailNow.
			var waiting bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%DELETE FROM datastore_changelog AS row_version%')`).Scan(&waiting); err != nil {
				return false
			}
			return waiting
		}, 10*time.Second, 10*time.Millisecond)
		require.NoError(t, updater.Commit())
		got := <-finished
		require.Zero(t, got.n)
		var pgErr *pq.Error
		require.ErrorAs(t, got.err, &pgErr)
		require.Equal(t, pq.ErrorCode("40001"), pgErr.Code)
		remaining(t, []int64{1, 2})
	})
	t.Run("factory-owner-missing-schema-no-ddl", func(t *testing.T) {
		exec(t, `DROP TABLE changelog_cleanup_state`)
		t.Setenv("DATASTORE_CHANGELOG_RETENTION", "0")
		c := NewOwnedPostgreSQLDatabaseClient(ctx, db, "health_events").(*PostgreSQLDatabaseClient)
		// Retention is disabled in the harness environment; explicitly exercise the shared core.
		require.Nil(t, c.cleaner)
		run(t, 0)
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('changelog_cleanup_state') IS NOT NULL`).Scan(&exists))
		require.False(t, exists)
	})
	// Record the exact server version without credentials.
	var version string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT version()`).Scan(&version))
	t.Log(strings.Split(version, " on ")[0])
}
