// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"log/slog"
	"time"
)

// RequiredSchemaVersion is the minimum PostgreSQL schema version required by
// this store-client release. DDL is applied separately from the application by
// running the SQL files in distros/kubernetes/nvsentinel/files/postgresql-migrations
// in filename order; the chart's PostgreSQL setup Job does this by default.
const RequiredSchemaVersion int64 = 3

// Components start alongside the chart's migration Job, so startup waits a
// bounded time for the schema instead of crash-looping into long backoff.
var (
	schemaVersionWaitTimeout  = 5 * time.Minute
	schemaVersionPollInterval = 5 * time.Second
)

const currentSchemaVersionQuery = `
	SELECT COALESCE(MAX(version), 0)
	FROM nvsentinel_schema_migrations
`

// ValidateSchemaVersion verifies that the database has been migrated before an
// application starts using it. This check is read-only and requires no DDL
// privileges.
func ValidateSchemaVersion(ctx context.Context, db *sql.DB) error {
	var currentVersion int64

	if err := db.QueryRowContext(ctx, currentSchemaVersionQuery).Scan(&currentVersion); err != nil {
		return fmt.Errorf(
			"failed to read PostgreSQL schema version; apply the SQL files in "+
				"distros/kubernetes/nvsentinel/files/postgresql-migrations in filename order: %w",
			err,
		)
	}

	if currentVersion < RequiredSchemaVersion {
		return fmt.Errorf(
			"PostgreSQL schema version %d is older than required version %d; "+
				"apply the pending SQL migrations before starting NVSentinel",
			currentVersion,
			RequiredSchemaVersion,
		)
	}

	return nil
}

// WaitForSchemaVersion runs ValidateSchemaVersion until it succeeds, the wait
// timeout elapses, or ctx is cancelled. The wait timeout also bounds each
// query, so a blocked database cannot extend startup past it.
func WaitForSchemaVersion(ctx context.Context, db *sql.DB) error {
	waitCtx, cancel := context.WithTimeout(ctx, schemaVersionWaitTimeout)
	defer cancel()

	for {
		err := ValidateSchemaVersion(waitCtx, db)
		if err == nil {
			return nil
		}

		if waitCtx.Err() != nil {
			return schemaWaitError(ctx, err)
		}

		slog.Warn("PostgreSQL schema not ready, waiting for migrations",
			"error", err, "retryIn", schemaVersionPollInterval)

		select {
		case <-waitCtx.Done():
			return schemaWaitError(ctx, err)
		case <-time.After(schemaVersionPollInterval):
		}
	}
}

// schemaWaitError distinguishes cancellation by the caller from the wait
// timeout elapsing.
func schemaWaitError(ctx context.Context, lastErr error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("waiting for PostgreSQL schema: %w", ctx.Err())
	}

	return fmt.Errorf("PostgreSQL schema not ready after waiting %s: %w", schemaVersionWaitTimeout, lastErr)
}
