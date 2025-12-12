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
	"strconv"
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

//nolint:cyclop,gocognit // Complexity acceptable for dual-case field name lookups (MongoDB vs PostgreSQL)
func (e *postgresqlEvent) GetNodeName() (string, error) {
	// Try to extract node name from new values (INSERT/UPDATE)
	//nolint:nestif // Nested complexity 8: required for extracting node name from JSONB structure
	if e.newValues != nil {
		if document, ok := e.newValues["document"].(map[string]interface{}); ok {
			if healthEvent, ok := document["healthevent"].(map[string]interface{}); ok {
				// Try lowercase first (MongoDB compatibility)
				if nodeName, ok := healthEvent["nodename"].(string); ok {
					return nodeName, nil
				}
				// Try camelCase (PostgreSQL JSON)
				if nodeName, ok := healthEvent["nodeName"].(string); ok {
					return nodeName, nil
				}
			}
			// Also try direct nodeName field
			if nodeName, ok := document["nodeName"].(string); ok {
				return nodeName, nil
			}
		}
	}

	// Try old values for DELETE operations
	//nolint:nestif // Nested complexity 6: required for extracting node name from JSONB structure
	if e.oldValues != nil {
		if document, ok := e.oldValues["document"].(map[string]interface{}); ok {
			if healthEvent, ok := document["healthevent"].(map[string]interface{}); ok {
				// Try lowercase first (MongoDB compatibility)
				if nodeName, ok := healthEvent["nodename"].(string); ok {
					return nodeName, nil
				}
				// Try camelCase (PostgreSQL JSON)
				if nodeName, ok := healthEvent["nodeName"].(string); ok {
					return nodeName, nil
				}
			}
		}
	}

	return "", datastore.NewValidationError(
		datastore.ProviderPostgreSQL,
		"unable to extract node name from event",
		nil,
	)
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
//
//nolint:gocyclo,cyclop,gocognit // Complexity 12: handles JSON unmarshaling, filtering, channel ops - acceptable
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
			slog.Error("Failed to scan changelog entry", "error", err)
			continue
		}

		slog.Debug("Scanned changelog entry", "changelogID", entry.changelogID, "operation", entry.operation)

		// Unmarshal JSON values
		if oldValuesJSON != nil {
			if err := json.Unmarshal(oldValuesJSON, &entry.oldValues); err != nil {
				slog.Error("Failed to unmarshal old_values", "error", err)
				continue
			}
		}

		if newValuesJSON != nil {
			if err := json.Unmarshal(newValuesJSON, &entry.newValues); err != nil {
				slog.Error("Failed to unmarshal new_values", "error", err)
				continue
			}
		}

		// Check if event matches pipeline filter
		if w.matchesPipeline(&entry) {
			w.lastSeenID = entry.changelogID

			select {
			case w.eventChan <- &entry:
				// Event sent successfully
			case <-ctx.Done():
				return
			case <-w.stopChan:
				return
			}
		} else {
			// Even if not matched, update lastSeenID to avoid reprocessing
			w.lastSeenID = entry.changelogID
		}
	}
}

// matchesPipeline checks if an event matches the pipeline filter
func (w *PostgreSQLChangeStreamWatcher) matchesPipeline(entry *postgresqlEvent) bool {
	// If no pipeline, accept all events
	if len(w.pipeline) == 0 {
		return true
	}

	// Process $match stages in pipeline
	for _, stage := range w.pipeline {
		//nolint:nestif // Nested complexity 7: required for pipeline filter matching
		if matchFilter, ok := stage["$match"]; ok {
			matchMap, ok := matchFilter.(map[string]interface{})
			if !ok {
				continue
			}

			// Check operation type filter
			if opType, ok := matchMap["operationType"]; ok {
				// Map PostgreSQL operations to MongoDB operation types
				expectedOp := w.mapOperationType(opType)
				if expectedOp != "" && entry.operation != expectedOp {
					return false
				}
			}

			// Check fullDocument filters (for new values)
			if entry.newValues != nil {
				if !w.matchesFilters(matchMap, entry.newValues) {
					return false
				}
			}
		}
	}

	return true
}

// mapOperationType maps MongoDB operation types to PostgreSQL
func (w *PostgreSQLChangeStreamWatcher) mapOperationType(opType interface{}) string {
	switch v := opType.(type) {
	case string:
		switch v {
		case "insert":
			return "INSERT"
		case "update":
			return "UPDATE"
		case "delete":
			return "DELETE"
		}
	case map[string]interface{}:
		// Handle {"$in": ["insert", "update"]} style filters
		if inArray, ok := v["$in"]; ok {
			if arr, ok := inArray.([]interface{}); ok && len(arr) > 0 {
				// For simplicity, just check the first operation
				if op, ok := arr[0].(string); ok {
					return w.mapOperationType(op)
				}
			}
		}
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

// extractValue extracts a value from nested map using dot notation
func (w *PostgreSQLChangeStreamWatcher) extractValue(data map[string]interface{}, path string) interface{} {
	// Handle simple paths like "fullDocument.healthevent.isfatal"
	// Split by dots and navigate
	parts := []string{}
	for _, part := range []string{path} {
		if len(parts) == 0 {
			// First split
			for _, p := range []string{part} {
				if p != "" {
					parts = append(parts, p)
				}
			}
		}
	}

	// For now, simple implementation - just return nil if not found
	// A full implementation would recursively navigate the map
	return nil
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
