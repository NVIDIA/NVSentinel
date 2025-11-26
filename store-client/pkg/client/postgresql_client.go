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

package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	_ "github.com/lib/pq" // PostgreSQL driver
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	jsonbDocumentColumn = "document"

	// MongoDB query operators
	opLTE = "$lte"
	opEQ  = "$eq"
	opGTE = "$gte"
	opGT  = "$gt"
	opLT  = "$lt"
	opNE  = "$ne"

	// SQL order direction
	orderDESC = "DESC"

	// SQL window frame bounds
	frameBoundUnbounded  = "ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING"
	frameBoundCurrentRow = "CURRENT ROW"
)

// PostgreSQLClient implements DatabaseClient for PostgreSQL
type PostgreSQLClient struct {
	db       *sql.DB
	database string
	table    string // PostgreSQL uses tables instead of collections
	config   config.DatabaseConfig
}

// NewPostgreSQLClientFromDB creates a new PostgreSQL client from an existing database connection
// This is useful when you already have a db connection and want to use the aggregation features
func NewPostgreSQLClientFromDB(db *sql.DB, tableName string) *PostgreSQLClient {
	return &PostgreSQLClient{
		db:    db,
		table: tableName,
	}
}

// NewPostgreSQLClient creates a new PostgreSQL client from database configuration
func NewPostgreSQLClient(ctx context.Context, dbConfig config.DatabaseConfig) (*PostgreSQLClient, error) {
	// Open PostgreSQL connection
	db, err := sql.Open("postgres", dbConfig.GetConnectionURI())
	if err != nil {
		return nil, datastore.NewConnectionError(
			datastore.ProviderPostgreSQL,
			"failed to open PostgreSQL connection",
			err,
		)
	}

	// Test the connection
	if err := db.PingContext(ctx); err != nil {
		db.Close()

		return nil, datastore.NewConnectionError(
			datastore.ProviderPostgreSQL,
			"failed to connect to PostgreSQL",
			err,
		)
	}

	return &PostgreSQLClient{
		db:       db,
		database: dbConfig.GetDatabaseName(),
		table:    dbConfig.GetCollectionName(), // In PostgreSQL context, collection = table
		config:   dbConfig,
	}, nil
}

// Ping checks database connectivity
func (c *PostgreSQLClient) Ping(ctx context.Context) error {
	if err := c.db.PingContext(ctx); err != nil {
		return datastore.NewConnectionError(
			datastore.ProviderPostgreSQL,
			"failed to ping database",
			err,
		)
	}

	return nil
}

// Close closes the PostgreSQL client
func (c *PostgreSQLClient) Close(ctx context.Context) error {
	return c.db.Close()
}

// InsertMany inserts multiple documents
func (c *PostgreSQLClient) InsertMany(ctx context.Context, documents []interface{}) (*InsertManyResult, error) {
	if len(documents) == 0 {
		return &InsertManyResult{InsertedIDs: []interface{}{}}, nil
	}

	slog.Debug("InsertMany called", "documentCount", len(documents), "table", c.table)

	// Build batch insert query
	// INSERT INTO table (document) VALUES ($1), ($2), ... RETURNING id
	placeholders := make([]string, len(documents))
	args := make([]interface{}, len(documents))

	for i, doc := range documents {
		// Marshal document to JSON
		docJSON, err := json.Marshal(doc)
		if err != nil {
			slog.Error("Failed to marshal document", "index", i, "error", err)

			return nil, datastore.NewSerializationError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("failed to marshal document at index %d", i),
				err,
			)
		}

		placeholders[i] = fmt.Sprintf("($%d)", i+1)
		args[i] = docJSON
	}

	//nolint:gosec // G201: table name from config, using parameterized placeholders
	query := fmt.Sprintf(
		"INSERT INTO %s (document) VALUES %s RETURNING id",
		c.table,
		strings.Join(placeholders, ", "),
	)

	// Execute query and collect returned IDs
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		slog.Error("INSERT query failed", "error", err)

		return nil, datastore.NewInsertError(
			datastore.ProviderPostgreSQL,
			"failed to insert documents",
			err,
		)
	}
	defer rows.Close()

	var insertedIDs []interface{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, datastore.NewInsertError(
				datastore.ProviderPostgreSQL,
				"failed to scan inserted ID",
				err,
			)
		}

		insertedIDs = append(insertedIDs, id)
	}

	if err := rows.Err(); err != nil {
		return nil, datastore.NewInsertError(
			datastore.ProviderPostgreSQL,
			"error reading inserted IDs",
			err,
		)
	}

	return &InsertManyResult{InsertedIDs: insertedIDs}, nil
}

// UpdateDocumentStatus updates a specific status field in a document
func (c *PostgreSQLClient) UpdateDocumentStatus(
	ctx context.Context, documentID string, statusPath string, status interface{},
) error {
	// Build the JSONB path for the status field
	parts := strings.Split(statusPath, ".")
	jsonbPath := "{" + strings.Join(parts, ",") + "}"

	// Marshal status value to JSON
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			fmt.Sprintf("failed to marshal status for path %s", statusPath),
			err,
		)
	}

	slog.Debug("Updating document status", "documentID", documentID, "statusPath", statusPath)

	// Build and execute update query
	// For specific status paths, also update the denormalized top-level column
	var query string

	var args []interface{}

	//nolint:gosec // G201: table name from config, using parameterized queries ($1, $2)

	switch statusPath {
	case "healtheventstatus.nodequarantined":
		// Update both the JSONB field and the denormalized node_quarantined column
		query = fmt.Sprintf(
			"UPDATE %s SET document = jsonb_set(document, '%s', $1), node_quarantined = $2, updated_at = NOW() WHERE id = $3",
			c.table, jsonbPath,
		)
		args = []interface{}{string(statusJSON), status, documentID}
	default:
		// Only update the JSONB field
		query = fmt.Sprintf(
			"UPDATE %s SET document = jsonb_set(document, '%s', $1), updated_at = NOW() WHERE id = $2",
			c.table, jsonbPath,
		)
		args = []interface{}{string(statusJSON), documentID}
	}

	result, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		slog.Error("Update query failed", "error", err, "documentID", documentID)

		return datastore.NewUpdateError(
			datastore.ProviderPostgreSQL,
			fmt.Sprintf("failed to update status at path %s for document %s", statusPath, documentID),
			err,
		)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return datastore.NewUpdateError(
			datastore.ProviderPostgreSQL,
			"failed to get rows affected",
			err,
		)
	}

	slog.Debug("Update query completed", "rowsAffected", rowsAffected, "documentID", documentID)

	if rowsAffected == 0 {
		slog.Debug("No rows affected - document not found", "documentID", documentID)

		return datastore.NewDocumentNotFoundError(
			datastore.ProviderPostgreSQL,
			fmt.Sprintf("document not found: %s", documentID),
			nil,
		)
	}

	return nil
}

// UpdateDocument performs a general update operation
func (c *PostgreSQLClient) UpdateDocument(
	ctx context.Context, filter interface{}, update interface{},
) (*UpdateResult, error) {
	// Build WHERE clause from filter
	whereClause, args, err := c.buildWhereClause(filter)
	if err != nil {
		return nil, err
	}

	// Build SET clause from update
	setClause, updateArgs, err := c.buildUpdateClause(update)
	if err != nil {
		return nil, err
	}

	// Combine args (WHERE args + SET args)
	args = append(args, updateArgs...)

	// Build final query
	//nolint:gosec // G201: table name from config, clauses built with parameterized queries
	query := fmt.Sprintf(
		"UPDATE %s SET %s, updated_at = NOW() WHERE %s",
		c.table, setClause, whereClause,
	)

	result, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, datastore.NewUpdateError(
			datastore.ProviderPostgreSQL,
			"failed to update document",
			err,
		)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, datastore.NewUpdateError(
			datastore.ProviderPostgreSQL,
			"failed to get rows affected",
			err,
		)
	}

	return &UpdateResult{
		MatchedCount:  rowsAffected,
		ModifiedCount: rowsAffected,
		UpsertedCount: 0,
		UpsertedID:    nil,
	}, nil
}

// UpdateManyDocuments performs a general update operation on multiple documents
// Same as UpdateDocument but can affect multiple rows
func (c *PostgreSQLClient) UpdateManyDocuments(
	ctx context.Context, filter interface{}, update interface{},
) (*UpdateResult, error) {
	// Implementation is identical to UpdateDocument since SQL UPDATE handles multiple rows by default
	return c.UpdateDocument(ctx, filter, update)
}

// UpsertDocument performs an upsert operation
func (c *PostgreSQLClient) UpsertDocument(
	ctx context.Context, filter interface{}, document interface{},
) (*UpdateResult, error) {
	// For PostgreSQL, we need to determine a unique key from the filter
	// This is complex for generic filters, so we'll use a simpler approach:
	// Try to update first, if no rows affected, insert

	// Try update first
	update := map[string]interface{}{
		"$set": document,
	}

	result, err := c.UpdateDocument(ctx, filter, update)
	if err != nil {
		return nil, err
	}

	// If update matched rows, return the result
	if result.MatchedCount > 0 {
		return result, nil
	}

	// No rows matched, perform insert
	docJSON, err := json.Marshal(document)
	if err != nil {
		return nil, datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to marshal document for upsert",
			err,
		)
	}

	//nolint:gosec // G201: table name from config, using parameterized query ($1)
	query := fmt.Sprintf("INSERT INTO %s (document) VALUES ($1) RETURNING id", c.table)

	var id string

	err = c.db.QueryRowContext(ctx, query, docJSON).Scan(&id)
	if err != nil {
		return nil, datastore.NewInsertError(
			datastore.ProviderPostgreSQL,
			"failed to insert document in upsert",
			err,
		)
	}

	return &UpdateResult{
		MatchedCount:  0,
		ModifiedCount: 0,
		UpsertedCount: 1,
		UpsertedID:    id,
	}, nil
}

