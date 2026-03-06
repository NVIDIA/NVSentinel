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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// PostgreSQLChangeStreamWatcher implements ChangeStreamWatcher for PostgreSQL
// Uses polling-based approach with the datastore_changelog table
type PostgreSQLChangeStreamWatcher struct {
	db          *sql.DB
	table       string
	tokenConfig TokenConfig
	pipeline    []map[string]interface{}

	eventChan  chan Event
	stopChan   chan struct{}
	stopped    bool
	stopMutex  sync.Mutex
	lastSeenID int64
}

// postgresqlEvent implements Event interface for PostgreSQL changelog entries
type postgresqlEvent struct {
	changelogID int64
	tableName   string
	recordID    string
	operation   string
	oldValues   map[string]interface{}
	newValues   map[string]interface{}
	changedAt   time.Time
}

// GetDocumentID returns the changelog sequence ID (not the record UUID).
// This maintains consistency with:
// - Resume tokens (which use changelogID)
// - Metrics queries (which expect int-parseable values)
// - The PostgreSQLEventAdapter implementation
//
// For the actual document UUID, use GetRecordUUID().
func (e *postgresqlEvent) GetDocumentID() (string, error) {
	// Return the changelog ID (not the record UUID) to maintain consistency
	// with resume tokens and to ensure the ID is int-parseable for metrics.
	// The changelog ID represents the changestream sequence position.
	changelogIDStr := fmt.Sprintf("%d", e.changelogID)

	slog.Debug("Returning changelogID as document ID", "changelogID", e.changelogID)

	return changelogIDStr, nil
}

// GetRecordUUID returns the actual document UUID for business logic.
// Use this when you need the document's business identifier, not for
// changestream position tracking.
func (e *postgresqlEvent) GetRecordUUID() (string, error) {
	return e.recordID, nil
}

func (e *postgresqlEvent) GetNodeName() (string, error) {
	if nodeName, ok := extractNodeNameFromValues(e.newValues); ok {
		return nodeName, nil
	}

	if nodeName, ok := extractNodeNameFromValues(e.oldValues); ok {
		return nodeName, nil
	}

	return "", datastore.NewValidationError(
		datastore.ProviderPostgreSQL,
		"unable to extract node name from event",
		nil,
	)
}

func extractNodeNameFromValues(values map[string]interface{}) (string, bool) {
	if values == nil {
		return "", false
	}

	document, ok := values["document"].(map[string]interface{})
	if !ok {
		return "", false
	}

	if healthEvent, ok := document["healthevent"].(map[string]interface{}); ok {
		if nodeName, ok := healthEvent["nodename"].(string); ok {
			return nodeName, true
		}

		if nodeName, ok := healthEvent["nodeName"].(string); ok {
			return nodeName, true
		}
	}

	if nodeName, ok := document["nodeName"].(string); ok {
		return nodeName, true
	}

	return "", false
}

func (e *postgresqlEvent) GetResumeToken() []byte {
	// Use changelog ID as resume token
	return []byte(fmt.Sprintf("%d", e.changelogID))
}

func (e *postgresqlEvent) UnmarshalDocument(v interface{}) error {
	// Get the full document from new_values or old_values
	var document map[string]interface{}

	if e.newValues != nil {
		if doc, ok := e.newValues["document"]; ok {
			document, _ = doc.(map[string]interface{})
		}
	}

	if document == nil && e.oldValues != nil {
		if doc, ok := e.oldValues["document"]; ok {
			document, _ = doc.(map[string]interface{})
		}
	}

	if document == nil {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"unable to extract document from event",
			nil,
		)
	}

	// Marshal and unmarshal to convert to target type
	docJSON, err := json.Marshal(document)
	if err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to marshal document",
			err,
		)
	}

	if err := json.Unmarshal(docJSON, v); err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to unmarshal document",
			err,
		)
	}

	return nil
}

// Start begins polling for changes
func (w *PostgreSQLChangeStreamWatcher) Start(ctx context.Context) {
	w.stopMutex.Lock()

	if w.stopped {
		w.stopMutex.Unlock()
		return
	}

	w.eventChan = make(chan Event, 100)
	w.stopChan = make(chan struct{})
	w.stopMutex.Unlock()

	// Load last processed ID from resume tokens table
	w.loadResumeToken(ctx)

	// Start polling goroutine
	go w.pollChangelog(ctx)
}

// Events returns the event channel
func (w *PostgreSQLChangeStreamWatcher) Events() <-chan Event {
	return w.eventChan
}

