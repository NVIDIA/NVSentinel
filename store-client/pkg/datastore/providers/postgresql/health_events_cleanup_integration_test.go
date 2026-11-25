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

// NVSENTINEL_TEST_POSTGRES_DSN must point to a disposable PostgreSQL database, as for
// TestChangelogCleaner_Integration_Safety. The test owns a unique schema and drops it.
func TestHealthEventsCleaner_Integration_DeletesExpiredEventsAndOwnChanges(t *testing.T) {
	dsn := os.Getenv("NVSENTINEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NVSENTINEL_TEST_POSTGRES_DSN to run PostgreSQL retention tests")
	}

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		var err error
		dsn, err = pq.ParseURL(dsn)
		require.NoError(t, err)
	}

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()

	// Keep public in search_path: createTables uses uuid-ossp functions that can live there.
	schema := fmt.Sprintf("events_cleaner_%d", time.Now().UnixNano())
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

	require.NoError(t, createTables(ctx, db))

	var ready bool
	require.NoError(t, db.QueryRowContext(ctx, healthEventsCleanupPrerequisitesQuery).Scan(&ready))
	require.True(t, ready)

	// Events written through the production trigger, three of them older than the retention.
	insert := func(t *testing.T, node string, age time.Duration) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO health_events (node_name, document, created_at)
 VALUES ($1, '{}'::jsonb, clock_timestamp() - $2::interval)`, node, fmt.Sprintf("%d seconds", int(age.Seconds())))
		require.NoError(t, err)
	}

	for _, node := range []string{"expired-a", "expired-b", "expired-c"} {
		insert(t, node, 40*24*time.Hour)
	}

	insert(t, "boundary-kept", 29*24*time.Hour)
	insert(t, "fresh-kept", time.Minute)

	_, err = db.ExecContext(ctx, `UPDATE health_events SET node_quarantined = 'Quarantined' WHERE node_name = 'expired-a'`)
	require.NoError(t, err)

	count := func(t *testing.T, query string) int {
		t.Helper()

		var n int
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&n))

		return n
	}

	require.Equal(t, 6, count(t, `SELECT count(*) FROM datastore_changelog WHERE table_name = 'health_events'`))

	var expiredIDs []string

	idRows, err := db.QueryContext(ctx, `SELECT id::text FROM health_events WHERE node_name LIKE 'expired-%' ORDER BY id`)
	require.NoError(t, err)

	for idRows.Next() {
		var id string
		require.NoError(t, idRows.Scan(&id))
		expiredIDs = append(expiredIDs, id)
	}

	require.NoError(t, idRows.Err())
	require.NoError(t, idRows.Close())
	require.Len(t, expiredIDs, 3)

	// DELETE history the cleaner did not write: one for an event it does not delete, and one
	// for another table that carries the id of an event it does delete. Both must stay.
	changelogRow := func(t *testing.T, table, recordID string) int64 {
		t.Helper()

		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO datastore_changelog (table_name, record_id, operation, old_values)
 VALUES ($1, $2::uuid, 'DELETE', '{}'::jsonb) RETURNING id`, table, recordID).Scan(&id))

		return id
	}

	unrelated := changelogRow(t, "health_events", "00000000-0000-4000-8000-000000000001")
	otherTable := changelogRow(t, "maintenance_events", expiredIDs[0])

	cleaner := &healthEventsCleaner{db: db, config: healthEventsCleanupConfig{720 * time.Hour, time.Hour, 2, 10}}

	n, err := cleaner.run(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "two batches: a full one of 2, then a short one of 1")

	rows, err := db.QueryContext(ctx, `SELECT node_name FROM health_events ORDER BY node_name`)
	require.NoError(t, err)

	var kept []string

	for rows.Next() {
		var node string
		require.NoError(t, rows.Scan(&node))
		kept = append(kept, node)
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"boundary-kept", "fresh-kept"}, kept)

	// The deletes wrote changelog rows in their transaction; the cleaner removed them before commit.
	var owned int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM datastore_changelog
 WHERE table_name = 'health_events' AND operation = 'DELETE' AND record_id = ANY($1::uuid[])`,
		pq.Array(expiredIDs)).Scan(&owned))
	require.Zero(t, owned)

	var deleteHistory []int64

	historyRows, err := db.QueryContext(ctx, `SELECT id FROM datastore_changelog WHERE operation = 'DELETE' ORDER BY id`)
	require.NoError(t, err)

	for historyRows.Next() {
		var id int64
		require.NoError(t, historyRows.Scan(&id))
		deleteHistory = append(deleteHistory, id)
	}

	require.NoError(t, historyRows.Err())
	require.NoError(t, historyRows.Close())
	require.Equal(t, []int64{unrelated, otherTable}, deleteHistory, "DELETE history the cleaner did not write stays")
	// The earlier history of the expired events is the changelog retention's to remove (#2170).
	require.Equal(t, 5, count(t, `SELECT count(*) FROM datastore_changelog WHERE operation = 'INSERT'`))
	require.Equal(t, 1, count(t, `SELECT count(*) FROM datastore_changelog WHERE operation = 'UPDATE'`))

	n, err = cleaner.run(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// Another owner holding the lock keeps this run from deleting anything.
	insert(t, "expired-d", 40*24*time.Hour)

	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()

	var locked bool
	require.NoError(t, holder.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", healthEventsCleanupLockKey).Scan(&locked))
	require.True(t, locked)

	n, err = cleaner.run(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, count(t, `SELECT count(*) FROM health_events WHERE node_name = 'expired-d'`))

	require.NoError(t, holder.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", healthEventsCleanupLockKey).Scan(&locked))
	require.True(t, locked)

	n, err = cleaner.run(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

// The events of drains in progress stay however old they are, in both key spellings, and a run
// deletes past them in order: batches continue after the last event they deleted. A quarantined
// event whose drain never started goes, together with the later event that ended its quarantine,
// so no stale record outlives its end and is replayed on restart.
func TestHealthEventsCleaner_Integration_KeepsDrainsInProgress(t *testing.T) {
	dsn := os.Getenv("NVSENTINEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NVSENTINEL_TEST_POSTGRES_DSN to run PostgreSQL retention tests")
	}

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		var err error
		dsn, err = pq.ParseURL(dsn)
		require.NoError(t, err)
	}

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()

	schema := fmt.Sprintf("events_kept_%d", time.Now().UnixNano())
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

	require.NoError(t, createTables(ctx, db))

	// Oldest first, so kept events sit ahead of and between the expired ones.
	for i, event := range []struct{ node, document string }{
		{"keep-in-progress", `{"healtheventstatus":{"nodequarantined":"Quarantined","userpodsevictionstatus":{"status":"InProgress"}}}`},
		{"delete-drained", `{"healtheventstatus":{"nodequarantined":"Quarantined","userpodsevictionstatus":{"status":"Succeeded"}}}`},
		{"delete-stale-not-started", `{"healtheventstatus":{"nodequarantined":"Quarantined","userpodsevictionstatus":{"status":""}}}`},
		{"keep-legacy-in-progress", `{"healtheventstatus":{"nodeQuarantined":"AlreadyQuarantined","userpodsevictionstatus":{"Status":"InProgress"}}}`},
		{"delete-session-end", `{"healtheventstatus":{"nodequarantined":"UnQuarantined","userpodsevictionstatus":{"status":"Succeeded"}}}`},
		{"delete-no-drain-status", `{"healtheventstatus":{"nodequarantined":"AlreadyQuarantined"}}`},
		{"delete-no-status", `{}`},
		{"delete-cancelled", `{"healtheventstatus":{"nodequarantined":"AlreadyQuarantined","userpodsevictionstatus":{"status":"Cancelled"}}}`},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO health_events (node_name, document, created_at)
 VALUES ($1, $2::jsonb, clock_timestamp() - interval '40 days' + make_interval(secs => $3))`, event.node, event.document, i)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO health_events (node_name, document) VALUES ('keep-fresh', '{}'::jsonb)`)
	require.NoError(t, err)

	cleaner := &healthEventsCleaner{db: db, config: healthEventsCleanupConfig{720 * time.Hour, time.Hour, 2, 10}}

	n, err := cleaner.run(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(6), n, "three full batches of 2, then an empty one")

	rows, err := db.QueryContext(ctx, `SELECT node_name FROM health_events ORDER BY created_at`)
	require.NoError(t, err)

	var kept []string

	for rows.Next() {
		var node string
		require.NoError(t, rows.Scan(&node))
		kept = append(kept, node)
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"keep-in-progress", "keep-legacy-in-progress", "keep-fresh"}, kept)
}
