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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	// healthEventsTableName is the PostgreSQL table health events are stored in.
	// Health events always land in this table (see the health event insert path),
	// so the idempotency index is managed on it regardless of the configured table.
	healthEventsTableName = "health_events"

	// healthEventIdempotencyKeyJSONPath is the PostgreSQL text-array path of the
	// idempotency key inside the JSONB document column, for use with the #>>
	// operator. It mirrors healthEventIdempotencyKeyDocumentPath for MongoDB.
	healthEventIdempotencyKeyJSONPath = "{healthevent,metadata," +
		datastore.HealthEventIdempotencyKeyMetadataField + "}"

	// dropIdempotencyIndexStatement removes a leftover INVALID build, or a
	// mismatched definition, of the idempotency index without blocking
	// writers. It must run outside a transaction block, like the CONCURRENTLY
	// create it makes room for.
	dropIdempotencyIndexStatement = "DROP INDEX CONCURRENTLY IF EXISTS " +
		datastore.HealthEventIdempotencyIndexName
)

// InsertManyIdempotent inserts documents one at a time, in order, through
// InsertManyIdempotentWith. The idempotency index lives on the health events
// table, so a client configured for any other table is refused: there a
// resend would be stored again and reported as new.
func (c *PostgreSQLClient) InsertManyIdempotent(ctx context.Context, documents []any) (*InsertManyResult, error) {
	if c.table != healthEventsTableName {
		return nil, fmt.Errorf("idempotent inserts are defined for the %s table only, which carries the idempotency index; "+
			"this client writes to %s", healthEventsTableName, c.table)
	}

	//nolint:gosec // G201: table name from config, values are parameterized
	query := fmt.Sprintf("INSERT INTO %s (document) VALUES ($1) RETURNING id", c.table)

	return InsertManyIdempotentWith(ctx, documents, func(ctx context.Context, doc any) (string, error) {
		docJSON, err := json.Marshal(doc)
		if err != nil {
			return "", fmt.Errorf("failed to marshal document: %w", err)
		}

		var id string
		if err := c.db.QueryRowContext(ctx, query, docJSON).Scan(&id); err != nil {
			return "", err
		}

		return id, nil
	})
}

// InsertManyIdempotentWith inserts documents one at a time, in order, with
// insertOne. A duplicate on the idempotency index is a resend of an event
// already stored: it is counted in the result and the insert goes on, so the
// rest of a partly stored batch is still stored. Any other answer from the
// server about a document (a constraint the batch violates) stops the insert
// there, so the documents after it are stored in order by the client's retry
// of the whole batch, as with MongoDB's ordered insert; that answer is
// returned as a *datastore.BulkWriteFailure. A failure that is not an answer
// about a document (a lost connection, a cancelled context, an undecodable
// document) fails the whole batch, as it does for MongoDB.
func InsertManyIdempotentWith(
	ctx context.Context, documents []any, insertOne func(ctx context.Context, doc any) (string, error),
) (*InsertManyResult, error) {
	if len(documents) == 0 {
		return &InsertManyResult{InsertedIDs: []any{}}, nil
	}

	result := &InsertManyResult{InsertedIDs: make([]any, 0, len(documents))}

	for i, doc := range documents {
		id, err := insertOne(ctx, doc)
		if err == nil {
			result.InsertedIDs = append(result.InsertedIDs, id)

			continue
		}

		docErr, answered := ClassifyPostgresDocumentError(i, err)
		if !answered {
			return nil, datastore.NewInsertError(
				datastore.ProviderPostgreSQL,
				"failed to insert documents",
				err,
			).WithMetadata("count", len(documents)).WithMetadata("insertedCount", len(result.InsertedIDs))
		}

		if docErr.DuplicateOn(datastore.HealthEventIdempotencyIndexName) {
			result.DuplicateCount++

			continue
		}

		return nil, &datastore.BulkWriteFailure{
			InsertedCount:  len(result.InsertedIDs),
			DuplicateCount: result.DuplicateCount,
			Failed:         docErr,
		}
	}

	return result, nil
}

// pqDocumentErrorClasses are the SQLSTATE classes that answer a question about
// the document itself: 22 (data exception) and 23 (integrity constraint
// violation). Every other class says something about the connection or the
// server instead, for example 08 (connection exception), 57 (operator
// intervention, such as 57P01 admin_shutdown), 53 (insufficient resources) or
// 40 (transaction rollback), and must fail the whole batch as a datastore
// failure, so the caller retries it.
var pqDocumentErrorClasses = map[pqerror.Class]bool{"22": true, "23": true}

// ClassifyPostgresDocumentError converts the server's answer about one
// document into a BulkDocumentError, marking a unique violation as a duplicate
// on the named constraint. It reports false for anything that is not such an
// answer: not a PostgreSQL error at all, or a PostgreSQL error whose SQLSTATE
// class concerns the connection or the server rather than the document.
func ClassifyPostgresDocumentError(documentIndex int, err error) (datastore.BulkDocumentError, bool) {
	pqErr, ok := errors.AsType[*pq.Error](err)
	if !ok || !pqDocumentErrorClasses[pqErr.Code.Class()] {
		return datastore.BulkDocumentError{}, false
	}

	docErr := datastore.BulkDocumentError{DocumentIndex: documentIndex, Message: pqErr.Message}

	if pqErr.Code == pqerror.UniqueViolation {
		docErr.IndexName = pqErr.Constraint
		docErr.Duplicate = true
	}

	return docErr, true
}

