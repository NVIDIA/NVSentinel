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
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestPostgreSQLDatabaseClientInsertManyIdempotentTransportFailure(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)

	defer db.Close()

	dbClient := NewPostgreSQLDatabaseClientWithConnString(db, "HealthEvents", "")
	pgClient, ok := dbClient.(*PostgreSQLDatabaseClient)
	require.True(t, ok)

	insertQuery := "INSERT INTO health_events (data) VALUES ($1) RETURNING id"
	mock.ExpectQuery(insertQuery).WillReturnError(errors.New("connection reset"))
	// No expectation for the second document: it must not be attempted.

	_, err = pgClient.InsertManyIdempotent(context.Background(),
		[]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
	require.Error(t, err)

	_, perDocument := datastore.AsBulkWriteFailure(err)
	assert.False(t, perDocument, "a lost connection is not an answer about a document; the whole batch failed")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgreSQLDatabaseClientInsertManyIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)

	defer db.Close()

	dbClient := NewPostgreSQLDatabaseClientWithConnString(db, "HealthEvents", "")
	pgClient, ok := dbClient.(*PostgreSQLDatabaseClient)
	require.True(t, ok)

	insertQuery := "INSERT INTO health_events (data) VALUES ($1) RETURNING id"
	mock.ExpectQuery(insertQuery).WillReturnError(&pq.Error{
		Code:       "23505",
		Constraint: datastore.HealthEventIdempotencyIndexName,
	})
	mock.ExpectQuery(insertQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("id-2"))

	result, err := pgClient.InsertManyIdempotent(context.Background(),
		[]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
	require.NoError(t, err, "a resend is a success, not a failure")
	assert.Equal(t, []any{"id-2"}, result.InsertedIDs)
	assert.Equal(t, 1, result.DuplicateCount)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Duplicate classification must survive the health-event route
// (insertSingleHealthEvent -> InsertHealthEventsWithIndexFields -> the
// health_events columnar INSERT), whose errors reach the classifier only
// through %w wrap sites — not just the generic JSONB route above.
func TestPostgreSQLDatabaseClientInsertManyIdempotentHealthEventDuplicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	dbClient := NewPostgreSQLDatabaseClientWithConnString(db, "HealthEvents", "")
	pgClient, ok := dbClient.(*PostgreSQLDatabaseClient)
	require.True(t, ok)

	mock.ExpectExec("INSERT INTO health_events").WillReturnError(&pq.Error{
		Code:       "23505",
		Constraint: datastore.HealthEventIdempotencyIndexName,
		Message:    "duplicate key value violates unique constraint",
	})

	result, err := pgClient.InsertManyIdempotent(context.Background(), []any{
		model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:  "node-1",
				CheckName: "GpuXidError",
			},
		},
	})
	require.NoError(t, err, "the duplicate reached the classifier through the wrap sites and counts as a resend")
	assert.Empty(t, result.InsertedIDs)
	assert.Equal(t, 1, result.DuplicateCount)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveMaxOpenConns(t *testing.T) {
	t.Run("default when nothing configured", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "")
		assert.Equal(t, defaultMaxOpenConns, resolveMaxOpenConns(map[string]string{}))
	})

	t.Run("options value wins", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "40")
		assert.Equal(t, 10, resolveMaxOpenConns(map[string]string{"maxConnections": "10"}))
	})

	t.Run("environment fallback", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "40")
		assert.Equal(t, 40, resolveMaxOpenConns(map[string]string{}))
	})

	t.Run("invalid values fall through to the default", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "-5")
		assert.Equal(t, defaultMaxOpenConns, resolveMaxOpenConns(map[string]string{"maxConnections": "abc"}))
	})
}

func TestConfigureConnectionPool(t *testing.T) {
	t.Setenv("DATASTORE_MAX_CONNECTIONS", "7")

	db, _, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	ConfigureConnectionPool(db, nil)
	assert.Equal(t, 7, db.Stats().MaxOpenConnections)
}