// MarkProcessed marks an event as processed and saves the resume token
func (w *PostgreSQLChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	// Parse changelog ID from token
	changelogID, err := strconv.ParseInt(string(token), 10, 64)
	if err != nil {
		return datastore.NewValidationError(
			datastore.ProviderPostgreSQL,
			"invalid resume token",
			err,
		)
	}

	// Mark changelog entries as processed up to this ID
	query := "UPDATE datastore_changelog SET processed = TRUE WHERE id <= $1 AND processed = FALSE"

	_, err = w.db.ExecContext(ctx, query, changelogID)
	if err != nil {
		return datastore.NewChangeStreamError(
			datastore.ProviderPostgreSQL,
			"failed to mark changelog entries as processed",
			err,
		)
	}

	// Save resume token
	return w.saveResumeToken(ctx, changelogID)
}

// GetUnprocessedEventCount returns count of unprocessed events
func (w *PostgreSQLChangeStreamWatcher) GetUnprocessedEventCount(
	ctx context.Context,
	lastProcessedID string,
) (int64, error) {
	id, err := strconv.ParseInt(lastProcessedID, 10, 64)
	if err != nil {
		// If invalid, just count all unprocessed
		id = 0
	}

	query := `
		SELECT COUNT(*)
		FROM datastore_changelog
		WHERE table_name = $1
		  AND id > $2
		  AND processed = FALSE
	`

	var count int64

	err = w.db.QueryRowContext(ctx, query, w.table, id).Scan(&count)
	if err != nil {
		return 0, datastore.NewQueryError(
			datastore.ProviderPostgreSQL,
			"failed to count unprocessed events",
			err,
		)
	}

	return count, nil
}

// Close stops the watcher
func (w *PostgreSQLChangeStreamWatcher) Close(ctx context.Context) error {
	w.stopMutex.Lock()
	defer w.stopMutex.Unlock()

	if w.stopped {
		return nil
	}

	w.stopped = true
	close(w.stopChan)

	// Wait a bit for polling goroutine to finish
	time.Sleep(100 * time.Millisecond)

	if w.eventChan != nil {
		close(w.eventChan)
	}

	return nil
}

// pollChangelog polls the changelog table for new entries
func (w *PostgreSQLChangeStreamWatcher) pollChangelog(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Millisecond) // Poll every 10ms for very low latency
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopChan:
			return
		case <-ticker.C:
			w.fetchAndProcessChanges(ctx)
		}
	}
}

