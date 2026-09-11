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

package client

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestClassifyPostgresDocumentError(t *testing.T) {
	t.Run("unique violation is a duplicate with the constraint name", func(t *testing.T) {
		pqErr := &pq.Error{
			Code:       pqUniqueViolationCode,
			Constraint: datastore.HealthEventIdempotencyIndexName,
			Message:    "duplicate key value violates unique constraint",
		}

		docErr, answered := ClassifyPostgresDocumentError(3, pqErr)
		require.True(t, answered)
		assert.Equal(t, 3, docErr.DocumentIndex)
		assert.True(t, docErr.Duplicate)
		assert.Equal(t, datastore.HealthEventIdempotencyIndexName, docErr.IndexName)
		assert.Equal(t, pqErr.Message, docErr.Message)
	})

	t.Run("wrapped unique violation is classified", func(t *testing.T) {
		pqErr := &pq.Error{Code: pqUniqueViolationCode, Constraint: "some_index"}
		wrapped := fmt.Errorf("failed to insert document: %w", pqErr)

		docErr, answered := ClassifyPostgresDocumentError(0, wrapped)
		require.True(t, answered)
		assert.True(t, docErr.Duplicate)
		assert.Equal(t, "some_index", docErr.IndexName)
	})

	t.Run("an error that is not the server's answer about the document is not classified", func(t *testing.T) {
		_, answered := ClassifyPostgresDocumentError(0, errors.New("connection reset"))
		assert.False(t, answered)
	})

	t.Run("other SQLSTATE codes are not duplicates", func(t *testing.T) {
		pqErr := &pq.Error{Code: "23502", Message: "null value in column"}

		docErr, answered := ClassifyPostgresDocumentError(1, pqErr)
		require.True(t, answered)
		assert.False(t, docErr.Duplicate)
		assert.Empty(t, docErr.IndexName)
	})

	t.Run("a data exception is an answer about the document", func(t *testing.T) {
		pqErr := &pq.Error{Code: "22P02", Message: "invalid input syntax for type json"}

		docErr, answered := ClassifyPostgresDocumentError(2, pqErr)
		require.True(t, answered)
		assert.Equal(t, 2, docErr.DocumentIndex)
		assert.False(t, docErr.Duplicate)
	})

	t.Run("a server shutdown is not an answer about the document", func(t *testing.T) {
		pqErr := &pq.Error{Code: "57P01", Message: "terminating connection due to administrator command"}

		_, answered := ClassifyPostgresDocumentError(0, pqErr)
		assert.False(t, answered, "SQLSTATE class 57 concerns the server, so the whole batch must fail")
	})

	t.Run("a connection failure is not an answer about the document", func(t *testing.T) {
		pqErr := &pq.Error{Code: "08006", Message: "connection failure"}

		_, answered := ClassifyPostgresDocumentError(0, pqErr)
		assert.False(t, answered, "SQLSTATE class 08 concerns the connection, so the whole batch must fail")
	})
}

// TestInsertManyIdempotentWith_ServerShutdownFailsTheBatch: a server that goes
// away in the middle of a batch has answered nothing about the document it was
// asked to store, so the batch fails as a datastore failure rather than as a
// per-document one: the caller retries the whole batch instead of treating
// the shutdown as a constraint violation.
func TestInsertManyIdempotentWith_ServerShutdownFailsTheBatch(t *testing.T) {
	calls := 0
	insertOne := func(_ context.Context, _ any) (string, error) {
		calls++
		if calls == 2 {
			return "", &pq.Error{Code: "57P01", Message: "terminating connection due to administrator command"}
		}

		return fmt.Sprintf("id-%d", calls), nil
	}

	_, err := InsertManyIdempotentWith(context.Background(), []any{"a", "b", "c"}, insertOne)
	require.Error(t, err)

	_, perDocument := datastore.AsBulkWriteFailure(err)
	assert.False(t, perDocument, "a shutdown is a datastore failure, not a per-document answer")
	assert.Equal(t, 2, calls, "the insert stops at the failure")
}

