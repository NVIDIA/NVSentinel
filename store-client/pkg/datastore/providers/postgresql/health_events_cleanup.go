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
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// Health events retention deletes the health_events rows older than the retention, as the
// MongoDB TTL index does for the HealthEvents collection. Like the TTL, it keys on the insert
// time only: an event still in progress when it expires is deleted too.

const healthEventsCleanupLockKey int64 = 2026112401

// Keep at least a day of events, as the changelog retention does.
const minimumHealthEventsRetention = 24 * time.Hour

const minimumHealthEventsCleanupInterval = time.Minute

// The batches walk idx_health_events_created_id and remove their changelog rows through
// idx_changelog_table_record. Without them each batch would scan a whole table.
const healthEventsCleanupPrerequisitesQuery = `SELECT
 to_regclass('health_events') IS NOT NULL
 AND to_regclass('datastore_changelog') IS NOT NULL
 AND EXISTS (SELECT 1 FROM pg_index i
 WHERE i.indexrelid = to_regclass('idx_health_events_created_id')
 AND i.indrelid = to_regclass('health_events') AND i.indisvalid)
 AND EXISTS (SELECT 1 FROM pg_index i
 WHERE i.indexrelid = to_regclass('idx_changelog_table_record')
 AND i.indrelid = to_regclass('datastore_changelog') AND i.indisvalid)`

// A batch picks its rows from idx_health_events_created_id and deletes them by primary key, so
// it reads only its own rows however large the expired backlog is. A join on the candidates
// lets the planner hash every expired row for each batch.
const healthEventsDeleteQuery = `DELETE FROM health_events
WHERE id = ANY (ARRAY(
 SELECT id FROM health_events
 WHERE created_at < $1
 ORDER BY created_at, id LIMIT $2
))
RETURNING id::text`

// The health_events trigger logs every delete to the changelog with a copy of the row. No
// consumer acts on a delete, and nothing cleans the changelog yet (#2170), so keeping those
// rows would only move the expired events into datastore_changelog. They are removed in the
// transaction that wrote them, so no consumer can read them.
const healthEventsDeleteChangesQuery = `DELETE FROM datastore_changelog
WHERE table_name = 'health_events' AND operation = 'DELETE' AND record_id = ANY($1::uuid[])`

type healthEventsCleanupConfig struct {
	retention  time.Duration
	interval   time.Duration
	batchSize  int
	maxBatches int
}

// Resolve once per store. Invalid settings disable the cleanup and never prevent ingestion.
func loadHealthEventsCleanupConfig(options map[string]string) healthEventsCleanupConfig {
	rawRetention := cleanupConfigValue(options,
		"healthEventsRetention", "DATASTORE_HEALTH_EVENTS_RETENTION", "0")

	retention, err := time.ParseDuration(rawRetention)
	if err == nil && retention == 0 {
		return healthEventsCleanupConfig{}
	}

	if err != nil || retention < minimumHealthEventsRetention {
		slog.Warn("Invalid health events retention; cleanup disabled",
			"value", rawRetention, "minimum", minimumHealthEventsRetention, "error", err)

		return healthEventsCleanupConfig{}
	}

	rawInterval := cleanupConfigValue(options,
		"healthEventsCleanupInterval", "DATASTORE_HEALTH_EVENTS_CLEANUP_INTERVAL", "1h")

	interval, err := time.ParseDuration(rawInterval)
	if err != nil || interval < minimumHealthEventsCleanupInterval {
		slog.Warn("Invalid health events cleanup interval; cleanup disabled",
			"value", rawInterval, "minimum", minimumHealthEventsCleanupInterval)

		return healthEventsCleanupConfig{}
	}

	rawSize := cleanupConfigValue(options,
		"healthEventsCleanupBatchSize", "DATASTORE_HEALTH_EVENTS_CLEANUP_BATCH_SIZE", "10000")
	size := cleanupPositiveInt(rawSize)

	rawBatches := cleanupConfigValue(options,
		"healthEventsCleanupMaxBatches", "DATASTORE_HEALTH_EVENTS_CLEANUP_MAX_BATCHES", "100")
	batches := cleanupPositiveInt(rawBatches)

	if size == 0 || batches == 0 {
		slog.Warn("Invalid health events cleanup batch limits; cleanup disabled",
			"batch_size", rawSize, "max_batches", rawBatches)

		return healthEventsCleanupConfig{}
	}

	return healthEventsCleanupConfig{retention: retention, interval: interval, batchSize: size, maxBatches: batches}
}