// EnsureHealthEventIdempotencyIndex makes sure the partial unique expression
// index that enforces per-event idempotency keys exists with the expected
// definition. The predicate covers only documents that carry the key, so
// existing rows need no backfill.
//
// The index is built CONCURRENTLY so the build never blocks writers: every
// health event in the fleet lands in this table, and it may already hold
// millions of rows. CONCURRENTLY cannot run inside a transaction block, and
// c.db is the pool handle, so each statement below runs autocommitted on its
// own connection. Since a concurrent build takes many times longer than a
// locking one, callers must not treat a nil return as "index ready"; that is
// what VerifyHealthEventIdempotencyIndex is for.
//
// An index of this name that does not enforce the key, either another
// definition or the invalid leftover of a failed build (which IF NOT EXISTS
// would otherwise skip forever), is dropped (also CONCURRENTLY) and rebuilt:
// the name is ours, and the index Job is how an operator repairs the index.
// An index that another session is still building is left alone, because
// dropping it would block behind that build and then discard its result.
func (c *PostgreSQLClient) EnsureHealthEventIdempotencyIndex(ctx context.Context) error {
	switch verifyErr := c.VerifyHealthEventIdempotencyIndex(ctx); {
	case verifyErr == nil:
		return nil
	case errors.Is(verifyErr, datastore.ErrIndexMissing):
	case errors.Is(verifyErr, datastore.ErrIndexMismatch):
		building, err := c.idempotencyIndexBuildInProgress(ctx)
		if err != nil {
			return err
		}

		if building {
			return fmt.Errorf("idempotency index %s on table %s is still being built by another session; retry later: %w",
				datastore.HealthEventIdempotencyIndexName, healthEventsTableName, verifyErr)
		}

		if _, err := c.db.ExecContext(ctx, dropIdempotencyIndexStatement); err != nil {
			return fmt.Errorf("failed to drop mismatched idempotency index %s on table %s: %w",
				datastore.HealthEventIdempotencyIndexName, healthEventsTableName, err)
		}
	default:
		return fmt.Errorf("failed to check idempotency index %s on table %s: %w",
			datastore.HealthEventIdempotencyIndexName, healthEventsTableName, verifyErr)
	}

	//nolint:gosec // G201: all operands are package-level constants, no external input
	createStatement := fmt.Sprintf(
		"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s ((document #>> '%s')) "+
			"WHERE (document #>> '%s') IS NOT NULL",
		datastore.HealthEventIdempotencyIndexName,
		healthEventsTableName,
		healthEventIdempotencyKeyJSONPath,
		healthEventIdempotencyKeyJSONPath,
	)

	if _, err := c.db.ExecContext(ctx, createStatement); err != nil {
		return fmt.Errorf("failed to ensure idempotency index %s on table %s: %w",
			datastore.HealthEventIdempotencyIndexName, healthEventsTableName, err)
	}

	// IF NOT EXISTS skips an existing index without error even when that
	// index is a leftover INVALID build or another session's build still in
	// progress; the verification reports it so the caller retries later.
	if err := c.VerifyHealthEventIdempotencyIndex(ctx); err != nil {
		return fmt.Errorf("idempotency index %s on table %s is not usable after the build: %w",
			datastore.HealthEventIdempotencyIndexName, healthEventsTableName, err)
	}

	return nil
}

// idempotencyIndexBuildInProgress reports whether another session is still
// building an index of our name.
func (c *PostgreSQLClient) idempotencyIndexBuildInProgress(ctx context.Context) (bool, error) {
	query := `
		SELECT EXISTS (
		    SELECT 1
		    FROM pg_stat_progress_create_index p
		    JOIN pg_class idx ON idx.oid = p.index_relid
		    WHERE idx.relname = $1 AND p.relid = to_regclass($2))`

	var building bool

	if err := c.db.QueryRowContext(ctx, query,
		datastore.HealthEventIdempotencyIndexName, healthEventsTableName).Scan(&building); err != nil {
		return false, fmt.Errorf("failed to check whether idempotency index %s on table %s is being built: %w",
			datastore.HealthEventIdempotencyIndexName, healthEventsTableName, err)
	}

	return building, nil
}