// FindOne finds a single document
func (c *PostgreSQLClient) FindOne(
	ctx context.Context,
	filter interface{},
	opts *FindOneOptions,
) (SingleResult, error) {
	// Build the SQL query
	whereClause, args, err := c.buildWhereClause(filter)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // G201: table name from config, whereClause built with parameterized queries
	query := fmt.Sprintf("SELECT id, document FROM %s WHERE %s LIMIT 1", c.table, whereClause)

	// Apply sort options if provided
	if opts != nil && opts.Sort != nil {
		orderBy, err := c.buildOrderByClause(opts.Sort)
		if err != nil {
			return nil, err
		}

		query = fmt.Sprintf("SELECT id, document FROM %s WHERE %s %s LIMIT 1", c.table, whereClause, orderBy)
	}

	row := c.db.QueryRowContext(ctx, query, args...)

	return &postgresqlSingleResult{row: row, table: c.table}, nil
}

// Find finds multiple documents
func (c *PostgreSQLClient) Find(ctx context.Context, filter interface{}, opts *FindOptions) (Cursor, error) {
	// Build the SQL query
	whereClause, args, err := c.buildWhereClause(filter)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // G201: table name from config, whereClause built with parameterized queries
	query := fmt.Sprintf("SELECT id, document FROM %s WHERE %s", c.table, whereClause)

	// Apply options
	//nolint:nestif // Nested complexity 5: required for handling optional sort/limit/skip
	if opts != nil {
		// Sort
		if opts.Sort != nil {
			orderBy, err := c.buildOrderByClause(opts.Sort)
			if err != nil {
				return nil, err
			}

			query += " " + orderBy
		}

		// Limit
		if opts.Limit != nil {
			query += fmt.Sprintf(" LIMIT %d", *opts.Limit)
		}

		// Skip/Offset
		if opts.Skip != nil {
			query += fmt.Sprintf(" OFFSET %d", *opts.Skip)
		}
	}

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to execute find query",
			err,
		)
	}

	return &postgresqlCursor{rows: rows}, nil
}

// CountDocuments counts documents matching the filter
func (c *PostgreSQLClient) CountDocuments(ctx context.Context, filter interface{}, opts *CountOptions) (int64, error) {
	// Build the SQL query
	whereClause, args, err := c.buildWhereClause(filter)
	if err != nil {
		return 0, err
	}

	//nolint:gosec // G201: table name from config, whereClause built with parameterized queries
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", c.table, whereClause)

	// Apply options
	if opts != nil {
		// Limit - restricts the number of documents to count
		if opts.Limit != nil {
			// For PostgreSQL, we need to use a subquery for COUNT with LIMIT
			query = fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE %s LIMIT %d) AS limited",
				c.table, whereClause, *opts.Limit)
		}

		// Skip/Offset - skips documents before counting
		if opts.Skip != nil {
			query = fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE %s OFFSET %d) AS skipped",
				c.table, whereClause, *opts.Skip)
		}

		// If both limit and skip are specified
		if opts.Limit != nil && opts.Skip != nil {
			query = fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE %s LIMIT %d OFFSET %d) AS limited_skipped",
				c.table, whereClause, *opts.Limit, *opts.Skip)
		}
	}

	var count int64

	err = c.db.QueryRowContext(ctx, query, args...).Scan(&count)
	if err != nil {
		return 0, datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to count documents",
			err,
		)
	}

	return count, nil
}

// Aggregate performs an aggregation query
// Supports basic MongoDB aggregation pipeline stages:
// - $match: Filtering (translates to WHERE)
// - $sort: Sorting (translates to ORDER BY)
// - $limit: Limit results
// - $skip: Skip results
// - $count: Count documents
// - $group: Group and aggregate (limited support)
// - $setWindowFields: Window functions with sortBy and output specifications
//
//nolint:gocyclo,cyclop,gocognit // Complexity 11: handles pipeline type conversion and stage routing - acceptable
func (c *PostgreSQLClient) Aggregate(ctx context.Context, pipeline interface{}) (Cursor, error) {
	// Convert pipeline to slice of stages
	var stages []map[string]interface{}

	switch p := pipeline.(type) {
	case []interface{}:
		for i, stage := range p {
			stageMap, ok := stage.(map[string]interface{})
			if !ok {
				return nil, datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					fmt.Sprintf("pipeline stage %d must be a map", i),
					fmt.Errorf("got type %T", stage),
				)
			}

			stages = append(stages, stageMap)
		}
	case []map[string]interface{}:
		stages = p
	case datastore.Pipeline:
		// Convert datastore.Pipeline to []map[string]interface{}
		for _, doc := range p {
			stageMap := make(map[string]interface{})
			for _, elem := range doc {
				stageMap[elem.Key] = c.convertDatastoreValue(elem.Value)
			}

			stages = append(stages, stageMap)
		}
	default:
		return nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"pipeline must be []interface{}, []map[string]interface{}, or datastore.Pipeline",
			fmt.Errorf("got type %T", pipeline),
		)
	}

	// Build SQL query from pipeline stages
	query, args, err := c.buildAggregationQuery(stages)
	if err != nil {
		return nil, err
	}

	// Execute query
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to execute aggregation query",
			err,
		)
	}

	return &postgresqlCursor{rows: rows}, nil
}

// NewChangeStreamWatcher creates a new change stream watcher
// Uses polling-based approach with the datastore_changelog table
func (c *PostgreSQLClient) NewChangeStreamWatcher(
	ctx context.Context, tokenConfig TokenConfig, pipeline interface{},
) (ChangeStreamWatcher, error) {
	// Convert pipeline to slice of stages
	var stages []map[string]interface{}

	switch p := pipeline.(type) {
	case []interface{}:
		for i, stage := range p {
			stageMap, ok := stage.(map[string]interface{})
			if !ok {
				return nil, datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					fmt.Sprintf("pipeline stage %d must be a map", i),
					fmt.Errorf("got type %T", stage),
				)
			}

			stages = append(stages, stageMap)
		}
	case []map[string]interface{}:
		stages = p
	case datastore.Pipeline:
		// Convert datastore.Pipeline to []map[string]interface{}
		for _, doc := range p {
			stageMap := make(map[string]interface{})
			for _, elem := range doc {
				stageMap[elem.Key] = c.convertDatastoreValue(elem.Value)
			}

			stages = append(stages, stageMap)
		}
	case nil:
		// Empty pipeline is OK
		stages = []map[string]interface{}{}
	default:
		return nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"pipeline must be []interface{}, []map[string]interface{}, or datastore.Pipeline",
			fmt.Errorf("got type %T", pipeline),
		)
	}

	watcher := &PostgreSQLChangeStreamWatcher{
		db:          c.db,
		table:       c.table,
		tokenConfig: tokenConfig,
		pipeline:    stages,
	}

	return watcher, nil
}

// PostgreSQL-specific helper methods

// buildWhereClause converts MongoDB-style filters to PostgreSQL WHERE clause
// Supports basic equality filters on JSONB document fields
func (c *PostgreSQLClient) buildWhereClause(filter interface{}) (string, []interface{}, error) {
	// Handle nil/empty filter
	if filter == nil {
		return "TRUE", []interface{}{}, nil
	}

	filterMap, ok := filter.(map[string]interface{})
	if !ok {
		return "", nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"filter must be a map[string]interface{}",
			fmt.Errorf("got type %T", filter),
		)
	}

	if len(filterMap) == 0 {
		return "TRUE", []interface{}{}, nil
	}

	var (
		conditions []string
		args       []interface{}
	)

	paramCount := 1

	for key, value := range filterMap {
		// Handle special operators
		if key == "$expr" {
			// Handle $expr operator which allows aggregation expressions in match
			exprCondition, err := c.buildExprCondition(value)
			if err != nil {
				return "", nil, err
			}

			conditions = append(conditions, exprCondition)

			continue
		}

		// Check if value is a map containing comparison operators
		if valueMap, ok := value.(map[string]interface{}); ok {
			// Handle comparison operators: {"count": {"$gte": 5}} → document->>'count' >= 5
			jsonPath := c.buildJSONPath(key)

			condition, valueArgs, err := c.buildFieldComparison(jsonPath, valueMap, paramCount)
			if err != nil {
				return "", nil, err
			}

			conditions = append(conditions, condition)
			args = append(args, valueArgs...)
			paramCount += len(valueArgs)

			continue
		}

		// Simple equality check on JSONB fields
		// Example: {"nodeName": "node-1"} → document->>'nodeName' = $1
		// Example: {"healthevent.nodename": "node-1"} → document->'healthevent'->>'nodename' = $1
		jsonPath := c.buildJSONPath(key)
		conditions = append(conditions, fmt.Sprintf("%s = $%d", jsonPath, paramCount))
		args = append(args, value)
		paramCount++
	}

	whereClause := strings.Join(conditions, " AND ")

	return whereClause, args, nil
}

