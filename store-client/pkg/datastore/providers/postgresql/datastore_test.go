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
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestNewPostgreSQLStore(t *testing.T) {
	tests := []struct {
		name        string
		config      datastore.DataStoreConfig
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid config",
			config: datastore.DataStoreConfig{
				Provider: datastore.ProviderPostgreSQL,
				Connection: datastore.ConnectionConfig{
					Host:     "localhost",
					Port:     5432,
					Database: "test",
					Username: "testuser",
					SSLMode:  "disable",
				},
			},
			expectError: false,
		},
		{
			name: "missing host",
			config: datastore.DataStoreConfig{
				Provider: datastore.ProviderPostgreSQL,
				Connection: datastore.ConnectionConfig{
					Port:     5432,
					Database: "test",
					Username: "testuser",
				},
			},
			expectError: true,
			errorMsg:    "host is required",
		},
		{
			name: "missing database",
			config: datastore.DataStoreConfig{
				Provider: datastore.ProviderPostgreSQL,
				Connection: datastore.ConnectionConfig{
					Host:     "localhost",
					Port:     5432,
					Username: "testuser",
				},
			},
			expectError: true,
			errorMsg:    "database is required",
		},
		{
			name: "missing username",
			config: datastore.DataStoreConfig{
				Provider: datastore.ProviderPostgreSQL,
				Connection: datastore.ConnectionConfig{
					Host:     "localhost",
					Port:     5432,
					Database: "test",
				},
			},
			expectError: true,
			errorMsg:    "username is required",
		},
		{
			name: "invalid port",
			config: datastore.DataStoreConfig{
				Provider: datastore.ProviderPostgreSQL,
				Connection: datastore.ConnectionConfig{
					Host:     "localhost",
					Port:     -1,
					Database: "test",
					Username: "testuser",
				},
			},
			expectError: true,
			errorMsg:    "port must be between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip "valid config" test unless PostgreSQL is available
			if tt.name == "valid config" {
				t.Skip("Skipping test that requires PostgreSQL database - runs in integration tests")
			}

			ds, err := NewPostgreSQLStore(context.Background(), tt.config)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
				assert.Nil(t, ds)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, ds)
				assert.IsType(t, &PostgreSQLDataStore{}, ds)
			}
		})
	}
}

