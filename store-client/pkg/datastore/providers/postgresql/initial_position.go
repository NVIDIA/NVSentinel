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

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type resetResumeTokenFunc func(
	context.Context, client.DatabaseClient, client.TokenConfig,
) (client.ResumeControlDecision, error)

const loadResumeTokenQuery = `SELECT resume_token FROM resume_tokens WHERE client_name = $1`

// initializeResumePosition prepares every error-returning construction path before reading.
// This correctness invariant applies regardless of whether changelog retention is enabled.
func (w *PostgreSQLChangeStreamWatcher) initializeResumePosition(ctx context.Context) error {
	tokenJSON, err := w.loadOrCreateInitialToken(ctx)
	if err != nil {
		return fmt.Errorf("initialize resume token for %s: %w", w.clientName, err)
	}

	// Release the reserved connection before legacy lookups, including when the pool has size one.
	if err := w.restoreResumePosition(ctx, tokenJSON); err != nil {
		return fmt.Errorf("restore initial position for %s: %w", w.clientName, err)
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

	// The database clock is evaluated in the insert, on this connection. DO NOTHING
	// preserves a concurrent constructor's winning pair and its last_updated value.
	const insert = `INSERT INTO resume_tokens (client_name, resume_token, last_updated)
		VALUES ($1, jsonb_build_object('timestamp', clock_timestamp(), 'eventID', 0), statement_timestamp())
		ON CONFLICT (client_name) DO NOTHING`

	if _, err := conn.ExecContext(ctx, insert, w.clientName); err != nil {
		return nil, fmt.Errorf("persist database-clock start position: %w", err)
	}

	// A separate statement sees the committed winner after ON CONFLICT waits.
	// Never use a locally proposed pair when another constructor inserted first.
	if err := conn.QueryRowContext(ctx, loadResumeTokenQuery, w.clientName).Scan(&tokenJSON); err != nil {
		return nil, fmt.Errorf("read persisted start position: %w", err)
	}

	return tokenJSON, nil
}