// buildFieldComparison builds a comparison condition for a field with operators
// Handles operators like $gte, $gt, $lte, $lt, $eq, $ne
func (c *PostgreSQLClient) buildFieldComparison(
	jsonPath string,
	operators map[string]interface{},
	startParam int,
) (string, []interface{}, error) {
	var (
		conditions []string
		args       []interface{}
	)

	paramCount := startParam

	for op, value := range operators {
		var sqlOp string

		switch op {
		case "$gte":
			sqlOp = ">="
		case "$gt":
			sqlOp = ">"
		case opLTE:
			sqlOp = "<="
		case "$lt":
			sqlOp = "<"
		case opEQ:
			sqlOp = "="
		case "$ne":
			sqlOp = "!="
		default:
			return "", nil, datastore.NewQueryError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("unsupported comparison operator: %s", op),
				nil,
			)
		}

		conditions = append(conditions, fmt.Sprintf("%s %s $%d", jsonPath, sqlOp, paramCount))
		args = append(args, value)
		paramCount++
	}

	condition := strings.Join(conditions, " AND ")

	return condition, args, nil
}

// buildExprCondition converts MongoDB $expr operator to PostgreSQL SQL
// This handles aggregation expressions used in $match stages
//
//nolint:cyclop,gocognit // Complexity acceptable: handles logical and comparison operators in $match $expr
func (c *PostgreSQLClient) buildExprCondition(expr interface{}) (string, error) {
	exprMap, ok := expr.(map[string]interface{})
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$expr value must be a map",
			fmt.Errorf("got type %T", expr),
		)
	}

	// Handle logical and comparison operators
	for op, value := range exprMap {
		switch op {
		case "$and":
			// Handle logical AND
			andArray, ok := value.([]interface{})
			if !ok {
				return "", datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					"$and operand must be an array",
					fmt.Errorf("got type %T", value),
				)
			}

			if len(andArray) == 0 {
				return "", datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					"$and must have at least one expression",
					nil,
				)
			}

			var conditions []string

			for i, andExpr := range andArray {
				condition, err := c.buildExprCondition(andExpr)
				if err != nil {
					return "", fmt.Errorf("failed to build $and expression %d: %w", i, err)
				}

				conditions = append(conditions, condition)
			}

			return fmt.Sprintf("(%s)", strings.Join(conditions, " AND ")), nil

		case "$or":
			// Handle logical OR
			orArray, ok := value.([]interface{})
			if !ok {
				return "", datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					"$or operand must be an array",
					fmt.Errorf("got type %T", value),
				)
			}

			if len(orArray) == 0 {
				return "", datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					"$or must have at least one expression",
					nil,
				)
			}

			var conditions []string

			for i, orExpr := range orArray {
				condition, err := c.buildExprCondition(orExpr)
				if err != nil {
					return "", fmt.Errorf("failed to build $or expression %d: %w", i, err)
				}

				conditions = append(conditions, condition)
			}

			return fmt.Sprintf("(%s)", strings.Join(conditions, " OR ")), nil

		case "$gte":
			return c.buildComparisonExpr(">=", value)
		case "$gt":
			return c.buildComparisonExpr(">", value)
		case opLTE:
			return c.buildComparisonExpr("<=", value)
		case "$lt":
			return c.buildComparisonExpr("<", value)
		case opEQ:
			return c.buildComparisonExpr("=", value)
		case "$ne":
			return c.buildComparisonExpr("!=", value)
		case "$in":
			// Handle $in operator for arrays
			inArray, ok := value.([]interface{})
			if !ok || len(inArray) != 2 {
				return "", datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					"$in operator must have exactly 2 operands [field, array]",
					fmt.Errorf("got %d operands", len(inArray)),
				)
			}

			fieldExpr, err := c.buildExprValue(inArray[0])
			if err != nil {
				return "", fmt.Errorf("failed to build $in field expression: %w", err)
			}

			// Build the array expression with special handling for field references
			// to preserve JSONB type
			var arrayExpr string

			if fieldRef, ok := inArray[1].(string); ok && strings.HasPrefix(fieldRef, "$") {
				fieldPath := strings.TrimPrefix(fieldRef, "$")
				arrayExpr = c.buildJSONPathAsJSONB(fieldPath)
			} else {
				arrayExpr, err = c.buildExprValue(inArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build $in array expression: %w", err)
				}
			}

			return fmt.Sprintf("%s = ANY(SELECT jsonb_array_elements_text(%s))", fieldExpr, arrayExpr), nil

		default:
			return "", datastore.NewQueryError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("unsupported $expr operator: %s", op),
				nil,
			)
		}
	}

	return "", datastore.NewValidationError(
		datastore.ProviderPostgreSQL,
		"$expr must contain a comparison or logical operator",
		nil,
	)
}

// buildComparisonExpr builds a comparison expression from an array of [leftExpr, rightExpr]
func (c *PostgreSQLClient) buildComparisonExpr(op string, value interface{}) (string, error) {
	valueArray, ok := value.([]interface{})
	if !ok || len(valueArray) != 2 {
		return "", datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"comparison operator must have exactly 2 operands",
			fmt.Errorf("got %v", value),
		)
	}

	leftSQL, err := c.buildExprValue(valueArray[0])
	if err != nil {
		return "", err
	}

	rightSQL, err := c.buildExprValue(valueArray[1])
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s %s %s", leftSQL, op, rightSQL), nil
}

