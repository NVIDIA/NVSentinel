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
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const cleanupLockKey int64 = 2026091402

const createCleanupStateStatement = `CREATE TABLE IF NOT EXISTS changelog_cleanup_state (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
 horizon xid8 NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL
)`

// Factory connections verify prerequisites; only store setup or an operator provisions them.
const cleanupPrerequisitesQuery = `SELECT
 to_regclass('datastore_changelog') IS NOT NULL
 AND to_regclass('resume_tokens') IS NOT NULL
 AND to_regclass('changelog_cleanup_state') IS NOT NULL
 AND EXISTS (SELECT 1 FROM pg_index i
 WHERE i.indexrelid = to_regclass('idx_changelog_resume_all')
 AND i.indrelid = to_regclass('datastore_changelog') AND i.indisvalid
 AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indnkeyatts = 3
 AND pg_get_indexdef(i.indexrelid, 1, true) = 'table_name'
 AND pg_get_indexdef(i.indexrelid, 2, true) = 'changed_at'
 AND pg_get_indexdef(i.indexrelid, 3, true) = 'id')`

const saveCleanupGuardQuery = `WITH horizon AS MATERIALIZED (
 SELECT pg_snapshot_xmin(pg_current_snapshot()) AS value
)
INSERT INTO changelog_cleanup_state (singleton, horizon, observed_at)
SELECT TRUE, value, clock_timestamp() FROM horizon
ON CONFLICT (singleton) DO UPDATE SET horizon = EXCLUDED.horizon, observed_at = EXCLUDED.observed_at`

const cleanupDeleteQuery = `WITH candidates AS MATERIALIZED (
 SELECT id FROM datastore_changelog
 WHERE table_name = $1 AND changed_at < $2
 AND (changed_at, id) < ($3, $4)
 ORDER BY changed_at, id LIMIT $5
)
DELETE FROM datastore_changelog AS row_version
USING candidates
WHERE row_version.id = candidates.id
 AND row_version.changed_at < $2
 AND (row_version.changed_at, row_version.id) < ($3, $4)
 AND (age(row_version.xmin) > age($6::xid8::xid)
      OR age(row_version.xmin) < 0)`

// Probe only each supported table's first eligible pair, using the same index as deletion.
const cleanupNextTableQuery = `SELECT table_name FROM (
 (SELECT table_name, changed_at, id FROM datastore_changelog
  WHERE table_name = 'health_events' AND changed_at < $1 AND (changed_at, id) < ($2, $3)
  ORDER BY changed_at, id LIMIT 1)
 UNION ALL
 (SELECT table_name, changed_at, id FROM datastore_changelog
  WHERE table_name = 'maintenance_events' AND changed_at < $1 AND (changed_at, id) < ($2, $3)
  ORDER BY changed_at, id LIMIT 1)
) AS heads ORDER BY changed_at, id LIMIT 1`

var errCleanupGuard = errors.New("cleanup horizon outside safe xid window")

type changelogCleaner struct {
	db     *sql.DB
	config cleanupConfig
	cancel context.CancelFunc
	done   chan struct{}
}

func startChangelogCleaner(ctx context.Context, db *sql.DB, options map[string]string) *changelogCleaner {
	config := loadCleanupConfig(options)
	if config.retention == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	c := &changelogCleaner{db: db, config: config, cancel: cancel, done: make(chan struct{})}
	go c.loop(ctx)

	return c
}

func (c *changelogCleaner) loop(ctx context.Context) {
	defer close(c.done)

	ticker := time.NewTicker(c.config.interval)
	defer ticker.Stop()

	for {
		if _, err := c.run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("PostgreSQL changelog cleanup failed; ending run", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *changelogCleaner) stop(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.cancel()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for changelog cleaner: %w", ctx.Err())
	}
}

func releaseCleanupLock(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var released bool

	err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", cleanupLockKey).Scan(&released)
	if err != nil || !released {
		slog.Warn("Releasing changelog cleanup lock failed; discarding session", "error", err)
		discardCleanupSession(conn)
	}
}

func (c *changelogCleaner) run(ctx context.Context) (int64, error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve cleanup connection: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", cleanupLockKey).Scan(&locked); err != nil {
		// An interrupted lock query may have acquired the lock before its result was lost.
		discardCleanupSession(conn)
		return 0, fmt.Errorf("take cleanup lock: %w", err)
	}

	if !locked {
		return 0, nil
	}

	defer releaseCleanupLock(conn)

	var ready bool
	if err := conn.QueryRowContext(ctx, cleanupPrerequisitesQuery).Scan(&ready); err != nil {
		return 0, fmt.Errorf("check cleanup prerequisites: %w", err)
	}

	if !ready {
		slog.Warn("Changelog cleanup requires schema and valid idx_changelog_resume_all; run store setup or provision them")
		return 0, nil
	}

	previous, now, usable, err := c.advanceGuard(ctx, conn)
	if err != nil || !usable {
		return 0, err
	}

	bound, err := cleanupConsumerBound(ctx, conn, now, now.Add(-c.config.retention))
	if err != nil || bound == nil {
		return 0, err
	}

	return c.deleteBatches(ctx, conn, previous, now.Add(-c.config.retention), *bound)
}

