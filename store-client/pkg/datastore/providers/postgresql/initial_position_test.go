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
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

// Deliberately far from the application clock, with PostgreSQL microsecond precision.
var initialDBTime = time.Date(2001, 2, 3, 4, 5, 6, 123456000, time.UTC)

const initialInsertPattern = `INSERT INTO resume_tokens \(client_name, resume_token, last_updated\) VALUES \(\$1, jsonb_build_object\('timestamp', clock_timestamp\(\), 'eventID', 0\), statement_timestamp\(\)\) ON CONFLICT \(client_name\) DO NOTHING`

type positionConstructor func(context.Context, *sql.DB, string) (*PostgreSQLChangeStreamWatcher, error)

func positionConstructors() map[string]positionConstructor {
	return map[string]positionConstructor{
		"factory": func(ctx context.Context, db *sql.DB, name string) (*PostgreSQLChangeStreamWatcher, error) {
			ds := &PostgreSQLDataStore{db: db, metricsRegisterer: prometheus.NewRegistry()}
			result, err := (&PostgreSQLWatcherFactory{}).CreateChangeStreamWatcher(ctx, ds,
				watcher.WatcherConfig{ClientName: name, CollectionName: "HealthEvents"})
			if err != nil {
				return nil, err
			}
			return result.(*PostgreSQLChangeStreamWatcherWithUnwrap).watcher, nil
		},
		"datastore": func(ctx context.Context, db *sql.DB, name string) (*PostgreSQLChangeStreamWatcher, error) {
			ds := &PostgreSQLDataStore{db: db, metricsRegisterer: prometheus.NewRegistry()}
			result, err := ds.NewChangeStreamWatcher(ctx, map[string]any{"ClientName": name, "TableName": "HealthEvents"})
			if err != nil {
				return nil, err
			}
			return result.(*PostgreSQLChangeStreamWatcherWithUnwrap).watcher, nil
		},
		"database-client": func(ctx context.Context, db *sql.DB, name string) (*PostgreSQLChangeStreamWatcher, error) {
			result, err := NewPostgreSQLDatabaseClient(db, "HealthEvents").NewChangeStreamWatcher(ctx,
				client.TokenConfig{ClientName: name}, nil)
			if err != nil {
				return nil, err
			}
			return result.(*PostgreSQLChangeStreamAdapter).watcher, nil
		},
	}
}

func positionToken(t *testing.T, ts time.Time, id int64) []byte {
	t.Helper()
	token, err := json.Marshal(map[string]any{"timestamp": ts.Format(time.RFC3339Nano), "eventID": id})
	require.NoError(t, err)
	return token
}

func expectPositionToken(mock sqlmock.Sqlmock, name string, token []byte) {
	mock.ExpectQuery(regexp.QuoteMeta(loadResumeTokenQuery)).WithArgs(name).
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).AddRow(token))
}

func expectInitialPosition(t *testing.T, mock sqlmock.Sqlmock, name string) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta(loadResumeTokenQuery)).WithArgs(name).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(initialInsertPattern).WithArgs(name).WillReturnResult(sqlmock.NewResult(0, 1))
	expectPositionToken(mock, name, positionToken(t, initialDBTime, 0))
}