// buildExprValue converts a MongoDB expression value to SQL
//
//nolint:cyclop,gocognit // Complexity acceptable for expression building with many operators
func (c *PostgreSQLClient) buildExprValue(value interface{}) (string, error) {
	switch v := value.(type) {
	case string:
		// Field reference like "$healthevent.generatedtimestamp.seconds"
		if strings.HasPrefix(v, "$") {
			fieldPath := strings.TrimPrefix(v, "$")

			// Use buildJSONPathWithCast for backward compatibility
			// This will be overridden in specific operators that need JSONB preservation
			return c.buildJSONPathWithCast(fieldPath), nil
		}

		// Plain string literal - return as SQL string literal
		// This handles cases like {"$in": ["79", "$arrayField"]} where "79" is a literal value
		// Escape single quotes to prevent SQL injection
		escaped := strings.ReplaceAll(v, "'", "''")

		return fmt.Sprintf("'%s'", escaped), nil
	case map[string]interface{}:
		// Handle operators like $subtract, $divide, $toLong, $size, $arrayElemAt, etc.
		for op, operand := range v {
			slog.Debug("Building expression for operator",
				"operator", op,
				"operand", operand)

			switch op {
			case "$subtract":
				return c.buildArithmeticExpr("-", operand)
			case "$divide":
				return c.buildArithmeticExpr("/", operand)
			case "$toLong":
				// $toLong converts to integer, but we need to handle $$NOW specially
				if operandStr, ok := operand.(string); ok && operandStr == "$$NOW" {
					return "EXTRACT(EPOCH FROM NOW())::bigint * 1000", nil // MongoDB NOW is in milliseconds
				}

				return "", datastore.NewQueryError(
					datastore.ProviderPostgreSQL,
					fmt.Sprintf("unsupported $toLong operand: %v", operand),
					nil,
				)
			case "$size":
				// $size returns the number of elements in an array
				// PostgreSQL: jsonb_array_length(expression)
				// The operand can be either a field reference or a complex expression
				// Try to build the operand as an expression (could be $setIntersection, $map, etc.)
				arrayExpr, err := c.buildExprValue(operand)
				if err != nil {
					return "", fmt.Errorf("failed to build $size operand: %w", err)
				}

				sql := fmt.Sprintf("jsonb_array_length(%s)", arrayExpr)

				slog.Debug("Built $size expression",
					"operand", operand,
					"sql", sql)

				return sql, nil
			case "$arrayElemAt":
				// $arrayElemAt returns element at specified index: [array, index]
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) != 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$arrayElemAt must have exactly 2 operands: [array, index]",
						fmt.Errorf("got %v", operand),
					)
				}

				// Build array expression
				arrayExpr, err := c.buildExprValue(operandArray[0])
				if err != nil {
					return "", fmt.Errorf("failed to build array expression: %w", err)
				}

				// Build index expression
				indexExpr, err := c.buildExprValue(operandArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build index expression: %w", err)
				}

				// PostgreSQL: (array_field->index)::text
				// Note: PostgreSQL arrays are 0-indexed like MongoDB
				sql := fmt.Sprintf("(%s->%s)::text", arrayExpr, indexExpr)

				slog.Debug("Built $arrayElemAt expression",
					"array", operandArray[0],
					"index", operandArray[1],
					"sql", sql)

				return sql, nil
			case "$ifNull":
				// $ifNull returns first non-null value: [expr, defaultValue]
				// PostgreSQL: COALESCE(expr, defaultValue)
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) != 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$ifNull must have exactly 2 operands: [expr, defaultValue]",
						fmt.Errorf("got %v", operand),
					)
				}

				// Special handling for field references to preserve JSONB type
				// If the first operand is a field reference (string starting with $),
				// use buildJSONPathAsJSONB to avoid casting arrays/objects to bigint
				var exprSQL string

				var err error

				if fieldRef, ok := operandArray[0].(string); ok && strings.HasPrefix(fieldRef, "$") {
					fieldPath := strings.TrimPrefix(fieldRef, "$")
					exprSQL = c.buildJSONPathAsJSONB(fieldPath)
				} else {
					exprSQL, err = c.buildExprValue(operandArray[0])
					if err != nil {
						return "", fmt.Errorf("failed to build $ifNull expression: %w", err)
					}
				}

				defaultSQL, err := c.buildExprValue(operandArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build $ifNull default: %w", err)
				}

				sql := fmt.Sprintf("COALESCE(%s, %s)", exprSQL, defaultSQL)

				slog.Debug("Built $ifNull expression",
					"expression", operandArray[0],
					"default", operandArray[1],
					"sql", sql)

				return sql, nil
			case "$filter":
				// $filter filters array elements: {input: array, cond: condition, as: varName}
				// PostgreSQL: (SELECT jsonb_agg(elem) FROM jsonb_array_elements(input) AS elem WHERE condition)
				operandMap, ok := operand.(map[string]interface{})
				if !ok {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$filter must have a map with 'input' and 'cond' fields",
						fmt.Errorf("got %v", operand),
					)
				}

				inputExpr, err := c.buildExprValue(operandMap["input"])
				if err != nil {
					return "", fmt.Errorf("failed to build $filter input: %w", err)
				}

				// The condition uses $$this to reference the current element
				// We need to replace $$this with the elem variable
				condExpr, err := c.buildFilterCondition(operandMap["cond"])
				if err != nil {
					return "", fmt.Errorf("failed to build $filter condition: %w", err)
				}

				sql := fmt.Sprintf(
					"(SELECT COALESCE(jsonb_agg(elem), '[]'::jsonb) FROM jsonb_array_elements(%s) AS elem WHERE %s)",
					inputExpr, condExpr,
				)

				slog.Debug("Built $filter expression",
					"input", operandMap["input"],
					"cond", operandMap["cond"],
					"sql", sql)

				return sql, nil
			case "$map":
				// $map transforms array elements: {input: array, in: expr, as: varName}
				// PostgreSQL: (SELECT jsonb_agg(expression) FROM jsonb_array_elements(input) AS elem)
				operandMap, ok := operand.(map[string]interface{})
				if !ok {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$map must have a map with 'input' and 'in' fields",
						fmt.Errorf("got %v", operand),
					)
				}

				inputExpr, err := c.buildExprValue(operandMap["input"])
				if err != nil {
					return "", fmt.Errorf("failed to build $map input: %w", err)
				}

				// The 'in' expression uses $$this to reference the current element
				inExpr, err := c.buildMapExpression(operandMap["in"])
				if err != nil {
					return "", fmt.Errorf("failed to build $map 'in' expression: %w", err)
				}

				sql := fmt.Sprintf(
					"(SELECT COALESCE(jsonb_agg(%s), '[]'::jsonb) FROM jsonb_array_elements(%s) AS elem)",
					inExpr, inputExpr,
				)

				slog.Debug("Built $map expression",
					"input", operandMap["input"],
					"in", operandMap["in"],
					"sql", sql)

				return sql, nil
			case "$setIntersection":
				// $setIntersection returns common elements: [array1, array2, ...]
				// PostgreSQL: We'll use a subquery with array intersection logic
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) < 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$setIntersection must have at least 2 array operands",
						fmt.Errorf("got %v", operand),
					)
				}

				// Build SQL for first array
				firstSQL, err := c.buildExprValue(operandArray[0])
				if err != nil {
					return "", fmt.Errorf("failed to build first $setIntersection array: %w", err)
				}

				// Build SQL for second array
				secondSQL, err := c.buildExprValue(operandArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build second $setIntersection array: %w", err)
				}

				// PostgreSQL: Find intersection using jsonb elements
				// SELECT jsonb_agg(DISTINCT elem) FROM jsonb_array_elements(array1) AS elem
				// WHERE elem IN (SELECT jsonb_array_elements(array2))
				sql := fmt.Sprintf(
					"(SELECT COALESCE(jsonb_agg(DISTINCT elem), '[]'::jsonb) "+
						"FROM jsonb_array_elements(%s) AS elem "+
						"WHERE elem IN (SELECT jsonb_array_elements(%s)))",
					firstSQL, secondSQL,
				)

				slog.Debug("Built $setIntersection expression",
					"arrays", operandArray,
					"sql", sql)

				return sql, nil
			case opEQ:
				// $eq compares two values for equality: [value1, value2]
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) != 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$eq must have exactly 2 operands",
						fmt.Errorf("got %v", operand),
					)
				}

				leftSQL, err := c.buildExprValue(operandArray[0])
				if err != nil {
					return "", fmt.Errorf("failed to build $eq left operand: %w", err)
				}

				rightSQL, err := c.buildExprValue(operandArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build $eq right operand: %w", err)
				}

				sql := fmt.Sprintf("(%s = %s)", leftSQL, rightSQL)

				slog.Debug("Built $eq expression",
					"left", operandArray[0],
					"right", operandArray[1],
					"sql", sql)

				return sql, nil
			case "$in":
				// $in checks if a value is in an array: [value, array]
				// PostgreSQL: value IN (array) or for JSONB: value = ANY(array)
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) != 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$in must have exactly 2 operands: [value, array]",
						fmt.Errorf("got %v", operand),
					)
				}

				// Build the value expression
				valueSQL, err := c.buildExprValue(operandArray[0])
				if err != nil {
					return "", fmt.Errorf("failed to build $in value operand: %w", err)
				}

				// Build the array expression with special handling for field references
				// to preserve JSONB type
				var arraySQL string

				if fieldRef, ok := operandArray[1].(string); ok && strings.HasPrefix(fieldRef, "$") {
					fieldPath := strings.TrimPrefix(fieldRef, "$")
					arraySQL = c.buildJSONPathAsJSONB(fieldPath)
				} else {
					arraySQL, err = c.buildExprValue(operandArray[1])
					if err != nil {
						return "", fmt.Errorf("failed to build $in array operand: %w", err)
					}
				}

				// For JSONB arrays, we need to use the @> operator or convert to text array
				// Using: (array @> to_jsonb(value))
				// This checks if the JSONB array contains the value
				sql := fmt.Sprintf("(%s @> to_jsonb(%s))", arraySQL, valueSQL)

				slog.Debug("Built $in expression",
					"value", operandArray[0],
					"array", operandArray[1],
					"sql", sql)

				return sql, nil
			case "$and":
				// $and performs logical AND on array of expressions: [expr1, expr2, ...]
				// PostgreSQL: (expr1 AND expr2 AND ...)
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) < 1 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$and must have at least 1 expression",
						fmt.Errorf("got %v", operand),
					)
				}

				// Build all expressions
				var expressions []string

				for i, expr := range operandArray {
					exprSQL, err := c.buildExprValue(expr)
					if err != nil {
						return "", fmt.Errorf("failed to build $and expression %d: %w", i, err)
					}

					expressions = append(expressions, exprSQL)
				}

				sql := fmt.Sprintf("(%s)", strings.Join(expressions, " AND "))

				slog.Debug("Built $and expression",
					"operandCount", len(operandArray),
					"sql", sql)

				return sql, nil
			case "$anyElementTrue":
				// $anyElementTrue returns true if any element in array evaluates to true
				// PostgreSQL: (SELECT bool_or((value)::text::boolean) FROM jsonb_array_elements(<array_expr>) AS value)
				// Build the array expression
				arrayExpr, err := c.buildExprValue(operand)
				if err != nil {
					return "", fmt.Errorf("failed to build $anyElementTrue array expression: %w", err)
				}

				// Use bool_or to check if any element is true
				// We need to convert JSONB elements to text, then to boolean
				sql := fmt.Sprintf(
					"(SELECT COALESCE(bool_or((value)::text::boolean), false) "+
						"FROM jsonb_array_elements(%s) AS value)",
					arrayExpr,
				)

				slog.Debug("Built $anyElementTrue expression",
					"operand", operand,
					"sql", sql)

				return sql, nil
			case opLTE:
				// $lte performs less than or equal comparison: [expr1, expr2]
				// PostgreSQL: (expr1 <= expr2)
				operandArray, ok := operand.([]interface{})
				if !ok || len(operandArray) != 2 {
					return "", datastore.NewValidationError(
						datastore.ProviderPostgreSQL,
						"$lte must have exactly 2 operands",
						fmt.Errorf("got %v", operand),
					)
				}

				leftSQL, err := c.buildExprValue(operandArray[0])
				if err != nil {
					return "", fmt.Errorf("failed to build $lte left operand: %w", err)
				}

				rightSQL, err := c.buildExprValue(operandArray[1])
				if err != nil {
					return "", fmt.Errorf("failed to build $lte right operand: %w", err)
				}

				sql := fmt.Sprintf("(%s <= %s)", leftSQL, rightSQL)

				slog.Debug("Built $lte expression",
					"left", operandArray[0],
					"right", operandArray[1],
					"sql", sql)

				return sql, nil
			default:
				slog.Warn("Unsupported expression operator",
					"operator", op,
					"operand", operand)

				return "", datastore.NewQueryError(
					datastore.ProviderPostgreSQL,
					fmt.Sprintf("unsupported expression operator: %s", op),
					nil,
				)
			}
		}
	case int, int64, float64:
		// Literal number
		return fmt.Sprintf("%v", v), nil
	case bool:
		// Literal boolean
		if v {
			return "true", nil
		}

		return "false", nil
	case []interface{}:
		// Array literal (e.g., empty array [] or array with values)
		// Convert to PostgreSQL JSONB array
		if len(v) == 0 {
			// Empty array
			return "'[]'::jsonb", nil
		}

		// Build array elements
		var elements []string

		for _, elem := range v {
			// Handle different element types
			switch elemVal := elem.(type) {
			case string:
				// String literals in arrays - quote them for jsonb_build_array
				elements = append(elements, fmt.Sprintf("'%s'", elemVal))
			case map[string]interface{}:
				// Convert map to JSON string
				jsonBytes, err := json.Marshal(elemVal)
				if err != nil {
					return "", fmt.Errorf("failed to marshal array element: %w", err)
				}

				elements = append(elements, fmt.Sprintf("'%s'::jsonb", string(jsonBytes)))
			default:
				// For other types (numbers, booleans, expressions), try to convert them to expressions
				elemSQL, err := c.buildExprValue(elem)
				if err != nil {
					return "", fmt.Errorf("failed to build array element: %w", err)
				}

				elements = append(elements, elemSQL)
			}
		}

		// Build JSONB array: jsonb_build_array(elem1, elem2, ...)
		return fmt.Sprintf("jsonb_build_array(%s)", strings.Join(elements, ", ")), nil
	case nil:
		// Null value
		return "NULL", nil
	}

	return "", datastore.NewValidationError(
		datastore.ProviderPostgreSQL,
		fmt.Sprintf("unsupported expression value type: %T", value),
		nil,
	)
}