// VerifyHealthEventIdempotencyIndex returns nil only when the idempotency index
// exists with the expected name, key expression, uniqueness, partial predicate,
// and a valid (completed) build per pg_index.indisvalid. A missing index yields
// datastore.ErrIndexMissing; any other deviation yields datastore.ErrIndexMismatch,
// both wrapped with detail.
//
// The CONCURRENTLY build started by EnsureHealthEventIdempotencyIndex keeps
// indisvalid false until it completes, so this also returns
// datastore.ErrIndexMismatch while that build is still running (or after it
// failed). Callers that gate readiness on this check therefore keep waiting
// until the index actually enforces uniqueness.
func (c *PostgreSQLClient) VerifyHealthEventIdempotencyIndex(ctx context.Context) error {
	// to_regclass resolves the table through the current search_path, so a
	// same-named table in another (invisible) schema cannot satisfy the check;
	// an absent or invisible table yields no rows and thus ErrIndexMissing.
	query := `
		SELECT i.indisunique,
		       i.indisvalid,
		       i.indnatts,
		       pg_get_indexdef(i.indexrelid),
		       COALESCE(pg_get_expr(i.indpred, i.indrelid), '')
		FROM pg_index i
		JOIN pg_class idx ON idx.oid = i.indexrelid
		WHERE idx.relname = $1 AND i.indrelid = to_regclass($2)`

	var (
		unique    bool
		valid     bool
		indnatts  int
		indexDef  string
		predicate string
	)

	row := c.db.QueryRowContext(ctx, query,
		datastore.HealthEventIdempotencyIndexName, healthEventsTableName)

	if err := row.Scan(&unique, &valid, &indnatts, &indexDef, &predicate); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: index %s not found on table %s",
				datastore.ErrIndexMissing, datastore.HealthEventIdempotencyIndexName,
				healthEventsTableName)
		}

		return fmt.Errorf("failed to query index definition for %s: %w",
			datastore.HealthEventIdempotencyIndexName, err)
	}

	return verifyPostgresIdempotencyIndexDefinition(unique, valid, indnatts, indexDef, predicate)
}

// normalizeIndexSQL strips what pg_get_indexdef and pg_get_expr add around
// the expression we created (parentheses, the text[] cast, whitespace) so an
// expression can be compared whole: another rendering of the same expression
// compares equal, a wrapped expression or an extra predicate term does not.
// Single-quoted literals are kept verbatim, so a JSON path that differs only
// inside its quotes (extra parentheses or spaces) does not compare equal. Case
// is kept too: PostgreSQL renders keywords in upper case, as the expected
// strings do, and the quoted JSON path is case-sensitive.
func normalizeIndexSQL(sql string) string {
	sql = strings.ReplaceAll(sql, "::text[]", "")

	var out strings.Builder

	inLiteral := false

	for i := range len(sql) {
		c := sql[i]

		switch {
		case c == '\'':
			// A doubled quote inside a literal toggles twice and stays inside.
			inLiteral = !inLiteral

			out.WriteByte(c)
		case inLiteral:
			out.WriteByte(c)
		case c == '(' || c == ')' || c == ' ' || c == '\t' || c == '\n':
		default:
			out.WriteByte(c)
		}
	}

	return out.String()
}

// indexKeyList extracts the indexed key list from a pg_get_indexdef result:
// what follows the access method (or the table, when no USING clause is
// present) up to the WHERE clause.
func indexKeyList(indexDef string) string {
	keys := indexDef
	if i := strings.Index(keys, " WHERE "); i >= 0 {
		keys = keys[:i]
	}

	marker := " USING "
	if !strings.Contains(keys, marker) {
		marker = " ON "
	}

	if i := strings.Index(keys, marker); i >= 0 {
		keys = keys[i+len(marker):]
		// Skip the access method (or the table name).
		if j := strings.Index(keys, " "); j >= 0 {
			keys = keys[j+1:]
		}
	}

	return keys
}

// verifyPostgresIdempotencyIndexDefinition checks the catalog view of the
// idempotency index against the definition Ensure creates: exactly one key,
// the bare idempotency key expression (not wrapped in another expression),
// unique, and a predicate of exactly "key IS NOT NULL" (no extra terms, which
// would leave rows outside the index).
func verifyPostgresIdempotencyIndexDefinition(unique, valid bool, indnatts int, indexDef, predicate string) error {
	expectedKey := normalizeIndexSQL(fmt.Sprintf("(document #>> '%s')", healthEventIdempotencyKeyJSONPath))
	expectedPredicate := normalizeIndexSQL(
		fmt.Sprintf("(document #>> '%s') IS NOT NULL", healthEventIdempotencyKeyJSONPath))

	if !unique {
		return fmt.Errorf("%w: index %s is not unique",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	if indnatts != 1 {
		return fmt.Errorf("%w: index %s indexes %d columns instead of exactly the idempotency key",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName, indnatts)
	}

	if normalizeIndexSQL(indexKeyList(indexDef)) != expectedKey {
		return fmt.Errorf("%w: index %s does not index exactly the idempotency key path %s",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName,
			healthEventIdempotencyKeyJSONPath)
	}

	if predicate == "" {
		return fmt.Errorf("%w: index %s is not a partial index",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	if normalizeIndexSQL(predicate) != expectedPredicate {
		return fmt.Errorf("%w: index %s partial predicate is not exactly the idempotency key IS NOT NULL",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	if !valid {
		return fmt.Errorf("%w: index %s is not valid (concurrent build still running or failed)",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	return nil
}
