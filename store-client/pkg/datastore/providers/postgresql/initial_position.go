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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type resetResumeTokenFunc func(
	context.Context, client.DatabaseClient, client.TokenConfig,
) (client.ResumeControlDecision, error)

// fullReplayStartTime precedes every changelog row. The Unix epoch renders as a parseable
// timestamp in every session time zone; a year-1 instant becomes a BC date west of UTC.
var fullReplayStartTime = time.Unix(0, 0).UTC()

const loadResumeTokenQuery = `SELECT resume_token FROM resume_tokens WHERE client_name = $1`

// Inspect activity first, then sample the clock; the CTE dependency fixes that order.
// A writer open during the scan bounds the start with its xact_start, however long it runs.
// pg_stat_activity is not consistent with the clock, so no order of observations is exact:
// this order can miss a writer that starts between the scan and the clock sample. Its
// changed_at is below the saved start, so the poll excludes its row whenever it commits,
// unless a NOTIFY recovers it once LISTEN runs. Sampling the clock first would instead miss
// an open writer that commits between the two observations. Closing either window exactly
// needs commit-order data such as track_commit_timestamp.
// Visible non-client backends cannot write the changelog. PostgreSQL also hides backend_type
// without statistics privileges, so an unknown type must still be treated as a possible writer.
// Hidden or disabled activity gives no start bound. Prepared transactions detach from their
// session and can commit between any two observations, so any database that allows them
// (max_prepared_transactions > 0) gets the full-replay boundary too.
// The boundary must round-trip through JSON and Go's time parser in every session time zone.
const insertInitialPositionQuery = `WITH activity AS MATERIALIZED (
	SELECT MIN(xact_start) AS oldest_start,
		COALESCE(bool_or(state IS NULL OR state = 'disabled'), FALSE) AS unknown_start
	FROM pg_stat_activity
	WHERE (backend_type = 'client backend' OR backend_type IS NULL)
		AND datname = current_database() AND pid <> pg_backend_pid()
), bound AS MATERIALIZED (
	SELECT activity.oldest_start, activity.unknown_start, clock_timestamp() AS observed_at
	FROM activity
)
INSERT INTO resume_tokens (client_name, resume_token, last_updated)
SELECT $1, jsonb_build_object('timestamp',
	CASE WHEN bound.unknown_start OR current_setting('max_prepared_transactions')::int > 0
	THEN TIMESTAMPTZ '1970-01-01 00:00:00+00'
	ELSE LEAST(bound.observed_at, bound.oldest_start) END,
	'eventID', 0), statement_timestamp()
FROM bound
ON CONFLICT (client_name) DO NOTHING`

// initializeResumePosition prepares every error-returning construction path before reading.
// This correctness invariant applies regardless of whether changelog retention is enabled.
func (w *PostgreSQLChangeStreamWatcher) initializeResumePosition(ctx context.Context) error {
	tokenJSON, err := w.loadOrCreateInitialToken(ctx)
	if err != nil {
		return fmt.Errorf("initialize resume token for %s: %w", w.clientName, err)
	}

	// Release the reserved connection before legacy lookups, including when the pool has size one.
	if err := w.restoreResumePosition(ctx, tokenJSON); err != nil {
		if !errors.Is(err, errMalformedResumeToken) {
			return fmt.Errorf("restore initial position for %s: %w", w.clientName, err)
		}

		// Failing here would fail every restart. Keep the row for an operator and replay
		// the changelog from the beginning, as before initial positions were persisted.
		slog.Error("Resume token is malformed; replaying the changelog from the beginning",
			"client", w.clientName, "error", err)

		w.lastTimestamp = time.Time{}
		w.lastEventID = 0
		w.highestReadEventID = 0
	}

	// Inspect the stored winner, including when another constructor inserted it.
	// The unchanged token schema cannot distinguish the causes after read-back.
	if w.lastTimestamp.Equal(fullReplayStartTime) {
		slog.Warn("PostgreSQL watcher starts with full changelog replay",
			"client", w.clientName,
			"reason", "hidden/disabled client activity, or prepared transactions enabled",
			"remedy", "Use the same role for all writers or grant pg_read_all_stats; "+
				"enable activity tracking, or set max_prepared_transactions to 0",
			"docs", "docs/postgresql-provider.md#durable-watcher-start-position")
	}

	w.positionInitialized = true

	return nil
}

func (w *PostgreSQLChangeStreamWatcher) loadOrCreateInitialToken(ctx context.Context) ([]byte, error) {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve initialization connection: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Warn("Failed to release initialization connection", "client", w.clientName, "error", err)
		}
	}()

	var tokenJSON []byte

	err = conn.QueryRowContext(ctx, loadResumeTokenQuery, w.clientName).Scan(&tokenJSON)
	if err == nil {
		return tokenJSON, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load resume token: %w", err)
	}

	// NOW() on writers is their transaction start, which can precede this insert.
	// Keep that earlier boundary so a writer still open at construction can commit later.
	// DO NOTHING preserves a concurrent constructor's pair and last_updated value.
	if _, err := conn.ExecContext(ctx, insertInitialPositionQuery, w.clientName); err != nil {
		return nil, fmt.Errorf("persist database-clock start position: %w", err)
	}

	// A separate statement sees the committed winner after ON CONFLICT waits.
	// Never use a locally proposed pair when another constructor inserted first.
	if err := conn.QueryRowContext(ctx, loadResumeTokenQuery, w.clientName).Scan(&tokenJSON); err != nil {
		return nil, fmt.Errorf("read persisted start position: %w", err)
	}

	return tokenJSON, nil
}