// buildArithmeticExpr builds an arithmetic expression
func (c *PostgreSQLClient) buildArithmeticExpr(op string, operand interface{}) (string, error) {
	operandArray, ok := operand.([]interface{})
	if !ok || len(operandArray) != 2 {
		return "", datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"arithmetic operator must have exactly 2 operands",
			fmt.Errorf("got %v", operand),
		)
	}

	leftSQL, err := c.buildExprValue(operandArray[0])
	if err != nil {
		return "", err
	}

	rightSQL, err := c.buildExprValue(operandArray[1])
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("(%s %s %s)", leftSQL, op, rightSQL), nil
}

// buildFilterCondition builds a filter condition, replacing $$this with elem
func (c *PostgreSQLClient) buildFilterCondition(cond interface{}) (string, error) {
	// The condition is typically a map with operators like $eq
	// Example: {"$eq": ["$$this.entitytype", "GPU"]}
	// We need to replace $$this with elem
	condMap, ok := cond.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("filter condition must be a map, got %T", cond)
	}

	// Handle the operator in the condition
	for op, operand := range condMap {
		if op == opEQ {
			operandArray, ok := operand.([]interface{})
			if !ok || len(operandArray) != 2 {
				return "", fmt.Errorf("$eq in filter must have 2 operands")
			}

			// Build both sides, replacing $$this references
			left, err := c.buildFilterOperand(operandArray[0])
			if err != nil {
				return "", err
			}

			right, err := c.buildFilterOperand(operandArray[1])
			if err != nil {
				return "", err
			}

			return fmt.Sprintf("(%s = %s)", left, right), nil
		}
	}

	return "", fmt.Errorf("unsupported filter condition operator")
}

// buildFilterOperand builds a filter operand, replacing $$this with elem
func (c *PostgreSQLClient) buildFilterOperand(operand interface{}) (string, error) {
	switch v := operand.(type) {
	case string:
		if strings.HasPrefix(v, "$$this.") {
			// Replace $$this with elem
			fieldPath := strings.TrimPrefix(v, "$$this.")
			parts := strings.Split(fieldPath, ".")

			// Build JSON path: elem->'field'->>'subfield'

			path := "elem"

			for i, part := range parts {
				if i == len(parts)-1 {
					// Last part: use ->> for text extraction
					path = fmt.Sprintf("%s->>'%s'", path, part)
				} else {
					// Intermediate parts: use -> for json navigation
					path = fmt.Sprintf("%s->'%s'", path, part)
				}
			}

			return path, nil
		}
		// Regular string literal
		return fmt.Sprintf("'%s'", v), nil
	default:
		return "", fmt.Errorf("unsupported filter operand type: %T", operand)
	}
}

// buildMapExpression builds a map expression, replacing $$this with elem
func (c *PostgreSQLClient) buildMapExpression(expr interface{}) (string, error) {
	// Example: "$$this.entityvalue"
	switch v := expr.(type) {
	case string:
		if strings.HasPrefix(v, "$$this.") {
			// Replace $$this with elem
			fieldPath := strings.TrimPrefix(v, "$$this.")
			parts := strings.Split(fieldPath, ".")

			// Build JSON path: elem->'field'->>'subfield'

			path := "elem"

			for i, part := range parts {
				if i == len(parts)-1 {
					// Last part: use ->> for text extraction, then wrap in to_jsonb for aggregation
					path = fmt.Sprintf("to_jsonb(%s->>'%s')", path, part)
				} else {
					// Intermediate parts: use -> for json navigation
					path = fmt.Sprintf("%s->'%s'", path, part)
				}
			}

			return path, nil
		}
		// Regular string literal
		return fmt.Sprintf("to_jsonb('%s')", v), nil
	default:
		return "", fmt.Errorf("unsupported map expression type: %T", expr)
	}
}

// buildJSONPathWithCast builds a JSON path with type casting for numeric comparisons.
// Field names are normalized from lowercase MongoDB-style to camelCase PostgreSQL-style.
func (c *PostgreSQLClient) buildJSONPathWithCast(fieldPath string) string {
	parts := strings.Split(fieldPath, ".")

	if len(parts) == 1 {
		// Simple field: (document->>'fieldName')::bigint
		normalizedPart := normalizeFieldName(parts[0])

		return fmt.Sprintf("(%s->>'%s')::bigint", jsonbDocumentColumn, normalizedPart)
	}

	// Nested field: (document->'path'->'to'->>'field')::bigint
	path := jsonbDocumentColumn

	for i, part := range parts {
		normalizedPart := normalizeFieldName(part)
		if i == len(parts)-1 {
			// Last part: use ->> to get text value, then cast to bigint
			path = fmt.Sprintf("(%s->>'%s')::bigint", path, normalizedPart)
		} else {
			// Intermediate parts: use -> to navigate JSON
			path = fmt.Sprintf("%s->'%s'", path, normalizedPart)
		}
	}

	return path
}

// fieldNameMapping maps lowercase MongoDB-style field names to the camelCase field names
// used in PostgreSQL JSON documents. MongoDB queries use lowercase field names, but the
// actual JSON documents stored in PostgreSQL use camelCase (from the protobuf definitions).
//
// NOTE: healtheventstatus fields (faultremediated, nodequarantined, userpodsevictionstatus)
// are stored in lowercase in the JSONB document, so they are NOT mapped here.
var fieldNameMapping = map[string]string{
	// healthevent fields - these use camelCase in the JSONB document
	"nodename":           "nodeName",
	"checkname":          "checkName",
	"isfatal":            "isFatal",
	"ishealthy":          "isHealthy",
	"errorcode":          "errorCode",
	"componentclass":     "componentClass",
	"entitiesimpacted":   "entitiesImpacted",
	"generatedtimestamp": "generatedTimestamp",
	"recommendedaction":  "recommendedAction",
	"entitytype":         "entityType",
	"entityvalue":        "entityValue",
	// timestamp fields
	"createdat": "createdAt",
	"updatedat": "updatedAt",
	// NOTE: healtheventstatus fields are NOT mapped because they are stored
	// in lowercase in the JSONB document:
	// - faultremediated (NOT faultRemediated)
	// - nodequarantined (NOT nodeQuarantined)
	// - userpodsevictionstatus (NOT userPodsEvictionStatus)
}

// normalizeFieldName converts a lowercase MongoDB-style field name to the camelCase
// field name used in PostgreSQL JSON documents.
func normalizeFieldName(fieldName string) string {
	if mapped, ok := fieldNameMapping[fieldName]; ok {
		return mapped
	}

	return fieldName
}

// buildJSONPathAsJSONB converts a MongoDB-style field path to PostgreSQL JSONB path expression
// that preserves JSONB type (using -> for all parts, including the last one).
// This is used in aggregation expressions where we need to preserve arrays/objects.
// Examples:
//
//	"nodeName" → "document->'nodeName'"
//	"healthevent.entitiesimpacted" → "document->'healthevent'->'entitiesImpacted'"
//	"status.metadata" → "document->'status'->'metadata'"
func (c *PostgreSQLClient) buildJSONPathAsJSONB(fieldPath string) string {
	parts := strings.Split(fieldPath, ".")
	path := jsonbDocumentColumn

	for _, part := range parts {
		// Normalize field name to camelCase and use -> to keep JSONB type
		normalizedPart := normalizeFieldName(part)
		path = fmt.Sprintf("%s->'%s'", path, normalizedPart)
	}

	return path
}

// buildJSONPath converts a MongoDB-style field path to PostgreSQL JSONB path expression
// Field names are normalized from lowercase MongoDB-style to camelCase PostgreSQL-style.
// Examples:
//
//	"nodeName" → "document->>'nodeName'"
//	"healthevent.nodename" → "document->'healthevent'->>'nodeName'"
//	"status.message" → "document->'status'->>'message'"
func (c *PostgreSQLClient) buildJSONPath(fieldPath string) string {
	parts := strings.Split(fieldPath, ".")

	if len(parts) == 1 {
		// Simple field: document->>'fieldName'
		normalizedPart := normalizeFieldName(parts[0])

		return fmt.Sprintf("%s->>'%s'", jsonbDocumentColumn, normalizedPart)
	}

	// Nested field: document->'path'->'to'->>'field'
	// All intermediate parts use '->' (returns JSONB)
	// Final part uses '->>' (returns text)
	path := jsonbDocumentColumn

	for i, part := range parts {
		normalizedPart := normalizeFieldName(part)
		if i == len(parts)-1 {
			// Last part: use ->> to get text value
			path = fmt.Sprintf("%s->>'%s'", path, normalizedPart)
		} else {
			// Intermediate parts: use -> to navigate JSON
			path = fmt.Sprintf("%s->'%s'", path, normalizedPart)
		}
	}

	return path
}

// buildOrderByClause converts sort options to PostgreSQL ORDER BY clause
func (c *PostgreSQLClient) buildOrderByClause(sort interface{}) (string, error) {
	sortMap, ok := sort.(map[string]interface{})
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"sort must be a map[string]interface{}",
			fmt.Errorf("got type %T", sort),
		)
	}

	var orderClauses []string

	for field, direction := range sortMap {
		jsonPath := c.buildJSONPath(field)

		// Convert direction to SQL (1 = ASC, -1 = DESC)
		dir := "ASC"
		if dirInt, ok := direction.(int); ok && dirInt < 0 {
			dir = orderDESC
		}

		orderClauses = append(orderClauses, fmt.Sprintf("%s %s", jsonPath, dir))
	}

	if len(orderClauses) == 0 {
		return "", nil
	}

	return "ORDER BY " + strings.Join(orderClauses, ", "), nil
}