func TestWatcherConstruction_InitialPosition_PersistsBeforeReturning(t *testing.T) {
	for path, construct := range positionConstructors() {
		t.Run(path, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			// event-exporter must skip reset configuration and still initialize on all paths.
			expectInitialPosition(t, mock, "event-exporter")
			w, err := construct(t.Context(), db, "event-exporter")
			require.NoError(t, err)
			require.True(t, w.positionInitialized)
			assert.Equal(t, initialDBTime, w.lastTimestamp)
			assert.Zero(t, w.lastEventID)
			assert.Empty(t, w.events)
			assert.Nil(t, w.listener)
			empty, read := w.LagState()
			assert.True(t, empty.IsZero())
			assert.True(t, read.IsZero(), "initial persistence is not lag evidence (ADR-054)")
			require.NoError(t, mock.ExpectationsWereMet())

			// Start uses the prepared pair, performs no lookup/write, and cancellation starts no reader.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			w.Start(ctx)
			assert.Equal(t, initialDBTime, w.lastTimestamp)
			assert.Zero(t, w.lastEventID)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWatcherConstruction_InitializationErrors_FailConstruction(t *testing.T) {
	for path, construct := range positionConstructors() {
		for _, failure := range []string{"load", "clock-or-write", "readback"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()

				load := mock.ExpectQuery(regexp.QuoteMeta(loadResumeTokenQuery)).WithArgs("event-exporter")
				if failure == "load" {
					load.WillReturnError(sql.ErrConnDone)
				} else {
					load.WillReturnError(sql.ErrNoRows)
					insert := mock.ExpectExec(initialInsertPattern).WithArgs("event-exporter")
					if failure == "clock-or-write" {
						insert.WillReturnError(sql.ErrConnDone)
					} else {
						insert.WillReturnResult(sqlmock.NewResult(0, 1))
						mock.ExpectQuery(regexp.QuoteMeta(loadResumeTokenQuery)).WithArgs("event-exporter").
							WillReturnError(sql.ErrConnDone)
					}
				}

				w, err := construct(t.Context(), db, "event-exporter")
				require.ErrorIs(t, err, sql.ErrConnDone)
				assert.Nil(t, w)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestWatcherConstruction_ExistingTokens_NeverWrites(t *testing.T) {
	fallbackTime := initialDBTime.Add(time.Hour)
	cases := []struct {
		name        string
		token       []byte
		legacy      bool
		missingRow  bool
		replay      bool // malformed: keep the row and replay from the beginning
		wantTime    time.Time
		wantEventID int64
	}{
		{name: "initial row", token: positionToken(t, initialDBTime, 0), wantTime: initialDBTime},
		{name: "acknowledged row", token: positionToken(t, initialDBTime, 42), wantTime: initialDBTime, wantEventID: 42},
		{name: "legacy", token: []byte(`{"eventID":42}`), legacy: true, wantTime: initialDBTime, wantEventID: 42},
		// As in #2250: an expired legacy row resumes at the earliest later timestamp, with ID zero.
		{name: "missing legacy row", token: []byte(`{"eventID":42}`), legacy: true, missingRow: true, wantTime: fallbackTime},
		{name: "malformed JSON", token: []byte(`{"eventID":`), replay: true},
		{name: "malformed timestamp", token: []byte(`{"eventID":42,"timestamp":"invalid"}`), replay: true},
		{name: "missing fields", token: []byte(`{}`), replay: true},
		{name: "missing timestamp at zero", token: []byte(`{"eventID":0}`), replay: true},
		{name: "fractional ID", token: []byte(`{"eventID":1.5}`), replay: true},
		{name: "negative ID", token: []byte(`{"eventID":-1}`), replay: true},
	}
	for path, construct := range positionConstructors() {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				db.SetMaxOpenConns(1) // Legacy resolution must release the initialization connection.
				expectPositionToken(mock, "event-exporter", tc.token)
				if tc.legacy {
					lookup := mock.ExpectQuery(`SELECT changed_at FROM datastore_changelog WHERE id = \$1`).WithArgs(int64(42))
					if tc.missingRow {
						lookup.WillReturnError(sql.ErrNoRows)
						mock.ExpectQuery(`SELECT MIN\(changed_at\) FROM datastore_changelog WHERE id > \$1`).
							WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(fallbackTime))
					} else {
						lookup.WillReturnRows(sqlmock.NewRows([]string{"changed_at"}).AddRow(initialDBTime))
					}
				}

				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				w, err := construct(ctx, db, "event-exporter")
				// No case fails construction, and none writes: existing rows are never replaced.
				require.NoError(t, err)
				require.True(t, w.positionInitialized)
				if tc.replay {
					assert.True(t, w.lastTimestamp.IsZero())
					assert.Zero(t, w.lastEventID)
				} else {
					assert.Equal(t, tc.wantTime, w.lastTimestamp)
					assert.Equal(t, tc.wantEventID, w.lastEventID)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestWatcherConstruction_RestartBeforeAck_ReplaysSavedStart(t *testing.T) {
	for path, construct := range positionConstructors() {
		t.Run(path, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			expectInitialPosition(t, mock, "event-exporter")
			first, err := construct(t.Context(), db, "event-exporter")
			require.NoError(t, err)

			for _, w := range []*PostgreSQLChangeStreamWatcher{first, nil} {
				if w == nil {
					expectPositionToken(mock, "event-exporter", positionToken(t, initialDBTime, 0))
					w, err = construct(t.Context(), db, "event-exporter")
					require.NoError(t, err)
				}
				mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
					WithArgs("health_events", initialDBTime, int64(0)).WillReturnRows(sqlmock.NewRows([]string{
					"id", "record_id", "operation", "old_values", "new_values", "changed_at",
				}).AddRow(1, "uuid-1", "INSERT", nil, `{"document":{}}`, initialDBTime.Add(time.Second)))
				require.NoError(t, w.fetchNewChanges(t.Context()))
				assert.Equal(t, []byte("1"), (<-w.Events()).ResumeToken)
				require.NoError(t, w.Close(t.Context())) // crash before ack: no checkpoint write
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWatcherConstruction_ConcurrentInsert_UsesStoredWinner(t *testing.T) {
	// Independent pools represent two constructors that both observed an absent row.
	// The server resolves the insert race; both must use its winner, including the loser.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 2 {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta(loadResumeTokenQuery)).WithArgs("event-exporter").WillReturnError(sql.ErrNoRows)
		mock.ExpectExec(initialInsertPattern).WithArgs("event-exporter").WillReturnResult(sqlmock.NewResult(0, int64(i)))
		expectPositionToken(mock, "event-exporter", positionToken(t, initialDBTime, 0))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w, err := positionConstructors()["database-client"](t.Context(), db, "event-exporter")
			if assert.NoError(t, err) {
				assert.Equal(t, initialDBTime, w.lastTimestamp)
				assert.Zero(t, w.lastEventID)
			}
			assert.NoError(t, mock.ExpectationsWereMet())
		}()
	}
	close(start)
	wg.Wait()
}

func TestWatcherConstruction_ResetBeforeInitialization_PreservesOrder(t *testing.T) {
	for _, path := range []string{"factory", "datastore"} {
		for _, fail := range []bool{false, true} {
			name := path + "/success"
			if fail {
				name = path + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				ds := &PostgreSQLDataStore{db: db, metricsRegisterer: prometheus.NewRegistry()}
				reset := func(ctx context.Context, dbClient client.DatabaseClient, config client.TokenConfig) (client.ResumeControlDecision, error) {
					err := dbClient.DeleteResumeToken(ctx, config)
					return client.ResumeControlDecision{StartFresh: err == nil}, err
				}
				deletion := mock.ExpectExec(`DELETE FROM resume_tokens WHERE client_name = \$1`).WithArgs("node-drainer")
				if fail {
					deletion.WillReturnError(sql.ErrConnDone)
				} else {
					deletion.WillReturnResult(sqlmock.NewResult(0, 1))
					expectInitialPosition(t, mock, "node-drainer")
				}

				var result datastore.ChangeStreamWatcher
				if path == "factory" {
					result, err = (&PostgreSQLWatcherFactory{}).createChangeStreamWatcher(t.Context(), ds,
						watcher.WatcherConfig{ClientName: "node-drainer", CollectionName: "HealthEvents"}, reset)
				} else {
					result, err = ds.newChangeStreamWatcher(t.Context(),
						map[string]any{"ClientName": "node-drainer", "TableName": "HealthEvents"}, reset)
				}
				if fail {
					require.ErrorIs(t, err, sql.ErrConnDone)
					assert.Nil(t, result)
				} else {
					require.NoError(t, err)
					w := result.(*PostgreSQLChangeStreamWatcherWithUnwrap)
					assert.True(t, w.ResumeControlDecision().StartFresh)
					assert.Equal(t, initialDBTime, w.watcher.lastTimestamp)
					assert.Zero(t, w.watcher.lastEventID)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

// Existing poll/ack tests load fixtures without invoking construction. Production uses the initializer.
// loadResumePosition loads an existing token without creating or changing a checkpoint.
func (w *PostgreSQLChangeStreamWatcher) loadResumePosition(ctx context.Context) error {
	var tokenJSON []byte

	if err := w.db.QueryRowContext(ctx, loadResumeTokenQuery, w.clientName).Scan(&tokenJSON); err != nil {
		return fmt.Errorf("load resume position for %s: %w", w.clientName, err)
	}

	return w.restoreResumePosition(ctx, tokenJSON)
}

func TestStart_PreparedPosition_ReadsWithoutReloadOrWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	expectInitialPosition(t, mock, "event-exporter")
	w, err := positionConstructors()["database-client"](t.Context(), db, "event-exporter")
	require.NoError(t, err)
	w.mode = ModePolling
	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs("health_events", initialDBTime, int64(0)).WillReturnRows(sqlmock.NewRows([]string{
		"id", "record_id", "operation", "old_values", "new_values", "changed_at",
	}).AddRow(1, "uuid-1", "INSERT", nil, `{"document":{}}`, initialDBTime.Add(time.Second)))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.Start(ctx)
	select {
	case event := <-w.Events():
		assert.Equal(t, []byte("1"), event.ResumeToken)
	case <-time.After(time.Second):
		t.Fatal("Start did not read from the prepared pair")
	}
	cancel()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWatcherConstruction_CancelledContext_ReturnsError(t *testing.T) {
	for path, construct := range positionConstructors() {
		t.Run(path, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			w, err := construct(ctx, db, "event-exporter")
			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, w)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWatcherConstruction_ManualTokenDeletion_InitializesFreshPair(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	construct := positionConstructors()["database-client"]
	expectPositionToken(mock, "event-exporter", positionToken(t, initialDBTime.Add(-time.Hour), 42))
	old, err := construct(t.Context(), db, "event-exporter")
	require.NoError(t, err)
	require.Equal(t, int64(42), old.lastEventID)

	mock.ExpectExec(`DELETE FROM resume_tokens WHERE client_name = \$1`).WithArgs("event-exporter").
		WillReturnResult(sqlmock.NewResult(0, 1))
	_, err = db.ExecContext(t.Context(), "DELETE FROM resume_tokens WHERE client_name = $1", "event-exporter")
	require.NoError(t, err)
	expectInitialPosition(t, mock, "event-exporter")
	recovered, err := construct(t.Context(), db, "event-exporter")
	require.NoError(t, err)
	assert.Equal(t, initialDBTime, recovered.lastTimestamp)
	assert.True(t, recovered.lastTimestamp.After(old.lastTimestamp))
	assert.Zero(t, recovered.lastEventID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWatcherConstruction_MalformedToken_ReplaysWithoutWriting(t *testing.T) {
	malformed := map[string][]byte{
		"not an object":           []byte(`[]`),
		"missing eventID":         []byte(`{"timestamp":"2024-11-24T10:00:00Z"}`),
		"negative eventID":        []byte(`{"timestamp":"2024-11-24T10:00:00Z","eventID":-1}`),
		"zero timestamp":          []byte(`{"timestamp":"0001-01-01T00:00:00Z","eventID":5}`),
		"no timestamp, eventID 0": []byte(`{"eventID":0}`),
	}

	for path, construct := range positionConstructors() {
		for name, token := range malformed {
			t.Run(path+"/"+name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()

				// Failing construction would fail every restart. The row is kept for an
				// operator and the watcher replays from the beginning, with no write.
				expectPositionToken(mock, "event-exporter", token)

				w, err := construct(t.Context(), db, "event-exporter")
				require.NoError(t, err)
				require.True(t, w.positionInitialized)
				assert.True(t, w.lastTimestamp.IsZero())
				assert.Zero(t, w.lastEventID)
				assert.Zero(t, w.highestReadEventID)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