type healthEventsCleaner struct {
	db     *sql.DB
	config healthEventsCleanupConfig
	cancel context.CancelFunc
	done   chan struct{}
}

func startHealthEventsCleaner(ctx context.Context, db *sql.DB, options map[string]string) *healthEventsCleaner {
	config := loadHealthEventsCleanupConfig(options)
	if config.retention == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	c := &healthEventsCleaner{db: db, config: config, cancel: cancel, done: make(chan struct{})}
	go c.loop(ctx)

	slog.Info("PostgreSQL health events retention enabled",
		"retention", config.retention, "interval", config.interval)

	return c
}

func (c *healthEventsCleaner) loop(ctx context.Context) {
	defer close(c.done)

	ticker := time.NewTicker(c.config.interval)
	defer ticker.Stop()

	for {
		if _, err := c.run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("PostgreSQL health events cleanup failed; ending run", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// stop cancels the cleaner and waits for its run to end. The pool close that follows waits
// for any query still in progress, so the pool is closed even when this wait gives up.
func (c *healthEventsCleaner) stop(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.cancel()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for health events cleaner: %w", ctx.Err())
	}
}

// run deletes the expired events in batches, one run at a time across all store owners.
func (c *healthEventsCleaner) run(ctx context.Context) (int64, error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve health events cleanup connection: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", healthEventsCleanupLockKey).
		Scan(&locked); err != nil {
		// An interrupted lock query may have acquired the lock before its result was lost.
		discardCleanupSession(conn)
		return 0, fmt.Errorf("take health events cleanup lock: %w", err)
	}

	if !locked {
		return 0, nil
	}

	defer releaseCleanupLock(conn, healthEventsCleanupLockKey)

	var ready bool
	if err := conn.QueryRowContext(ctx, healthEventsCleanupPrerequisitesQuery).Scan(&ready); err != nil {
		return 0, fmt.Errorf("check health events cleanup prerequisites: %w", err)
	}

	if !ready {
		slog.Warn("Health events cleanup requires the schema and valid idx_health_events_created_id " +
			"and idx_changelog_table_record; run store setup or provision them")

		return 0, nil
	}

	var now time.Time
	if err := conn.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return 0, fmt.Errorf("read health events cleanup database clock: %w", err)
	}

	cutoff := now.Add(-c.config.retention)

	total, err := c.deleteBatches(ctx, conn, cutoff)
	if total > 0 {
		slog.Info("Deleted expired health events", "rows", total, "created_before", cutoff)
	}

	return total, err
}

func (c *healthEventsCleaner) deleteBatches(ctx context.Context, conn *sql.Conn, cutoff time.Time) (int64, error) {
	var total int64

	for range c.config.maxBatches {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("health events cleanup cancelled: %w", err)
		}

		deleted, err := c.deleteBatch(ctx, conn, cutoff)
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

// deleteBatch deletes one batch of expired events and the changelog rows its deletes wrote.
func (c *healthEventsCleaner) deleteBatch(ctx context.Context, conn *sql.Conn, cutoff time.Time) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin health events cleanup batch: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Warn("Rollback health events cleanup batch failed", "error", err)
		}
	}()

	ids, err := deleteExpiredHealthEvents(ctx, tx, cutoff, c.config.batchSize)
	if err != nil {
		return 0, err
	}

	if len(ids) == 0 {
		return 0, nil
	}

	if _, err := tx.ExecContext(ctx, healthEventsDeleteChangesQuery, pq.Array(ids)); err != nil {
		return 0, fmt.Errorf("delete changelog rows of expired health events: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit health events cleanup batch: %w", err)
	}

	return int64(len(ids)), nil
}

func deleteExpiredHealthEvents(ctx context.Context, tx *sql.Tx, cutoff time.Time, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, healthEventsDeleteQuery, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("delete expired health events: %w", err)
	}

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired health event id: %w", errors.Join(err, rows.Close()))
		}

		ids = append(ids, id)
	}

	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("finish deleting expired health events: %w", err)
	}

	return ids, nil
}