// convertDatastoreValue recursively converts datastore types to standard Go types
func (c *PostgreSQLClient) convertDatastoreValue(value interface{}) interface{} {
	switch v := value.(type) {
	case datastore.Document:
		result := make(map[string]interface{})
		for _, elem := range v {
			result[elem.Key] = c.convertDatastoreValue(elem.Value)
		}

		return result
	case datastore.Array:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = c.convertDatastoreValue(item)
		}

		return result
	default:
		return v
	}
}

// buildAggregationQuery builds SQL query from MongoDB aggregation pipeline stages
func (c *PostgreSQLClient) buildAggregationQuery(stages []map[string]interface{}) (string, []interface{}, error) {
	builder := &aggregationQueryBuilder{
		client: c,
		query:  fmt.Sprintf("SELECT id, document FROM %s", c.table),
	}

	for i, stage := range stages {
		if err := builder.processStage(i, stage); err != nil {
			return "", nil, err
		}
	}

	return builder.buildFinalQuery(), builder.args, nil
}

// aggregationQueryBuilder helps build aggregation queries with reduced complexity
type aggregationQueryBuilder struct {
	client       *PostgreSQLClient
	query        string
	args         []interface{}
	whereClauses []string
	orderBy      string
	limit        string
	offset       string
	isCount      bool
	countField   string
	groupBy      map[string]interface{}
	windowFields *windowFieldsSpec
	addFields    map[string]interface{} // Fields to add via $addFields
	// postCountMatch stores $match conditions that come AFTER $count
	// These filter the count result, not the source rows
	postCountMatch map[string]interface{}
}

// windowFieldsSpec holds the specification for $setWindowFields
type windowFieldsSpec struct {
	sortBy map[string]interface{}
	output map[string]windowFieldOutput
}

// windowFieldOutput represents a single output field in $setWindowFields
type windowFieldOutput struct {
	operator string                 // $push, $sum, $max, etc.
	operand  interface{}            // the value to aggregate
	window   map[string]interface{} // window specification
}

//nolint:cyclop // Complexity 11: handles multiple aggregation operators - acceptable
func (b *aggregationQueryBuilder) processStage(stageIndex int, stage map[string]interface{}) error {
	if len(stage) != 1 {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			fmt.Sprintf("stage %d must have exactly one operator", stageIndex),
			fmt.Errorf("got %d keys", len(stage)),
		)
	}

	for operator, value := range stage {
		switch operator {
		case "$match":
			return b.processMatch(value)
		case "$sort":
			return b.processSort(value)
		case "$limit":
			return b.processLimit(value)
		case "$skip":
			return b.processSkip(value)
		case "$count":
			return b.processCount(value)
		case "$group":
			return b.processGroup(value)
		case "$setWindowFields":
			return b.processSetWindowFields(value)
		case "$addFields":
			return b.processAddFields(value)
		case "$project", "$lookup", "$unwind", "$facet":
			return datastore.NewQueryError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("aggregation operator %s not yet supported", operator),
				fmt.Errorf("complex aggregation requires custom SQL implementation"),
			)
		default:
			return datastore.NewValidationError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("unknown aggregation operator: %s", operator),
				nil,
			)
		}
	}

	return nil
}

func (b *aggregationQueryBuilder) processMatch(value interface{}) error {
	matchMap, ok := value.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$match value must be a map",
			fmt.Errorf("got type %T", value),
		)
	}

	// If $count has already been processed, this is a post-count $match
	// that should filter the count result, not the source rows
	if b.isCount {
		b.postCountMatch = matchMap

		return nil
	}

	whereClause, matchArgs, err := b.client.buildWhereClause(matchMap)
	if err != nil {
		return err
	}

	adjustedClause := b.client.adjustParameterNumbers(whereClause, len(b.args))
	b.whereClauses = append(b.whereClauses, adjustedClause)
	b.args = append(b.args, matchArgs...)

	return nil
}

func (b *aggregationQueryBuilder) processSort(value interface{}) error {
	sortMap, ok := value.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$sort value must be a map",
			fmt.Errorf("got type %T", value),
		)
	}

	orderByClause, err := b.client.buildOrderByClause(sortMap)
	if err != nil {
		return err
	}

	b.orderBy = orderByClause

	return nil
}

func (b *aggregationQueryBuilder) processLimit(value interface{}) error {
	limitVal, err := b.extractIntValue(value, "$limit")
	if err != nil {
		return err
	}

	b.limit = fmt.Sprintf("LIMIT %d", limitVal)

	return nil
}

func (b *aggregationQueryBuilder) processSkip(value interface{}) error {
	skipVal, err := b.extractIntValue(value, "$skip")
	if err != nil {
		return err
	}

	b.offset = fmt.Sprintf("OFFSET %d", skipVal)

	return nil
}

func (b *aggregationQueryBuilder) extractIntValue(value interface{}, operator string) (int, error) {
	if intVal, ok := value.(int); ok {
		return intVal, nil
	}

	if floatVal, ok := value.(float64); ok {
		return int(floatVal), nil
	}

	return 0, datastore.NewValidationError(
		datastore.ProviderPostgreSQL,
		fmt.Sprintf("%s value must be an integer", operator),
		fmt.Errorf("got type %T", value),
	)
}

func (b *aggregationQueryBuilder) processCount(value interface{}) error {
	countField, ok := value.(string)
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$count value must be a string (field name)",
			fmt.Errorf("got type %T", value),
		)
	}

	b.isCount = true
	b.countField = countField

	return nil
}

func (b *aggregationQueryBuilder) processGroup(value interface{}) error {
	groupMap, ok := value.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$group value must be a map",
			fmt.Errorf("got type %T", value),
		)
	}

	b.groupBy = groupMap

	return nil
}

//nolint:cyclop // Complexity acceptable for window fields processing
func (b *aggregationQueryBuilder) processSetWindowFields(value interface{}) error {
	windowMap, ok := value.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$setWindowFields value must be a map",
			fmt.Errorf("got type %T", value),
		)
	}

	spec := &windowFieldsSpec{
		output: make(map[string]windowFieldOutput),
	}

	// Parse sortBy
	if sortBy, hasSortBy := windowMap["sortBy"]; hasSortBy {
		sortByMap, ok := sortBy.(map[string]interface{})
		if !ok {
			return datastore.NewValidationError(
				datastore.ProviderPostgreSQL,
				"sortBy must be a map",

				fmt.Errorf("got type %T", sortBy),
			)
		}

		spec.sortBy = sortByMap
	}

	// Parse output fields
	output, hasOutput := windowMap["output"]
	if !hasOutput {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$setWindowFields must have 'output' field",
			nil,
		)
	}

	outputMap, ok := output.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"output must be a map",
			fmt.Errorf("got type %T", output),
		)
	}

	// Parse each output field
	for fieldName, fieldSpec := range outputMap {
		fieldSpecMap, ok := fieldSpec.(map[string]interface{})
		if !ok {
			return datastore.NewValidationError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("output field '%s' must be a map", fieldName),
				fmt.Errorf("got type %T", fieldSpec),
			)
		}

		var windowOut windowFieldOutput

		// Extract window specification
		if window, hasWindow := fieldSpecMap["window"]; hasWindow {
			windowSpecMap, ok := window.(map[string]interface{})
			if !ok {
				return datastore.NewValidationError(
					datastore.ProviderPostgreSQL,
					fmt.Sprintf("window spec for field '%s' must be a map", fieldName),
					fmt.Errorf("got type %T", window),
				)
			}

			windowOut.window = windowSpecMap
		}

		// Find the operator ($push, $sum, $max, etc.)
		for op, operand := range fieldSpecMap {
			if op == "window" {
				continue // Already processed
			}

			if strings.HasPrefix(op, "$") {
				windowOut.operator = op
				windowOut.operand = operand

				break
			}
		}

		if windowOut.operator == "" {
			return datastore.NewValidationError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("output field '%s' must have a window function operator ($push, $sum, etc.)", fieldName),
				nil,
			)
		}

		spec.output[fieldName] = windowOut
	}

	b.windowFields = spec

	return nil
}

func (b *aggregationQueryBuilder) processAddFields(value interface{}) error {
	// $addFields adds new fields to documents
	// Example: {"$addFields": {"field1": expr1, "field2": expr2}}
	fieldsMap, ok := value.(map[string]interface{})
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$addFields must be a map",
			fmt.Errorf("got type %T", value),
		)
	}

	if len(fieldsMap) == 0 {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$addFields must have at least one field",
			nil,
		)
	}

	b.addFields = fieldsMap

	return nil
}

func (b *aggregationQueryBuilder) buildFinalQuery() string {
	// Handle $count operator
	if b.isCount {
		return b.buildCountQuery()
	}

	// Handle $group operator
	if b.groupBy != nil {
		return b.buildGroupQuery()
	}

	// Handle $setWindowFields operator
	if b.windowFields != nil {
		return b.buildWindowFieldsQuery()
	}

	// Handle $addFields operator
	if b.addFields != nil {
		return b.buildAddFieldsQuery()
	}

	// Standard query
	return b.buildStandardQuery()
}