func TestBuildConnectionString(t *testing.T) {
	tests := []struct {
		name     string
		config   datastore.ConnectionConfig
		expected string
	}{
		{
			name: "basic connection",
			config: datastore.ConnectionConfig{
				Host:     "localhost",
				Port:     5432,
				Database: "test",
				Username: "testuser",
				Password: "testpass",
				SSLMode:  "disable",
			},
			expected: "host=localhost port=5432 dbname=test user=testuser password=testpass sslmode=disable",
		},
		{
			name: "with SSL certificates",
			config: datastore.ConnectionConfig{
				Host:        "localhost",
				Port:        5432,
				Database:    "test",
				Username:    "testuser",
				SSLMode:     "require",
				SSLCert:     "/path/to/cert.crt",
				SSLKey:      "/path/to/key.key",
				SSLRootCert: "/path/to/ca.crt",
			},
			expected: "host=localhost port=5432 dbname=test user=testuser sslmode=require sslcert=/path/to/cert.crt sslkey=/path/to/key.key sslrootcert=/path/to/ca.crt",
		},
		{
			name: "default SSL mode",
			config: datastore.ConnectionConfig{
				Host:     "localhost",
				Port:     5432,
				Database: "test",
				Username: "testuser",
			},
			expected: "host=localhost port=5432 dbname=test user=testuser sslmode=prefer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildConnectionString(tt.config)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPostgreSQLDataStore_Close(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	ds := &PostgreSQLDataStore{db: db}

	mock.ExpectClose()

	err = ds.Close(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgreSQLDataStore_RuntimeUpgrades_RunInBackgroundAndStopOnClose(t *testing.T) {
	originalIndexes := runtimeUpgradeIndexes
	runtimeUpgradeIndexes = []runtimeUpgradeIndex{{
		name:            "delayed_index",
		createStatement: "delayed concurrent index",
	}}
	t.Cleanup(func() { runtimeUpgradeIndexes = originalIndexes })

	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	mock.ExpectQuery(regexp.QuoteMeta(runtimeUpgradeIndexValidityQuery)).
		WithArgs("delayed_index").
		WillDelayFor(5 * time.Second).
		WillReturnError(sql.ErrNoRows)

	ds := &PostgreSQLDataStore{db: db}
	startedAt := time.Now()
	ds.startRuntimeUpgrades()
	require.Less(t, time.Since(startedAt), time.Second)

	require.Eventually(t, func() bool {
		return mock.ExpectationsWereMet() == nil
	}, time.Second, 10*time.Millisecond)
	mock.ExpectClose()
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, ds.Close(closeCtx))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgreSQLDataStore_Ping(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer db.Close()

	ds := &PostgreSQLDataStore{db: db}

	tests := []struct {
		name        string
		setupMock   func()
		expectError bool
	}{
		{
			name: "successful ping",
			setupMock: func() {
				mock.ExpectPing().WillReturnError(nil)
			},
			expectError: false,
		},
		{
			name: "ping fails",
			setupMock: func() {
				mock.ExpectPing().WillReturnError(fmt.Errorf("connection lost"))
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupMock()

			err := ds.Ping(context.Background())

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRecoveryIndexesIncludePartialPendingEventCursor(t *testing.T) {
	var pendingIndex string
	for _, statement := range recoveryIndexStatements {
		if strings.Contains(statement, "idx_health_events_fault_quarantine_pending") {
			pendingIndex = statement

			break
		}
	}

	require.NotEmpty(t, pendingIndex)
	assert.Contains(t, pendingIndex, "ON health_events (created_at, id) WHERE")
	assert.Contains(t, pendingIndex,
		"COALESCE(document->'healtheventstatus'->>'nodequarantined', document->'healtheventstatus'->>'nodeQuarantined')")
	assert.Contains(t, pendingIndex, "= 'NotStarted'")
	assert.Contains(t, pendingIndex,
		"document->'healtheventstatus'->>'faultquarantinerecovery' IS NULL")
}

func TestRuntimeUpgradeIndexes_ExistingDatabase_IncludesAnalyzerLookup(t *testing.T) {
	var analyzerIndex runtimeUpgradeIndex
	for _, index := range runtimeUpgradeIndexes {
		if index.name == "idx_health_events_analyzer_lookup" {
			analyzerIndex = index

			break
		}
	}

	require.NotEmpty(t, analyzerIndex.name)
	assert.Contains(t, analyzerIndex.createStatement, "CREATE INDEX CONCURRENTLY IF NOT EXISTS")
	assert.Contains(t, analyzerIndex.createStatement, "ON health_events (node_name, event_type, created_at DESC")
	assert.Contains(t, analyzerIndex.createStatement, "document->'healthevent'->>'agent'")
	assert.Equal(t, "DROP INDEX CONCURRENTLY IF EXISTS idx_health_events_analyzer_lookup", analyzerIndex.dropStatement)
}

func TestRunRuntimeUpgrades_ConcurrentIndex_ExecutesOutsideTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	for _, index := range runtimeUpgradeIndexes {
		mock.ExpectQuery(regexp.QuoteMeta(runtimeUpgradeIndexValidityQuery)).
			WithArgs(index.name).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectExec(regexp.QuoteMeta(index.createStatement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}

	runRuntimeUpgrades(context.Background(), db)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunRuntimeUpgrades_OneStatementFails_ContinuesRemainingStatements(t *testing.T) {
	originalIndexes := runtimeUpgradeIndexes
	runtimeUpgradeIndexes = []runtimeUpgradeIndex{
		{name: "first_index", createStatement: "first concurrent index"},
		{name: "second_index", createStatement: "second concurrent index"},
	}
	t.Cleanup(func() { runtimeUpgradeIndexes = originalIndexes })

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	for _, index := range runtimeUpgradeIndexes {
		mock.ExpectQuery(regexp.QuoteMeta(runtimeUpgradeIndexValidityQuery)).
			WithArgs(index.name).
			WillReturnError(sql.ErrNoRows)
		if index.name == "first_index" {
			mock.ExpectExec(regexp.QuoteMeta(index.createStatement)).WillReturnError(assert.AnError)
		} else {
			mock.ExpectExec(regexp.QuoteMeta(index.createStatement)).WillReturnResult(sqlmock.NewResult(0, 0))
		}
	}

	runRuntimeUpgrades(context.Background(), db)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunRuntimeUpgrades_InvalidIndex_RecreatesConcurrently(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	index := runtimeUpgradeIndexes[0]
	mock.ExpectQuery(regexp.QuoteMeta(runtimeUpgradeIndexValidityQuery)).
		WithArgs(index.name).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(false))
	mock.ExpectExec(regexp.QuoteMeta(index.dropStatement)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(index.createStatement)).WillReturnResult(sqlmock.NewResult(0, 0))

	runRuntimeUpgrades(context.Background(), db)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunRuntimeUpgrades_ValidIndex_SkipsMigration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	index := runtimeUpgradeIndexes[0]
	mock.ExpectQuery(regexp.QuoteMeta(runtimeUpgradeIndexValidityQuery)).
		WithArgs(index.name).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))
	// sqlmock has no negative expectations. Queue both destructive statements
	// and require them to remain unmet so removing the valid-index guard fails
	// this test instead of being hidden by the helper's best-effort error path.
	mock.ExpectExec(regexp.QuoteMeta(index.dropStatement)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(index.createStatement)).WillReturnResult(sqlmock.NewResult(0, 0))

	runRuntimeUpgrades(context.Background(), db)
	expectationsErr := mock.ExpectationsWereMet()
	require.ErrorContains(t, expectationsErr, "ExpectedExec")
}

func TestPostgreSQLDataStore_Provider(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ds := &PostgreSQLDataStore{db: db}

	provider := ds.Provider()
	assert.Equal(t, datastore.ProviderPostgreSQL, provider)
}

func TestPostgreSQLDataStore_MaintenanceEventStore(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	maintenanceStore := NewPostgreSQLMaintenanceEventStore(db)
	ds := &PostgreSQLDataStore{
		db:                    db,
		maintenanceEventStore: maintenanceStore,
	}

	store := ds.MaintenanceEventStore()
	assert.NotNil(t, store)
	assert.IsType(t, &PostgreSQLMaintenanceEventStore{}, store)
}

func TestPostgreSQLDataStore_HealthEventStore(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	healthStore := NewPostgreSQLHealthEventStore(db)
	ds := &PostgreSQLDataStore{
		db:               db,
		healthEventStore: healthStore,
	}

	store := ds.HealthEventStore()
	assert.NotNil(t, store)
	assert.IsType(t, &PostgreSQLHealthEventStore{}, store)
}

// TestCreateChangeTriggers_MissingTriggers_CreatedRaceSafely verifies that
// trigger creation SQL handles concurrent duplicate creation.
func TestCreateChangeTriggers_MissingTriggers_CreatedRaceSafely(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`(?s)CREATE OR REPLACE FUNCTION log_table_changes\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(
		`(?s)DO \$\$.*IF NOT EXISTS.*tgname = 'maintenance_events_changes'.*` +
			`CREATE TRIGGER maintenance_events_changes.*EXCEPTION.*` +
			`WHEN duplicate_object THEN.*NULL`,
	).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(
		`(?s)DO \$\$.*IF NOT EXISTS.*tgname = 'health_events_changes'.*` +
			`CREATE TRIGGER health_events_changes.*EXCEPTION.*` +
			`WHEN duplicate_object THEN.*NULL`,
	).WillReturnResult(sqlmock.NewResult(0, 0))

	require.NoError(t, createChangeTriggers(context.Background(), db))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestEnsureIdempotencyIndex: the table setup builds the index concurrently
// when it is missing, leaves a build another session is running alone,
// replaces a leftover or a different definition, and reports a build that
// did not end valid.
func TestEnsureIdempotencyIndex(t *testing.T) {
	createStatement := regexp.QuoteMeta(client.CreateIdempotencyIndexStatement)
	dropStatement := regexp.QuoteMeta(client.DropIdempotencyIndexStatement)
	verifyQuery := "SELECT i.indisunique"
	progressQuery := "pg_stat_progress_create_index"
	verifyColumns := []string{"indisunique", "indisvalid", "indnatts", "indexdef", "predicate"}
	validIndexDef := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
		"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))) " +
		"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	validPredicate := "((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"

	expectIndex := func(mock sqlmock.Sqlmock, valid bool) {
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
			WillReturnRows(sqlmock.NewRows(verifyColumns).AddRow(true, valid, 1, validIndexDef, validPredicate))
	}
	expectMissing := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
			WillReturnRows(sqlmock.NewRows(verifyColumns))
	}
	expectBuilding := func(mock sqlmock.Sqlmock, building bool) {
		mock.ExpectQuery(progressQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
			WillReturnRows(sqlmock.NewRows([]string{"building"}).AddRow(building))
	}
	newDB := func(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
		t.Helper()

		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		return db, mock
	}

	t.Run("missing index is built concurrently and verified", func(t *testing.T) {
		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a correct index is left alone", func(t *testing.T) {
		db, mock := newDB(t)
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a build another session is running is left alone", func(t *testing.T) {
		db, mock := newDB(t)
		expectIndex(mock, false)
		expectBuilding(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("an invalid leftover is dropped and built again", func(t *testing.T) {
		db, mock := newDB(t)
		expectIndex(mock, false)
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a valid index with another definition is dropped and built again", func(t *testing.T) {
		db, mock := newDB(t)
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
			WillReturnRows(sqlmock.NewRows(verifyColumns).AddRow(true, true, 1,
				strings.Replace(validIndexDef, "IS NOT NULL)", "IS NOT NULL AND (node_name = 'node-a'::text))", 1),
				"(((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL) AND (node_name = 'node-a'::text))"))
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a failed drop is reported and no create follows", func(t *testing.T) {
		db, mock := newDB(t)
		expectIndex(mock, false)
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnError(errors.New("lock timeout"))

		err := ensureIdempotencyIndex(context.Background(), db)
		require.ErrorContains(t, err, "dropping the mismatched idempotency index")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("documents sharing a key stop the build and name the key", func(t *testing.T) {
		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnError(&pq.Error{
			Code:   pqerror.UniqueViolation,
			Detail: "Key ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))=(dup-2) is duplicated.",
		})

		err := ensureIdempotencyIndex(context.Background(), db)
		require.ErrorContains(t, err, "share an idempotency key")
		require.ErrorContains(t, err, "dup-2")
		require.ErrorContains(t, err, "GROUP BY 1 HAVING count(*) > 1")
		assert.NoError(t, mock.ExpectationsWereMet(), "no retry: the next build would fail the same way")
	})

	t.Run("a build that did not end valid is reported", func(t *testing.T) {
		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, false)

		err := ensureIdempotencyIndex(context.Background(), db)
		require.Error(t, err)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a failed check is reported", func(t *testing.T) {
		db, mock := newDB(t)
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTable).
			WillReturnError(errors.New("connection refused"))

		err := ensureIdempotencyIndex(context.Background(), db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checking the idempotency index")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	// The components start together and their table setup takes strong locks
	// on the same table, so PostgreSQL may abort the CONCURRENTLY build as a
	// deadlock victim; the build is then tried again without a wait here.
	noRetryDelay := func(t *testing.T) {
		t.Helper()

		previous := idempotencyIndexRetryDelay
		idempotencyIndexRetryDelay = 0

		t.Cleanup(func() { idempotencyIndexRetryDelay = previous })
	}
	deadlock := &pq.Error{Code: pqerror.TRDeadlockDetected, Message: "deadlock detected"}

	t.Run("a build aborted as deadlock victim drops its leftover and is built again", func(t *testing.T) {
		noRetryDelay(t)

		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnError(deadlock)
		expectIndex(mock, false)
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a deadlock victim finds the index another session finished", func(t *testing.T) {
		noRetryDelay(t)

		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnError(deadlock)
		expectIndex(mock, true)

		require.NoError(t, ensureIdempotencyIndex(context.Background(), db))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a build that keeps losing is reported after the last attempt", func(t *testing.T) {
		noRetryDelay(t)

		db, mock := newDB(t)

		for range idempotencyIndexBuildAttempts {
			expectMissing(mock)
			mock.ExpectExec(createStatement).WillReturnError(deadlock)
		}

		err := ensureIdempotencyIndex(context.Background(), db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "building the idempotency index")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a build that fails for another reason is not tried again", func(t *testing.T) {
		noRetryDelay(t)

		db, mock := newDB(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnError(errors.New("disk full"))

		err := ensureIdempotencyIndex(context.Background(), db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "disk full")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestWithSetupLock: the table setup runs once the advisory lock is held,
// waiting while another component holds it, and releases the lock afterwards,
// also when the setup fails.
func TestWithSetupLock(t *testing.T) {
	setupLockPoll = time.Millisecond

	t.Cleanup(func() { setupLockPoll = time.Second })

	lockQuery := "SELECT pg_try_advisory_lock"
	unlockQuery := "SELECT pg_advisory_unlock"
	locked := func(v bool) *sqlmock.Rows { return sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(v) }

	t.Run("waits for the lock, runs the setup, releases the lock", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		mock.ExpectQuery(lockQuery).WithArgs(setupLockKey).WillReturnRows(locked(false))
		mock.ExpectQuery(lockQuery).WithArgs(setupLockKey).WillReturnRows(locked(true))
		mock.ExpectExec(unlockQuery).WithArgs(setupLockKey).WillReturnResult(sqlmock.NewResult(0, 0))

		ran := false
		require.NoError(t, withSetupLock(context.Background(), db, func() error { ran = true; return nil }))
		assert.True(t, ran)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a failed setup still releases the lock", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		mock.ExpectQuery(lockQuery).WithArgs(setupLockKey).WillReturnRows(locked(true))
		mock.ExpectExec(unlockQuery).WithArgs(setupLockKey).WillReturnResult(sqlmock.NewResult(0, 0))

		err = withSetupLock(context.Background(), db, func() error { return errors.New("setup failed") })
		require.ErrorContains(t, err, "setup failed")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("gives up with the context while waiting", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		mock.ExpectQuery(lockQuery).WithArgs(setupLockKey).WillReturnRows(locked(false))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err = withSetupLock(ctx, db, func() error { t.Fatal("setup must not run"); return nil })
		require.ErrorIs(t, err, context.Canceled)
	})
}
