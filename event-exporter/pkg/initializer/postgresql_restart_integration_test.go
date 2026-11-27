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

//go:build integration

package initializer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/postgresql"
)

// Run with -tags=integration and NVSENTINEL_TEST_POSTGRES_DSN set to a disposable database.
func TestInitializeAll_PostgreSQLRestart_SendsOnlyNewEvents(t *testing.T) {
	dsn := os.Getenv("NVSENTINEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NVSENTINEL_TEST_POSTGRES_DSN to run the PostgreSQL restart test")
	}

	tests := []struct {
		name          string
		expiredLegacy bool
	}{
		{name: "timestamp and ID"},
		{name: "expired legacy ID", expiredLegacy: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			db := setupRestartPostgreSQL(t, ctx, dsn)
			params, deliveries := restartExporterParams(t)

			first, stopFirst := startRestartExporter(t, ctx, params)
			ready, ok := first.DatastoreBundle.ChangeStreamWatcher.(interface{ IsReady() bool })
			require.True(t, ok)
			require.Eventually(t, ready.IsReady, 5*time.Second, 20*time.Millisecond)

			oldID := insertRestartEvent(t, ctx, db, "before restart")
			waitForRestartCheckpoint(t, ctx, db, oldID)
			stopFirst()

			if tt.expiredLegacy {
				// Retain the health event for backfill, but expire the row referenced by the legacy token.
				_, err := db.ExecContext(ctx,
					"UPDATE resume_tokens SET resume_token = resume_token - 'timestamp' WHERE client_name = 'event-exporter'")
				require.NoError(t, err)

				_, err = db.ExecContext(ctx, "DELETE FROM datastore_changelog WHERE id = $1", oldID)
				require.NoError(t, err)
			}

			newID := insertRestartEvent(t, ctx, db, "during downtime")
			_, stopRestarted := startRestartExporter(t, ctx, params)
			waitForRestartCheckpoint(t, ctx, db, newID)
			stopRestarted()

			var received []string
			for len(deliveries) > 0 {
				received = append(received, <-deliveries)
			}

			require.Equal(t, []string{"before restart", "during downtime"}, received)
		})
	}
}

func setupRestartPostgreSQL(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()

	connection, err := pq.NewConfig(dsn)
	require.NoError(t, err)

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, admin.Close()) })

	var hasApplicationTables bool

	err = admin.QueryRowContext(ctx, `
		SELECT to_regclass('public.health_events') IS NOT NULL
			OR to_regclass('public.datastore_changelog') IS NOT NULL
			OR to_regclass('public.resume_tokens') IS NOT NULL`).Scan(&hasApplicationTables)
	require.NoError(t, err)
	require.False(t, hasApplicationTables, "use a disposable database without NVSentinel tables in public")

	schema := fmt.Sprintf("exporter_restart_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
		assert.NoError(t, err)
	})

	t.Setenv("PGOPTIONS", "-c search_path="+schema+",public")
	t.Setenv("DATASTORE_PROVIDER", "postgresql")
	t.Setenv("DATASTORE_HOST", connection.Host)
	t.Setenv("DATASTORE_PORT", strconv.Itoa(int(connection.Port)))
	t.Setenv("DATASTORE_DATABASE", connection.Database)
	t.Setenv("DATASTORE_USERNAME", connection.User)
	t.Setenv("DATASTORE_PASSWORD", connection.Password)
	t.Setenv("DATASTORE_SSLMODE", string(connection.SSLMode))
	t.Setenv("DATASTORE_SSLCERT", connection.SSLCert)
	t.Setenv("DATASTORE_SSLKEY", connection.SSLKey)
	t.Setenv("DATASTORE_SSLROOTCERT", connection.SSLRootCert)
	t.Setenv("DATASTORE_CHANGELOG_RETENTION", "")
	t.Setenv("MONGODB_DATABASE_NAME", connection.Database)
	t.Setenv("MONGODB_COLLECTION_NAME", "HealthEvents")
	t.Setenv("MONGODB_TOKEN_COLLECTION_NAME", "ResumeTokens")

	cfg, err := datastore.LoadDatastoreConfig()
	require.NoError(t, err)

	store, err := postgresql.NewPostgreSQLStore(ctx, *cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		assert.NoError(t, store.Close(cleanupCtx))
	})

	return store.(*postgresql.PostgreSQLDataStore).GetDB()
}

func restartExporterParams(t *testing.T) (Params, <-chan string) {
	t.Helper()

	deliveries := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			Data struct {
				HealthEvent struct {
					Message string
				}
			}
		}

		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		select {
		case deliveries <- event.Data.HealthEvent.Message:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	params := Params{
		ConfigPath:     filepath.Join(dir, "config.toml"),
		OIDCSecretPath: filepath.Join(dir, "oidc-secret"),
		Workers:        1,
	}

	config := fmt.Sprintf(`
[exporter.metadata]
cluster = "restart-test"
[exporter.sink]
endpoint = %q
timeout = "5s"
[exporter.oidc]
token_url = %q
client_id = "restart-test"
scope = "events:write"
[exporter.backfill]
enabled = true
max_age = "720h"
max_events = 10
[exporter.resume_token]
collection = "ResumeTokens"
database = "restart-test"
[exporter.failure_handling]
max_retries = 1
initial_backoff = "1ms"
max_backoff = "1ms"
backoff_multiplier = 2
`, server.URL+"/events", server.URL+"/token")
	require.NoError(t, os.WriteFile(params.ConfigPath, []byte(config), 0o600))
	require.NoError(t, os.WriteFile(params.OIDCSecretPath, []byte("test-secret"), 0o600))

	return params, deliveries
}

func startRestartExporter(t *testing.T, ctx context.Context, params Params) (*Components, func()) {
	t.Helper()

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	components, err := InitializeAll(runCtx, params)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- components.Exporter.Run(runCtx) }()

	stop := sync.OnceFunc(func() {
		cancel()

		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelClose()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				assert.NoError(t, err)
			}
		case <-closeCtx.Done():
			t.Error("exporter did not stop")
		}

		assert.NoError(t, components.DatastoreBundle.Close(closeCtx))
	})
	t.Cleanup(stop)

	return components, stop
}

func insertRestartEvent(t *testing.T, ctx context.Context, db *sql.DB, message string) int64 {
	t.Helper()

	err := postgresql.NewPostgreSQLHealthEventStore(db).InsertHealthEvents(ctx, &datastore.HealthEventWithStatus{
		CreatedAt:   time.Now().UTC(),
		HealthEvent: &pb.HealthEvent{NodeName: "restart-test-node", Message: message},
	})
	require.NoError(t, err)

	var id int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT max(id) FROM datastore_changelog").Scan(&id))

	return id
}

func waitForRestartCheckpoint(t *testing.T, ctx context.Context, db *sql.DB, wantID int64) {
	t.Helper()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var id int64

		err := db.QueryRowContext(ctx,
			"SELECT (resume_token->>'eventID')::bigint FROM resume_tokens WHERE client_name = 'event-exporter'").Scan(&id)
		if assert.NoError(c, err) {
			assert.Equal(c, wantID, id)
		}
	}, 5*time.Second, 20*time.Millisecond)
}