// buildCountQuery builds the SQL for $count aggregation with optional post-count filtering
func (b *aggregationQueryBuilder) buildCountQuery() string {
	subquery := b.query
	if len(b.whereClauses) > 0 {
		subquery += " WHERE " + strings.Join(b.whereClauses, " AND ")
	}
	// Wrap in a subquery and count
	// Include a dummy 'id' column to match the cursor's Decode() expectations
	countQuery := fmt.Sprintf("SELECT '1' as id, jsonb_build_object('%s', COUNT(*)) as document FROM (%s) as subq",
		b.countField, subquery)

	// If there's a post-count $match, wrap the count query and filter the result
	if b.postCountMatch != nil {
		return b.buildPostCountFilter(countQuery)
	}

	return countQuery
}

// buildStandardQuery builds a standard SELECT query with WHERE, ORDER BY, LIMIT, OFFSET
func (b *aggregationQueryBuilder) buildStandardQuery() string {
	query := b.query

	if len(b.whereClauses) > 0 {
		query += " WHERE " + strings.Join(b.whereClauses, " AND ")
	}

	if b.orderBy != "" {
		query += " " + b.orderBy
	}

	if b.limit != "" {
		query += " " + b.limit
	}

	if b.offset != "" {
		query += " " + b.offset
	}

	return query
}

// buildPostCountFilter wraps a count query with a WHERE clause to filter the count result.
// This handles the MongoDB pattern: $count -> $match (filter on count)
// Example: {$match: {count: {$gte: 5}}} after $count should return empty if count < 5
func (b *aggregationQueryBuilder) buildPostCountFilter(countQuery string) string {
	// Build WHERE conditions for the count result
	conditions := []string{}

	for field, value := range b.postCountMatch {
		condition := b.buildPostCountCondition(field, value)
		if condition != "" {
			conditions = append(conditions, condition)
		}
	}

	if len(conditions) == 0 {
		return countQuery
	}

	// Wrap the count query and apply the filter on the result
	// The count result is in document->>'countField', so we filter on that
	return fmt.Sprintf("SELECT * FROM (%s) as count_result WHERE %s",
		countQuery, strings.Join(conditions, " AND "))
}

// buildPostCountCondition builds a single condition for filtering count results
func (b *aggregationQueryBuilder) buildPostCountCondition(field string, value interface{}) string {
	// The count result is stored as document->>'field'
	// We need to cast it to a number for comparison
	fieldPath := fmt.Sprintf("(document->>'%s')::bigint", field)

	switch v := value.(type) {
	case map[string]interface{}:
		// Handle comparison operators like {$gte: 5}
		for op, opValue := range v {
			if sqlOp := b.mapComparisonOperator(op); sqlOp != "" {
				b.args = append(b.args, opValue)

				return fmt.Sprintf("%s %s $%d", fieldPath, sqlOp, len(b.args))
			}
		}
	default:
		// Direct equality comparison
		b.args = append(b.args, v)

		return fmt.Sprintf("%s = $%d", fieldPath, len(b.args))
	}

	return ""
}

// mapComparisonOperator maps MongoDB comparison operators to SQL operators
func (b *aggregationQueryBuilder) mapComparisonOperator(op string) string {
	switch op {
	case opGTE:
		return ">="
	case opGT:
		return ">"
	case opLTE:
		return "<="
	case opLT:
		return "<"
	case opEQ:
		return "="
	case opNE:
		return "!="
	default:
		return ""
	}
}

func (b *aggregationQueryBuilder) buildGroupQuery() string {
	// For now, implement a simple GROUP BY with COUNT
	// This is sufficient for the MultipleRemediations rule
	subquery := b.query
	if len(b.whereClauses) > 0 {
		subquery += " WHERE " + strings.Join(b.whereClauses, " AND ")
	}

	// Extract _id field for grouping
	idField, hasID := b.groupBy["_id"]
	if !hasID {
		idField = nil
	}

	// Build aggregation fields
	// For now, we always set _id to null in the simplified implementation
	_ = idField // Mark as used for future implementation

	selectFields := []string{}
	selectFields = append(selectFields, "jsonb_build_object('_id', null) as document")

	// Handle aggregation operators in the group stage
	for fieldName, fieldExpr := range b.groupBy {
		if fieldName == "_id" {
			continue // Already handled
		}

		// Check if it's a $sum operator
		if exprMap, ok := fieldExpr.(map[string]interface{}); ok {
			if sumVal, hasSum := exprMap["$sum"]; hasSum {
				if sumVal == 1 {
					// Simple count
					return fmt.Sprintf("SELECT jsonb_build_object('%s', COUNT(*)) as document FROM (%s) as subq",
						fieldName, subquery)
				}
			}
		}
	}

	return fmt.Sprintf("SELECT %s FROM (%s) as subq", strings.Join(selectFields, ", "), subquery)
}

//nolint:gocyclo,cyclop // Complexity acceptable for window function SQL generation
func (b *aggregationQueryBuilder) buildWindowFieldsQuery() string {
	// Build the base query with WHERE clause
	baseQuery := b.query

	if len(b.whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(b.whereClauses, " AND ")
	}

	// Build ORDER BY clause from sortBy
	var orderByClause string

	if b.windowFields.sortBy != nil {
		orderByParts := []string{}

		for fieldPath, direction := range b.windowFields.sortBy {
			jsonPath := b.client.buildJSONPathWithCast(fieldPath)

			dir := "ASC"

			if dirInt, ok := direction.(int); ok && dirInt < 0 {
				dir = orderDESC
			}

			if dirFloat, ok := direction.(float64); ok && dirFloat < 0 {
				dir = orderDESC
			}

			orderByParts = append(orderByParts, fmt.Sprintf("%s %s", jsonPath, dir))
		}

		if len(orderByParts) > 0 {
			orderByClause = "ORDER BY " + strings.Join(orderByParts, ", ")
		}
	}

	// Build SELECT clause with window functions
	selectParts := []string{"id"}

	// Start with the original document, we'll add window function results to it
	documentExpr := "document"

	// Add each window function field to the document
	for fieldName, fieldOutput := range b.windowFields.output {
		windowFuncSQL := b.buildWindowFunction(fieldOutput, orderByClause)

		// Add the window function result to the document JSONB
		// jsonb_set(document, '{fieldName}', window_function_result)
		documentExpr = fmt.Sprintf("jsonb_set(%s, '{%s}', %s)",
			documentExpr, fieldName, windowFuncSQL)
	}

	selectParts = append(selectParts, fmt.Sprintf("%s as document", documentExpr))

	// Build final query
	//nolint:gosec // G201: table name from client config, no user input
	query := fmt.Sprintf("SELECT %s FROM (%s) AS base_query",
		strings.Join(selectParts, ", "), baseQuery)

	// Apply remaining clauses
	if b.orderBy != "" {
		query += " " + b.orderBy
	}

	if b.limit != "" {
		query += " " + b.limit
	}

	if b.offset != "" {
		query += " " + b.offset
	}

	return query
}

func (b *aggregationQueryBuilder) buildWindowFunction(fieldOutput windowFieldOutput, orderByClause string) string {
	// Map MongoDB window functions to PostgreSQL
	var pgFunc string

	switch fieldOutput.operator {
	case "$push":
		// $push with $$ROOT means aggregate all documents
		pgFunc = "jsonb_agg(document)"
	case "$sum":
		// $sum with a conditional expression
		// We need to build the expression and sum it
		if exprVal, err := b.client.buildExprValue(fieldOutput.operand); err == nil {
			pgFunc = fmt.Sprintf("to_jsonb(SUM(%s))", exprVal)
		} else {
			// Fallback: simple sum
			pgFunc = "to_jsonb(SUM(1))"
		}
	case "$max":
		// $max with a field reference
		if fieldRef, ok := fieldOutput.operand.(string); ok && strings.HasPrefix(fieldRef, "$") {
			fieldPath := strings.TrimPrefix(fieldRef, "$")

			jsonPath := b.client.buildJSONPathWithCast(fieldPath)
			pgFunc = fmt.Sprintf("to_jsonb(MAX(%s))", jsonPath)
		} else {
			pgFunc = "to_jsonb(MAX(document))"
		}
	default:
		// Unsupported window function - return NULL
		slog.Warn("Unsupported window function operator",
			"operator", fieldOutput.operator)

		return "NULL"
	}

	// Build window frame specification
	var frameSpec string
	if fieldOutput.window != nil {
		frameSpec = b.buildWindowFrame(fieldOutput.window)
	} else {
		// Default: entire partition
		frameSpec = frameBoundUnbounded
	}

	overClause := fmt.Sprintf("OVER (%s %s)", orderByClause, frameSpec)

	// Return the complete window function
	return fmt.Sprintf("%s %s", pgFunc, overClause)
}

func (b *aggregationQueryBuilder) buildWindowFrame(windowSpec map[string]interface{}) string {
	// Handle MongoDB window specification
	// Example: {"documents": ["unbounded", -1]} → ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
	// Example: {"documents": ["unbounded", "current"]} → ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
	if documents, hasDocuments := windowSpec["documents"]; hasDocuments {
		docArray, ok := documents.([]interface{})
		if !ok || len(docArray) != 2 {
			// Invalid specification, use default
			return frameBoundUnbounded
		}

		startBound := b.buildFrameBound(docArray[0], true)
		endBound := b.buildFrameBound(docArray[1], false)

		return fmt.Sprintf("ROWS BETWEEN %s AND %s", startBound, endBound)
	}

	// Default frame
	return frameBoundUnbounded
}