// fetchAndProcessChanges fetches new changelog entries and processes them
func (w *PostgreSQLChangeStreamWatcher) fetchAndProcessChanges(ctx context.Context) {
	query := `
		SELECT id, table_name, record_id, operation, old_values, new_values, changed_at
		FROM datastore_changelog
		WHERE table_name = $1
		  AND id > $2
		  AND processed = FALSE
		ORDER BY id ASC
		LIMIT 100
	`

	rows, err := w.db.QueryContext(ctx, query, w.table, w.lastSeenID)
	if err != nil {
		slog.Error("Failed to fetch changelog entries", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		entry, err := scanChangelogEntry(rows)
		if err != nil {
			slog.Error("Failed to process changelog entry", "error", err)
			continue
		}

		w.lastSeenID = entry.changelogID

		if !w.matchesPipeline(entry) {
			continue
		}

		select {
		case w.eventChan <- entry:
		case <-ctx.Done():
			return
		case <-w.stopChan:
			return
		}
	}
}

func scanChangelogEntry(rows *sql.Rows) (*postgresqlEvent, error) {
	var (
		entry                        postgresqlEvent
		oldValuesJSON, newValuesJSON []byte
	)

	err := rows.Scan(
		&entry.changelogID,
		&entry.tableName,
		&entry.recordID,
		&entry.operation,
		&oldValuesJSON,
		&newValuesJSON,
		&entry.changedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan changelog entry: %w", err)
	}

	slog.Debug("Scanned changelog entry", "changelogID", entry.changelogID, "operation", entry.operation)

	if oldValuesJSON != nil {
		if err := json.Unmarshal(oldValuesJSON, &entry.oldValues); err != nil {
			return nil, fmt.Errorf("failed to unmarshal old_values: %w", err)
		}
	}

	if newValuesJSON != nil {
		if err := json.Unmarshal(newValuesJSON, &entry.newValues); err != nil {
			return nil, fmt.Errorf("failed to unmarshal new_values: %w", err)
		}
	}

	return &entry, nil
}

// matchesPipeline checks if an event matches the pipeline filter
func (w *PostgreSQLChangeStreamWatcher) matchesPipeline(entry *postgresqlEvent) bool {
	if len(w.pipeline) == 0 {
		return true
	}

	for _, stage := range w.pipeline {
		if !w.matchesStage(stage, entry) {
			return false
		}
	}

	return true
}

func (w *PostgreSQLChangeStreamWatcher) matchesStage(
	stage map[string]interface{}, entry *postgresqlEvent,
) bool {
	matchFilter, ok := stage["$match"]
	if !ok {
		return true
	}

	matchMap, ok := matchFilter.(map[string]interface{})
	if !ok {
		return true
	}

	if opType, ok := matchMap["operationType"]; ok {
		expectedOps := mapOperationTypes(opType)
		if len(expectedOps) == 0 || !slices.Contains(expectedOps, entry.operation) {
			return false
		}
	}

	values := entry.newValues
	if values == nil {
		values = entry.oldValues
	}

	if values == nil || !w.matchesFilters(matchMap, values) {
		return false
	}

	return true
}

// mapOperationTypes maps MongoDB operation type filters to PostgreSQL operation strings.
// Handles both single strings ("insert") and $in arrays ({"$in": ["insert", "update"]}).
func mapOperationTypes(opType interface{}) []string {
	switch v := opType.(type) {
	case string:
		if mapped := mapSingleOpType(v); mapped != "" {
			return []string{mapped}
		}
	case map[string]interface{}:
		return mapInArrayOpTypes(v)
	}

	return nil
}

func mapInArrayOpTypes(filter map[string]interface{}) []string {
	inArray, ok := filter["$in"]
	if !ok {
		return nil
	}

	arr, ok := inArray.([]interface{})
	if !ok {
		return nil
	}

	var ops []string

	for _, item := range arr {
		if s, ok := item.(string); ok {
			if mapped := mapSingleOpType(s); mapped != "" {
				ops = append(ops, mapped)
			}
		}
	}

	return ops
}

func mapSingleOpType(op string) string {
	switch op {
	case "insert":
		return "INSERT"
	case "update":
		return "UPDATE"
	case "delete":
		return "DELETE"
	}

	return ""
}

// matchesFilters checks if event data matches filter criteria
func (w *PostgreSQLChangeStreamWatcher) matchesFilters(
	filters map[string]interface{},
	data map[string]interface{},
) bool {
	for key, expectedValue := range filters {
		// Skip special operators
		if key == "operationType" {
			continue
		}

		// Extract actual value from data using dot notation
		actualValue := w.extractValue(data, key)

		// Simple equality check
		if actualValue != expectedValue {
			return false
		}
	}

	return true
}

// extractValue extracts a value from a nested map using dot notation
// (e.g., "fullDocument.healthevent.isfatal"). Numeric segments are
// interpreted as array indices when the current value is a slice.
func (w *PostgreSQLChangeStreamWatcher) extractValue(
	data map[string]interface{}, path string,
) interface{} {
	if path == "" {
		return nil
	}

	segments := strings.Split(path, ".")

	var current interface{} = data

	for _, seg := range segments {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[seg]
		case []interface{}:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(v) {
				return nil
			}

			current = v[idx]
		default:
			return nil
		}
	}

	return current
}

// loadResumeToken loads the last processed changelog ID
func (w *PostgreSQLChangeStreamWatcher) loadResumeToken(ctx context.Context) {
	query := "SELECT resume_token FROM resume_tokens WHERE client_name = $1"

	var tokenJSON []byte

	err := w.db.QueryRowContext(ctx, query, w.tokenConfig.ClientName).Scan(&tokenJSON)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("Failed to load resume token", "error", err)
		}

		w.lastSeenID = 0

		return
	}

	var token map[string]interface{}
	if err := json.Unmarshal(tokenJSON, &token); err != nil {
		slog.Warn("Failed to unmarshal resume token", "error", err)

		w.lastSeenID = 0

		return
	}

	if id, ok := token["lastChangelogID"].(float64); ok {
		w.lastSeenID = int64(id)
	}

	slog.Info("Loaded resume token", "clientName", w.tokenConfig.ClientName, "lastSeenID", w.lastSeenID)
}

// saveResumeToken saves the current changelog ID as resume token
func (w *PostgreSQLChangeStreamWatcher) saveResumeToken(ctx context.Context, changelogID int64) error {
	token := map[string]interface{}{
		"lastChangelogID": changelogID,
	}

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderPostgreSQL,
			"failed to marshal resume token",
			err,
		)
	}

	query := `
		INSERT INTO resume_tokens (client_name, resume_token, last_updated)
		VALUES ($1, $2, NOW())
		ON CONFLICT (client_name)
		DO UPDATE SET resume_token = $2, last_updated = NOW()
	`

	_, err = w.db.ExecContext(ctx, query, w.tokenConfig.ClientName, tokenJSON)
	if err != nil {
		return datastore.NewChangeStreamError(
			datastore.ProviderPostgreSQL,
			"failed to save resume token",
			err,
		)
	}

	return nil
}