// TestNormalizeIndexSQL_KeepsLiterals: only SQL syntax outside single-quoted
// literals is normalized away, so a JSON path that differs inside its quotes
// stays different.
func TestNormalizeIndexSQL_KeepsLiterals(t *testing.T) {
	assert.Equal(t, "document#>>'{a,b (c)}'", normalizeIndexSQL("((document #>> '{a,b (c)}'::text[]))"))
	assert.Equal(t, "'it''s ( x )'ISNOTNULL", normalizeIndexSQL("('it''s ( x )') IS NOT NULL"))
	assert.NotEqual(t, normalizeIndexSQL("(document #>> '{a,b}')"), normalizeIndexSQL("(document #>> '{a,(b)}')"))
}

func TestPostgreSQLClientInsertManyIdempotent(t *testing.T) {
	newClient := func(t *testing.T) (*PostgreSQLClient, sqlmock.Sqlmock) {
		t.Helper()

		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		return NewPostgreSQLClientFromDB(db, healthEventsTableName), mock
	}

	insertQuery := "INSERT INTO health_events (document) VALUES ($1) RETURNING id"

	t.Run("empty input returns empty result", func(t *testing.T) {
		client, _ := newClient(t)

		result, err := client.InsertManyIdempotent(context.Background(), []any{})
		require.NoError(t, err)
		assert.Empty(t, result.InsertedIDs)
	})

	t.Run("all documents inserted", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(insertQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("id-1"))
		mock.ExpectQuery(insertQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("id-2"))

		result, err := client.InsertManyIdempotent(context.Background(),
			[]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
		require.NoError(t, err)
		assert.Equal(t, []any{"id-1", "id-2"}, result.InsertedIDs)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("duplicate does not stop the remaining inserts", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(insertQuery).WillReturnError(&pq.Error{
			Code:       pqUniqueViolationCode,
			Constraint: datastore.HealthEventIdempotencyIndexName,
			Message:    "duplicate key value violates unique constraint",
		})
		mock.ExpectQuery(insertQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("id-2"))

		result, err := client.InsertManyIdempotent(context.Background(),
			[]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
		require.NoError(t, err, "a resend is a success, not a failure")
		assert.Equal(t, []any{"id-2"}, result.InsertedIDs)
		assert.Equal(t, 1, result.DuplicateCount)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a server answer that is not a duplicate stops the insert so the retry stores the rest in order",
		func(t *testing.T) {
			client, mock := newClient(t)
			mock.ExpectQuery(insertQuery).WillReturnError(&pq.Error{
				Code:       pqUniqueViolationCode,
				Constraint: datastore.HealthEventIdempotencyIndexName,
			})
			mock.ExpectQuery(insertQuery).WillReturnError(&pq.Error{Code: "23514", Message: "check constraint violated"})
			// No expectation for the third document: it must not be attempted.

			_, err := client.InsertManyIdempotent(context.Background(),
				[]any{map[string]any{"a": 1}, map[string]any{"b": 2}, map[string]any{"c": 3}})
			require.Error(t, err)

			failure, ok := datastore.AsBulkWriteFailure(err)
			require.True(t, ok)
			assert.Equal(t, 0, failure.InsertedCount)
			assert.Equal(t, 1, failure.DuplicateCount)
			assert.Equal(t, 1, failure.Failed.DocumentIndex)
			assert.False(t, failure.Failed.Duplicate)
			assert.Contains(t, err.Error(), "check constraint violated", "the server's answer survives into the error")
			assert.NoError(t, mock.ExpectationsWereMet())
		})

	t.Run("a transport failure fails the whole batch, as it does for MongoDB", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(insertQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("id-1"))
		mock.ExpectQuery(insertQuery).WillReturnError(errors.New("connection reset"))
		// No expectation for the third document: it must not be attempted.

		_, err := client.InsertManyIdempotent(context.Background(),
			[]any{map[string]any{"a": 1}, map[string]any{"b": 2}, map[string]any{"c": 3}})
		require.Error(t, err)

		_, perDocument := datastore.AsBulkWriteFailure(err)
		assert.False(t, perDocument, "not an answer about a document")
		assert.Contains(t, err.Error(), "connection reset")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("a duplicate on another unique index stops the insert", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(insertQuery).WillReturnError(&pq.Error{
			Code:       pqUniqueViolationCode,
			Constraint: "health_events_pkey",
		})

		_, err := client.InsertManyIdempotent(context.Background(),
			[]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
		require.Error(t, err)

		failure, ok := datastore.AsBulkWriteFailure(err)
		require.True(t, ok)
		assert.True(t, failure.Failed.Duplicate)
		assert.Equal(t, "health_events_pkey", failure.Failed.IndexName)
		assert.Contains(t, err.Error(), `duplicate on index "health_events_pkey"`)
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestPostgreSQLClientEnsureHealthEventIdempotencyIndex(t *testing.T) {
	createStatement := regexp.QuoteMeta(
		"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS healthevent_idempotency_key_unique ON health_events " +
			"((document #>> '{healthevent,metadata,idempotencyKey}')) " +
			"WHERE (document #>> '{healthevent,metadata,idempotencyKey}') IS NOT NULL")
	dropStatement := regexp.QuoteMeta("DROP INDEX CONCURRENTLY IF EXISTS healthevent_idempotency_key_unique")
	progressQuery := "pg_stat_progress_create_index"
	progressColumns := []string{"building"}

	verifyQuery := "SELECT i.indisunique"
	verifyColumns := []string{"indisunique", "indisvalid", "indnatts", "indexdef", "predicate"}
	validIndexDef := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
		"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))) " +
		"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	validPredicate := "((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"

	expectIndex := func(mock sqlmock.Sqlmock, valid bool, predicate string) {
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnRows(sqlmock.NewRows(verifyColumns).AddRow(true, valid, 1, validIndexDef, predicate))
	}
	expectMissing := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnRows(sqlmock.NewRows(verifyColumns))
	}
	expectBuilding := func(mock sqlmock.Sqlmock, building bool) {
		mock.ExpectQuery(progressQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnRows(sqlmock.NewRows(progressColumns).AddRow(building))
	}

	newClient := func(t *testing.T) (*PostgreSQLClient, sqlmock.Sqlmock) {
		t.Helper()

		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		return NewPostgreSQLClientFromDB(db, healthEventsTableName), mock
	}

	t.Run("missing index is created concurrently", func(t *testing.T) {
		client, mock := newClient(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true, validPredicate)

		require.NoError(t, client.EnsureHealthEventIdempotencyIndex(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("index with the expected definition is left alone", func(t *testing.T) {
		client, mock := newClient(t)
		expectIndex(mock, true, validPredicate)

		require.NoError(t, client.EnsureHealthEventIdempotencyIndex(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("invalid leftover index is dropped concurrently and recreated", func(t *testing.T) {
		client, mock := newClient(t)
		expectIndex(mock, false, validPredicate)
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true, validPredicate)

		require.NoError(t, client.EnsureHealthEventIdempotencyIndex(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("index still being built by another session is left alone", func(t *testing.T) {
		client, mock := newClient(t)
		expectIndex(mock, false, validPredicate)
		expectBuilding(mock, true)

		err := client.EnsureHealthEventIdempotencyIndex(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still being built")
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("valid index with another definition is replaced", func(t *testing.T) {
		client, mock := newClient(t)
		expectIndex(mock, true, "(((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL) AND (node_name = 'node-a'::text))")
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, true, validPredicate)

		require.NoError(t, client.EnsureHealthEventIdempotencyIndex(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("failed drop is returned and the create is not attempted", func(t *testing.T) {
		client, mock := newClient(t)
		expectIndex(mock, false, validPredicate)
		expectBuilding(mock, false)
		mock.ExpectExec(dropStatement).WillReturnError(errors.New("lock timeout"))

		err := client.EnsureHealthEventIdempotencyIndex(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to drop mismatched idempotency index")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("aborted concurrent build is reported so the caller retries", func(t *testing.T) {
		client, mock := newClient(t)
		expectMissing(mock)
		mock.ExpectExec(createStatement).WillReturnResult(sqlmock.NewResult(0, 0))
		expectIndex(mock, false, validPredicate)

		err := client.EnsureHealthEventIdempotencyIndex(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not usable after the build")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("datastore failure during the check is returned", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(verifyQuery).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnError(errors.New("connection refused"))

		err := client.EnsureHealthEventIdempotencyIndex(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to check idempotency index")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestPostgreSQLClientVerifyHealthEventIdempotencyIndex(t *testing.T) {
	validIndexDef := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
		"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))) " +
		"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	validPredicate := "((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"

	newClient := func(t *testing.T) (*PostgreSQLClient, sqlmock.Sqlmock) {
		t.Helper()

		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		return NewPostgreSQLClientFromDB(db, healthEventsTableName), mock
	}

	columns := []string{"indisunique", "indisvalid", "indnatts", "indexdef", "predicate"}

	t.Run("matching index passes", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery("SELECT i.indisunique").
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnRows(
				sqlmock.NewRows(columns).AddRow(true, true, 1, validIndexDef, validPredicate))

		assert.NoError(t, client.VerifyHealthEventIdempotencyIndex(context.Background()))
	})

	t.Run("table is resolved through the search_path via to_regclass", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery(`i\.indrelid = to_regclass\(\$2\)`).
			WithArgs(datastore.HealthEventIdempotencyIndexName, healthEventsTableName).
			WillReturnRows(
				sqlmock.NewRows(columns).AddRow(true, true, 1, validIndexDef, validPredicate))

		assert.NoError(t, client.VerifyHealthEventIdempotencyIndex(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("missing index yields ErrIndexMissing", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery("SELECT i.indisunique").WillReturnRows(sqlmock.NewRows(columns))

		err := client.VerifyHealthEventIdempotencyIndex(context.Background())
		assert.ErrorIs(t, err, datastore.ErrIndexMissing)
	})

	t.Run("invalid index yields ErrIndexMismatch", func(t *testing.T) {
		client, mock := newClient(t)
		mock.ExpectQuery("SELECT i.indisunique").WillReturnRows(
			sqlmock.NewRows(columns).AddRow(true, false, 1, validIndexDef, validPredicate))

		err := client.VerifyHealthEventIdempotencyIndex(context.Background())
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})

	t.Run("composite index containing the expression yields ErrIndexMismatch", func(t *testing.T) {
		client, mock := newClient(t)
		compositeIndexDef := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
			"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[])), node_name) " +
			"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
		mock.ExpectQuery("SELECT i.indisunique").WillReturnRows(
			sqlmock.NewRows(columns).AddRow(true, true, 2, compositeIndexDef, validPredicate))

		err := client.VerifyHealthEventIdempotencyIndex(context.Background())
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})
}

func TestVerifyPostgresIdempotencyIndexDefinition(t *testing.T) {
	validIndexDef := "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
		"USING btree (((document #>> '{healthevent,metadata,idempotencyKey}'::text[]))) " +
		"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"
	validPredicate := "((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)"

	tests := []struct {
		name      string
		unique    bool
		valid     bool
		indnatts  int
		indexDef  string
		predicate string
		wantErr   error
	}{
		{
			name:   "matching definition",
			unique: true, valid: true, indnatts: 1, indexDef: validIndexDef, predicate: validPredicate,
			wantErr: nil,
		},
		{
			name:   "not unique",
			unique: false, valid: true, indnatts: 1, indexDef: validIndexDef, predicate: validPredicate,
			wantErr: datastore.ErrIndexMismatch,
		},
		{
			name:   "composite index containing the key expression",
			unique: true, valid: true, indnatts: 2,
			indexDef:  validIndexDef,
			predicate: validPredicate,
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "wrong key expression",
			unique: true, valid: true, indnatts: 1,
			indexDef:  "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON health_events (node_name)",
			predicate: validPredicate,
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "not partial",
			unique: true, valid: true, indnatts: 1, indexDef: validIndexDef, predicate: "",
			wantErr: datastore.ErrIndexMismatch,
		},
		{
			name:   "wrapped key expression",
			unique: true, valid: true, indnatts: 1,
			indexDef: "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
				"USING btree (\"left\"((document #>> '{healthevent,metadata,idempotencyKey}'::text[]), 1)) " +
				"WHERE ((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL)",
			predicate: validPredicate,
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "extra predicate term",
			unique: true, valid: true, indnatts: 1, indexDef: validIndexDef,
			predicate: "(((document #>> '{healthevent,metadata,idempotencyKey}'::text[]) IS NOT NULL) " +
				"AND (node_name = 'node-a'::text))",
			wantErr: datastore.ErrIndexMismatch,
		},
		{
			name:   "wrong-case key path",
			unique: true, valid: true, indnatts: 1,
			indexDef: "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
				"USING btree (((document #>> '{healthevent,metadata,idempotencykey}'::text[]))) " +
				"WHERE ((document #>> '{healthevent,metadata,idempotencykey}'::text[]) IS NOT NULL)",
			predicate: "((document #>> '{healthevent,metadata,idempotencykey}'::text[]) IS NOT NULL)",
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "parentheses inside the key literal",
			unique: true, valid: true, indnatts: 1,
			indexDef: "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
				"USING btree (((document #>> '{healthevent,metadata,idempotency(Key)}'::text[]))) " +
				"WHERE ((document #>> '{healthevent,metadata,idempotency(Key)}'::text[]) IS NOT NULL)",
			predicate: "((document #>> '{healthevent,metadata,idempotency(Key)}'::text[]) IS NOT NULL)",
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "whitespace inside the key literal",
			unique: true, valid: true, indnatts: 1,
			indexDef: "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON public.health_events " +
				"USING btree (((document #>> '{healthevent,metadata,idempotency Key}'::text[]))) " +
				"WHERE ((document #>> '{healthevent,metadata,idempotency Key}'::text[]) IS NOT NULL)",
			predicate: "((document #>> '{healthevent,metadata,idempotency Key}'::text[]) IS NOT NULL)",
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "same definition rendered without casts",
			unique: true, valid: true, indnatts: 1,
			indexDef: "CREATE UNIQUE INDEX healthevent_idempotency_key_unique ON health_events " +
				"((document #>> '{healthevent,metadata,idempotencyKey}')) " +
				"WHERE (document #>> '{healthevent,metadata,idempotencyKey}') IS NOT NULL",
			predicate: "(document #>> '{healthevent,metadata,idempotencyKey}') IS NOT NULL",
		},
		{
			name:   "wrong predicate",
			unique: true, valid: true, indnatts: 1, indexDef: validIndexDef,
			predicate: "(node_name IS NOT NULL)",
			wantErr:   datastore.ErrIndexMismatch,
		},
		{
			name:   "invalid build",
			unique: true, valid: false, indnatts: 1, indexDef: validIndexDef, predicate: validPredicate,
			wantErr: datastore.ErrIndexMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyPostgresIdempotencyIndexDefinition(tt.unique, tt.valid, tt.indnatts, tt.indexDef, tt.predicate)
			if tt.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tt.wantErr)
			}
		})
	}
}