//nolint:cyclop // Complexity acceptable for frame bound building
func (b *aggregationQueryBuilder) buildFrameBound(bound interface{}, isStart bool) string {
	switch v := bound.(type) {
	case string:
		if v == "unbounded" {
			if isStart {
				return "UNBOUNDED PRECEDING"
			}

			return "UNBOUNDED FOLLOWING"
		}

		if v == "current" {
			return frameBoundCurrentRow
		}
	case int:
		if v == 0 {
			return frameBoundCurrentRow
		}

		if v < 0 {
			// Negative offset means PRECEDING
			return fmt.Sprintf("%d PRECEDING", -v)
		}
		// Positive offset means FOLLOWING
		return fmt.Sprintf("%d FOLLOWING", v)
	case float64:
		intVal := int(v)
		if intVal == 0 {
			return frameBoundCurrentRow
		}

		if intVal < 0 {
			return fmt.Sprintf("%d PRECEDING", -intVal)
		}

		return fmt.Sprintf("%d FOLLOWING", intVal)
	}

	// Default
	if isStart {
		return "UNBOUNDED PRECEDING"
	}

	return frameBoundCurrentRow
}

//nolint:gocyclo // Complexity acceptable for addFields SQL generation
func (b *aggregationQueryBuilder) buildAddFieldsQuery() string {
	// Build the base query with WHERE clause
	baseQuery := b.query

	if len(b.whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(b.whereClauses, " AND ")
	}

	// Start with the original document, we'll add new fields to it
	documentExpr := "document"

	// Add each new field to the document using nested jsonb_set calls
	// The order doesn't matter, but we'll sort the field names for consistency
	fieldNames := make([]string, 0, len(b.addFields))
	for fieldName := range b.addFields {
		fieldNames = append(fieldNames, fieldName)
	}

	sort.Strings(fieldNames)

	// Build nested jsonb_set expressions
	for _, fieldName := range fieldNames {
		fieldExpr := b.addFields[fieldName]

		// Build the expression SQL for the field value
		fieldValueSQL, err := b.client.buildExprValue(fieldExpr)
		if err != nil {
			// If we can't build the expression, log a warning and skip this field
			slog.Warn("Failed to build $addFields expression, skipping field",
				"field", fieldName,
				"expr", fieldExpr,
				"error", err)

			continue
		}

		// Wrap the value in to_jsonb to ensure it's a valid JSONB value
		// This handles both literal values and complex expressions
		documentExpr = fmt.Sprintf("jsonb_set(%s, '{%s}', to_jsonb(%s))",
			documentExpr, fieldName, fieldValueSQL)
	}

	// Build SELECT clause
	selectParts := []string{"id", fmt.Sprintf("%s as document", documentExpr)}

	// Build final query
	//nolint:gosec // G201: table name from client config, no user input
	query := fmt.Sprintf("SELECT %s FROM (%s) AS subquery",
		strings.Join(selectParts, ", "), baseQuery)

	// Apply remaining clauses
	if b.orderBy != "" {
		query += " " + b.orderBy
	}

	if b.limit != "" {
		query += " " + b.limit
	}

	if b.offset != "" {
		query += " " + b.offset
	}

	return query
}

// adjustParameterNumbers adjusts SQL parameter numbers ($1, $2, etc.) based on offset
func (c *PostgreSQLClient) adjustParameterNumbers(clause string, offset int) string {
	if offset == 0 {
		return clause
	}

	// Replace parameter placeholders: $1 → $N, $2 → $N+1, etc.
	// This is a simple implementation; for production, use a more robust parser
	result := clause

	for i := 20; i >= 1; i-- { // Process in reverse to avoid double replacement
		oldParam := fmt.Sprintf("$%d", i)
		newParam := fmt.Sprintf("$%d", i+offset)
		result = strings.ReplaceAll(result, oldParam, newParam)
	}

	return result
}

// buildUpdateClause converts MongoDB-style update operators to PostgreSQL SET clause
// Supports basic $set operator for now
func (c *PostgreSQLClient) buildUpdateClause(update interface{}) (string, []interface{}, error) {
	updateMap, ok := update.(map[string]interface{})
	if !ok {
		return "", nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"update must be a map[string]interface{}",
			fmt.Errorf("got type %T", update),
		)
	}

	// Handle $set operator
	setFields, hasSet := updateMap["$set"]
	if !hasSet {
		return "", nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"update must contain $set operator",
			fmt.Errorf("supported operators: $set"),
		)
	}

	setMap, ok := setFields.(map[string]interface{})
	if !ok {
		return "", nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$set value must be a map[string]interface{}",
			fmt.Errorf("got type %T", setFields),
		)
	}

	if len(setMap) == 0 {
		return "", nil, datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"$set cannot be empty",
			nil,
		)
	}

	// Build JSONB set operations
	// For each field, use jsonb_set to update the JSONB document
	// Example: document = jsonb_set(document, '{field}', '"value"')
	// For nested fields: document = jsonb_set(document, '{path,to,field}', '"value"')

	var (
		setClauses []string
		args       []interface{}
	)

	paramCount := 1

	for fieldPath, value := range setMap {
		// Convert field path to JSONB path array
		// "nodeName" → '{nodeName}'
		// "healthevent.nodename" → '{healthevent,nodename}'
		parts := strings.Split(fieldPath, ".")
		jsonbPath := "{" + strings.Join(parts, ",") + "}"

		// Marshal value to JSON for JSONB
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return "", nil, datastore.NewSerializationError(
				datastore.ProviderPostgreSQL,
				fmt.Sprintf("failed to marshal value for field %s", fieldPath),
				err,
			)
		}

		setClauses = append(setClauses, fmt.Sprintf("jsonb_set(document, '%s', $%d)", jsonbPath, paramCount))
		args = append(args, string(valueJSON))
		paramCount++
	}

	// Chain multiple jsonb_set calls
	// document = jsonb_set(jsonb_set(document, path1, val1), path2, val2)
	setExpression := jsonbDocumentColumn
	for _, clause := range setClauses {
		setExpression = strings.Replace(clause, jsonbDocumentColumn, setExpression, 1)
	}

	return fmt.Sprintf("%s = %s", jsonbDocumentColumn, setExpression), args, nil
}

// PostgreSQL-specific wrapper types

// postgresqlSingleResult wraps sql.Row to implement SingleResult interface
type postgresqlSingleResult struct {
	row   *sql.Row
	table string
	err   error
}

func (r *postgresqlSingleResult) Decode(v interface{}) error {
	if r.err != nil {
		return r.err
	}

	var (
		id           string
		documentJSON []byte
	)

	// Scan the row to get id and document JSONB

	if err := r.row.Scan(&id, &documentJSON); err != nil {
		if err == sql.ErrNoRows {
			return datastore.NewDocumentNotFoundError(
				datastore.ProviderPostgreSQL,
				"document not found",
				err,
			)
		}

		return datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to scan result",
			err,
		)
	}

	// Unmarshal JSONB into target struct
	if err := json.Unmarshal(documentJSON, v); err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to unmarshal document",
			err,
		)
	}

	// Add _id field if target is a map (for compatibility with MongoDB)
	if targetMap, ok := v.(*map[string]interface{}); ok && targetMap != nil {
		(*targetMap)["_id"] = id
	}

	return nil
}

func (r *postgresqlSingleResult) Err() error {
	return r.err
}

// postgresqlCursor wraps sql.Rows to implement Cursor interface
type postgresqlCursor struct {
	rows *sql.Rows
	err  error
}

func (c *postgresqlCursor) Next(ctx context.Context) bool {
	if c.err != nil {
		return false
	}

	return c.rows.Next()
}

func (c *postgresqlCursor) Decode(v interface{}) error {
	if c.err != nil {
		return c.err
	}

	var (
		id           string
		documentJSON []byte
	)

	// Scan the current row

	if err := c.rows.Scan(&id, &documentJSON); err != nil {
		return datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to scan cursor row",
			err,
		)
	}

	// Unmarshal JSONB into target struct
	if err := json.Unmarshal(documentJSON, v); err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to unmarshal document",
			err,
		)
	}

	// Add _id field if target is a map (for compatibility with MongoDB)
	if targetMap, ok := v.(*map[string]interface{}); ok && targetMap != nil {
		(*targetMap)["_id"] = id
	}

	return nil
}

func (c *postgresqlCursor) Close(ctx context.Context) error {
	if c.rows != nil {
		return c.rows.Close()
	}

	return nil
}

func (c *postgresqlCursor) All(ctx context.Context, results interface{}) error {
	if c.err != nil {
		return c.err
	}

	// Results must be a pointer to a slice
	// We'll decode each row and append to the slice
	// For now, support []map[string]interface{} and similar slice types

	defer c.rows.Close()

	var documents []map[string]interface{}

	for c.rows.Next() {
		var (
			id           string
			documentJSON []byte
		)

		if err := c.rows.Scan(&id, &documentJSON); err != nil {
			return datastore.NewQueryError(
				datastore.ProviderPostgreSQL,
				"failed to scan row",
				err,
			)
		}

		var doc map[string]interface{}
		if err := json.Unmarshal(documentJSON, &doc); err != nil {
			return datastore.NewSerializationError(
				datastore.ProviderPostgreSQL,
				"failed to unmarshal document",
				err,
			)
		}

		doc["_id"] = id
		documents = append(documents, doc)
	}

	if err := c.rows.Err(); err != nil {
		return datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"error iterating rows",
			err,
		)
	}

	// Marshal and unmarshal to convert to target type
	// This handles different slice types
	jsonBytes, err := json.Marshal(documents)
	if err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to marshal intermediate results",
			err,
		)
	}

	if err := json.Unmarshal(jsonBytes, results); err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to unmarshal to target type",
			err,
		)
	}

	return nil
}

func (c *postgresqlCursor) Err() error {
	if c.err != nil {
		return c.err
	}

	if c.rows != nil {
		return c.rows.Err()
	}

	return nil
}