// Persist the next horizon before deleting, while retaining the previous horizon for every batch.
func (c *changelogCleaner) advanceGuard(ctx context.Context, conn *sql.Conn) (string, time.Time, bool, error) {
	var now time.Time
	if err := conn.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return "", now, false, fmt.Errorf("read cleanup database clock: %w", err)
	}

	var (
		previous string
		observed time.Time
	)

	err := conn.QueryRowContext(ctx, "SELECT horizon::text, observed_at FROM changelog_cleanup_state WHERE singleton").
		Scan(&previous, &observed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", now, false, fmt.Errorf("read previous cleanup guard: %w", err)
	}

	valid := err == nil && validCleanupGuard(previous, observed, now)
	if valid && now.Sub(observed) < c.config.interval {
		return "", now, false, nil
	}

	if _, err := conn.ExecContext(ctx, saveCleanupGuardQuery); err != nil {
		return "", now, false, fmt.Errorf("save next cleanup guard: %w", err)
	}

	if !valid {
		slog.Warn("Changelog cleanup seeded snapshot guard; deferring deletion")
	}

	return previous, now, valid, nil
}

type cleanupPosition struct {
	timestamp time.Time
	id        int64
}

type cleanupToken struct {
	client  string
	raw     []byte
	updated sql.NullTime
}

// The bound is sampled before batch snapshots. A concurrent late-NOTIFY rewind is safe
// because its newly committed row remains protected by the previous-snapshot xmin guard.
// Token resets require the operator rollout gate; a nonempty set cannot certify completeness.
func cleanupConsumerBound(ctx context.Context, conn *sql.Conn, now, cutoff time.Time) (*cleanupPosition, error) {
	tokens, err := readCleanupTokens(ctx, conn)
	if err != nil {
		return nil, err
	}

	var oldest *cleanupPosition

	for _, token := range tokens {
		position, err := parseCleanupPosition(ctx, conn, token)
		if err != nil {
			slog.Warn("Changelog cleanup blocked by unreadable consumer position", "client", token.client, "error", err)
			return nil, err
		}

		if err := validateCleanupPosition(token, position, now, cutoff); err != nil {
			return nil, err
		}

		if oldest == nil || position.before(*oldest) {
			oldest = &position
		}
	}

	if oldest == nil {
		slog.Warn("Changelog cleanup deferred: no saved consumer positions")
	}

	return oldest, nil
}

func validateCleanupPosition(token cleanupToken, position cleanupPosition, now, cutoff time.Time) error {
	// Both values use the database clock. Allow only one second for concurrent token saves
	// and small clock adjustments; larger skew cannot certify an acknowledged position.
	if position.timestamp.After(now.Add(time.Second)) {
		slog.Warn("Changelog cleanup blocked by future consumer position", "client", token.client,
			"timestamp", position.timestamp, "database_time", now)

		return fmt.Errorf("future cleanup timestamp for client %s", token.client)
	}

	if !token.updated.Valid || token.updated.Time.Before(cutoff) {
		slog.Warn("Stale changelog cleanup token remains protected; "+
			"quiet consumers need acknowledgments until idle checkpoints are implemented",
			"client", token.client, "timestamp", position.timestamp)
	}

	if !position.timestamp.After(time.Unix(0, 0)) {
		slog.Warn("Changelog cleanup blocked by full-replay consumer position; "+
			"requires an acknowledgment until idle checkpoints are implemented",
			"client", token.client)
	}

	return nil
}

func validCleanupGuard(previous string, observed, now time.Time) bool {
	horizon, err := strconv.ParseUint(previous, 10, 64)
	return err == nil && horizon >= 3 && !observed.IsZero() && !observed.After(now)
}

func readCleanupTokens(ctx context.Context, conn *sql.Conn) ([]cleanupToken, error) {
	rows, err := conn.QueryContext(ctx, "SELECT client_name, resume_token, last_updated FROM resume_tokens")
	if err != nil {
		return nil, fmt.Errorf("read cleanup consumer tokens: %w", err)
	}
	// Close the cursor before legacy lookups on this same reserved connection.
	var tokens []cleanupToken

	for rows.Next() {
		var token cleanupToken
		if err := rows.Scan(&token.client, &token.raw, &token.updated); err != nil {
			return nil, fmt.Errorf("scan cleanup consumer token: %w", errors.Join(err, rows.Close()))
		}

		tokens = append(tokens, token)
	}

	readErr := rows.Err()

	closeErr := rows.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("finish reading cleanup tokens: %w", err)
	}

	return tokens, nil
}

func parseCleanupPosition(ctx context.Context, conn *sql.Conn, token cleanupToken) (cleanupPosition, error) {
	var decoded struct {
		Timestamp *time.Time `json:"timestamp"`
		EventID   *int64     `json:"eventID"`
	}
	if err := json.Unmarshal(token.raw, &decoded); err != nil {
		return cleanupPosition{}, fmt.Errorf("parse cleanup token for client %s: %w", token.client, err)
	}

	if decoded.EventID == nil || *decoded.EventID < 0 {
		return cleanupPosition{}, fmt.Errorf("invalid cleanup eventID for client %s", token.client)
	}

	position := cleanupPosition{id: *decoded.EventID}
	if decoded.Timestamp != nil {
		position.timestamp = *decoded.Timestamp
	} else if position.id > 0 {
		if err := conn.QueryRowContext(ctx, "SELECT changed_at FROM datastore_changelog WHERE id = $1", position.id).
			Scan(&position.timestamp); err != nil {
			return cleanupPosition{}, fmt.Errorf("resolve legacy cleanup token for client %s: %w", token.client, err)
		}
	}

	if position.timestamp.IsZero() {
		return cleanupPosition{}, fmt.Errorf("missing cleanup timestamp for client %s", token.client)
	}

	return position, nil
}

func (c *changelogCleaner) deleteBatches(
	ctx context.Context, conn *sql.Conn, horizon string, cutoff time.Time, bound cleanupPosition,
) (int64, error) {
	var total int64

	for range c.config.maxBatches {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("cleanup cancelled: %w", err)
		}
		// Two one-row index reads choose the oldest table prefix without an unbounded scan.
		// A short or protected batch ends the whole run, so a later run must also visit maintenance rows.
		var table string

		err := conn.QueryRowContext(ctx, cleanupNextTableQuery, cutoff, bound.timestamp, bound.id).Scan(&table)
		if errors.Is(err, sql.ErrNoRows) {
			return total, nil
		}

		if err != nil {
			return total, fmt.Errorf("find cleanup candidate table: %w", err)
		}

		deleted, err := c.deleteBatch(ctx, conn, table, horizon, cutoff, bound)
		if err != nil {
			return total, err
		}

		total += deleted
		if deleted < int64(c.config.batchSize) {
			return total, nil
		}
	}

	return total, nil
}

func (c *changelogCleaner) deleteBatch(
	ctx context.Context, conn *sql.Conn, table, horizon string, cutoff time.Time, bound cleanupPosition,
) (int64, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, fmt.Errorf("begin cleanup batch: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Warn("Rollback cleanup batch failed", "error", err)
		}
	}()

	if err := validateCleanupHorizon(ctx, tx, horizon); err != nil {
		return 0, err
	}

	result, err := tx.ExecContext(ctx, cleanupDeleteQuery,
		table, cutoff, bound.timestamp, bound.id, c.config.batchSize, horizon)
	if err != nil {
		return 0, fmt.Errorf("delete changelog batch: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count changelog batch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit changelog batch: %w", err)
	}

	return deleted, nil
}

func validateCleanupHorizon(ctx context.Context, tx *sql.Tx, horizon string) error {
	var anchor uint64
	if err := tx.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&anchor); err != nil {
		return fmt.Errorf("assign cleanup batch xid: %w", err)
	}

	previous, err := strconv.ParseUint(horizon, 10, 64)
	if err != nil || previous < 3 || previous > anchor || anchor-previous >= 1<<31 {
		return errCleanupGuard
	}

	return nil
}

func (p cleanupPosition) before(other cleanupPosition) bool {
	return p.timestamp.Before(other.timestamp) || (p.timestamp.Equal(other.timestamp) && p.id < other.id)
}

func discardCleanupSession(conn *sql.Conn) {
	// Conn.Close returns a live session to the pool. ErrBadConn discards it and its session locks.
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
		slog.Warn("Discarding cleanup session failed", "error", err)
	}
}

// Provision only from createTables, whose caller holds the existing setup lock.
func provisionCleanupState(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, createCleanupStateStatement); err != nil {
		slog.Warn("Failed to provision optional changelog cleanup state; operator provisioning required", "error", err)
	}
}
