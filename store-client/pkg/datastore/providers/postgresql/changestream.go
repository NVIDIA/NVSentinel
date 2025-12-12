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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	healthEventsTable = "health_events"
	// PostgreSQL NOTIFY channel for instant change notifications
	notifyChannel = "nvsentinel_changes"
)

// ChangeStreamMode defines the operating mode for the changestream watcher
type ChangeStreamMode string

const (
	// ModePolling uses only polling (original implementation)
	ModePolling ChangeStreamMode = "polling"
	// ModeNotify uses only LISTEN/NOTIFY (experimental)
	ModeNotify ChangeStreamMode = "notify"
	// ModeHybrid uses LISTEN/NOTIFY with adaptive fallback polling (recommended)
	ModeHybrid ChangeStreamMode = "hybrid"
)

// notificationPayload represents the JSON payload sent via NOTIFY
type notificationPayload struct {
	ID        int64  `json:"id"`        // Changelog ID
	Table     string `json:"table"`     // Table name
	Operation string `json:"operation"` // INSERT/UPDATE/DELETE
}

// PostgreSQLChangeStreamWatcher implements ChangeStreamWatcher for PostgreSQL
// Supports three modes: polling (original), notify (LISTEN/NOTIFY), and hybrid (recommended)
//
// Resume Strategy: Uses timestamp-based resume (like MongoDB) for reliable replay of time-window events.
// The resume token stores both `lastTimestamp` and `lastEventID` for precise positioning.
//
// Filtering Strategy: Two-layer filtering for optimal performance:
// 1. Server-side: SQL WHERE clause filters events in the database query (reduces volume)
// 2. Application-side: PipelineFilter handles edge cases the SQL filter can't express
type PostgreSQLChangeStreamWatcher struct {
	db             *sql.DB
	clientName     string
	tableName      string
	events         chan datastore.EventWithToken
	stopCh         chan struct{}
	lastEventID    int64        // For tie-breaking when timestamps are equal
	lastTimestamp  time.Time    // Primary resume position (timestamp-based, like MongoDB)
	mu             sync.RWMutex // Protects lastEventID, lastTimestamp, and lastNotifyTime
	pollInterval   time.Duration
	pipelineFilter *PipelineFilter // Application-side filter (fallback for edge cases)
	pipeline       interface{}     // Raw pipeline for SQL filter building

	// Hybrid mode fields
	mode           ChangeStreamMode
	listener       *pq.Listener // LISTEN connection for notifications
	lastNotifyTime time.Time    // Last time we received a NOTIFY
	connString     string       // Connection string for LISTEN
}

// NewPostgreSQLChangeStreamWatcher creates a new PostgreSQL change stream watcher
func NewPostgreSQLChangeStreamWatcher(
	db *sql.DB, clientName string, tableName string, connString string, mode ChangeStreamMode,
) *PostgreSQLChangeStreamWatcher {
	return &PostgreSQLChangeStreamWatcher{
		db:           db,
		clientName:   clientName,
		tableName:    tableName,
		connString:   connString,
		mode:         mode,
		events:       make(chan datastore.EventWithToken, 100),
		stopCh:       make(chan struct{}),
		pollInterval: 10 * time.Millisecond, // Aggressive poll interval (used in polling and fallback modes)
	}
}

// Events returns the events channel
func (w *PostgreSQLChangeStreamWatcher) Events() <-chan datastore.EventWithToken {
	return w.events
}

// Start starts the change stream watcher
func (w *PostgreSQLChangeStreamWatcher) Start(ctx context.Context) {
	// Load last processed event ID
	if err := w.loadResumePosition(ctx); err != nil {
		slog.Error("Failed to load resume position", "client", w.clientName, "error", err)
	}

	switch w.mode {
	case ModePolling:
		slog.Info("Starting PostgreSQL changestream in POLLING mode",
			"client", w.clientName,
			"mode", "polling",
			"pollInterval", w.pollInterval,
			"description", "Queries changelog table every interval")

		go w.pollForChanges(ctx)

	case ModeNotify:
		slog.Info("Starting PostgreSQL changestream in LISTEN/NOTIFY mode (experimental)",
			"client", w.clientName,
			"mode", "notify",
			"description", "Uses PostgreSQL NOTIFY for instant notifications")

		go w.listenForNotifications(ctx)

	case ModeHybrid:
		hasConnString := w.connString != ""
		effectiveMode := "hybrid-notify"

		if !hasConnString {
			effectiveMode = "hybrid-degraded-to-polling"
			slog.Warn("PostgreSQL changestream HYBRID mode degraded to POLLING (no connString)",
				"client", w.clientName,
				"mode", effectiveMode,
				"reason", "connString is empty, cannot create LISTEN connection",
				"fallbackBehavior", "will use polling only")
		} else {
			slog.Info("Starting PostgreSQL changestream in HYBRID mode (LISTEN/NOTIFY + adaptive fallback)",
				"client", w.clientName,
				"mode", effectiveMode,
				"fallbackPollInterval", w.pollInterval,
				"description", "Uses LISTEN/NOTIFY with intelligent polling fallback")
		}

		go w.hybridChangeStream(ctx)

	default:
		slog.Error("Unknown changestream mode, defaulting to polling",
			"client", w.clientName,
			"requestedMode", w.mode,
			"actualMode", "polling-fallback")

		go w.pollForChanges(ctx)
	}
}

// MarkProcessed marks events as processed up to the given token
// Uses timestamp-based resume for reliable replay
func (w *PostgreSQLChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	var eventID int64

	var timestamp time.Time

	if len(token) == 0 {
		// When token is empty, use the current position (similar to MongoDB's behavior)
		// This handles the common case where event processors pass empty tokens
		// and expect the watcher to track its own position.
		w.mu.RLock()
		eventID = w.lastEventID
		timestamp = w.lastTimestamp
		w.mu.RUnlock()

		if eventID == 0 {
			slog.Debug("No events processed yet, skipping MarkProcessed",
				"client", w.clientName)

			return nil
		}

		slog.Debug("Using current stream position for empty token",
			"client", w.clientName,
			"eventID", eventID,
			"timestamp", timestamp)
	} else {
		// Token is the event ID as string
		eventIDStr := string(token)

		var err error

		eventID, err = strconv.ParseInt(eventIDStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid token format: %w", err)
		}

		// Query the timestamp for this event
		timestampQuery := `
			SELECT changed_at FROM datastore_changelog
			WHERE id = $1
		`

		if err := w.db.QueryRowContext(ctx, timestampQuery, eventID).Scan(&timestamp); err != nil {
			slog.Warn("Failed to get timestamp for event, using current time",
				"client", w.clientName,
				"eventID", eventID,
				"error", err)

			timestamp = time.Now()
		}
	}

	// Mark all events up to and including this eventID as processed
	// This is safe because:
	// 1. The application only calls MarkProcessed after successfully processing an event
	// 2. Events are delivered in order (by ID)
	// 3. If an event is filtered by the pipeline, we never send it, so MarkProcessed is never called for it
	query := `
		UPDATE datastore_changelog
		SET processed = TRUE
		WHERE id <= $1 AND table_name = $2 AND processed = FALSE
	`

	_, err := w.db.ExecContext(ctx, query, eventID, w.tableName)
	if err != nil {
		return fmt.Errorf("failed to mark event as processed: %w", err)
	}

	// Update resume position
	w.mu.Lock()
	w.lastEventID = eventID
	w.lastTimestamp = timestamp
	w.mu.Unlock()

	if err := w.saveResumePosition(ctx, timestamp, eventID); err != nil {
		slog.Error("Failed to save resume position", "error", err)
	}

	slog.Debug("Marked events processed",
		"client", w.clientName,
		"eventID", eventID,
		"timestamp", timestamp,
		"table", w.tableName)

	return nil
}

// Close closes the change stream watcher
func (w *PostgreSQLChangeStreamWatcher) Close(ctx context.Context) error {
	close(w.stopCh)
	close(w.events)

	// Close LISTEN connection if in notify or hybrid mode
	w.closeListener()

	return nil
}

// pollForChanges polls the changelog table for new changes
func (w *PostgreSQLChangeStreamWatcher) pollForChanges(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	slog.Debug("Started polling for changes", "client", w.clientName, "table", w.tableName)

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Context cancelled, stopping polling", "client", w.clientName)

			return
		case <-w.stopCh:
			slog.Debug("Stop channel triggered, stopping polling", "client", w.clientName)

			return
		case <-ticker.C:
			if err := w.fetchNewChanges(ctx); err != nil {
				slog.Error("Error fetching changes", "table", w.tableName, "error", err)
			}
		}
	}
}

// hybridChangeStream implements hybrid mode: LISTEN/NOTIFY with adaptive fallback polling
func (w *PostgreSQLChangeStreamWatcher) hybridChangeStream(ctx context.Context) {
	// Initialize LISTEN connection
	if err := w.initializeListener(ctx); err != nil {
		slog.Error("Failed to initialize LISTEN connection, degrading to POLLING-ONLY mode",
			"client", w.clientName,
			"requestedMode", "hybrid",
			"actualMode", "polling-fallback",
			"error", err,
			"impact", "Events will be discovered via polling instead of instant NOTIFY")

		w.pollForChanges(ctx)

		return
	}

	defer w.closeListener()

	// Initialize last notify time to now (assume NOTIFY is working initially)
	w.mu.Lock()
	w.lastNotifyTime = time.Now()
	w.mu.Unlock()

	// Start adaptive fallback polling in a separate goroutine
	adaptiveStopCh := make(chan struct{})
	defer close(adaptiveStopCh)

	go w.adaptiveFallbackPoller(ctx, adaptiveStopCh)

	// Main loop: listen for notifications
	w.listenLoop(ctx)
}

// listenForNotifications implements pure LISTEN/NOTIFY mode (experimental)
func (w *PostgreSQLChangeStreamWatcher) listenForNotifications(ctx context.Context) {
	// Initialize LISTEN connection
	if err := w.initializeListener(ctx); err != nil {
		slog.Error("Failed to initialize LISTEN connection",
			"client", w.clientName,
			"error", err)

		return
	}

	defer w.closeListener()

	// Main loop: listen for notifications
	w.listenLoop(ctx)
}

// initializeListener sets up the pq.Listener for LISTEN/NOTIFY
func (w *PostgreSQLChangeStreamWatcher) initializeListener(ctx context.Context) error {
	if w.connString == "" {
		return fmt.Errorf("connString is empty, cannot create LISTEN connection")
	}

	// Event callback for listener lifecycle events
	eventCallback := func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventConnected:
			slog.Info("LISTEN connection established",
				"client", w.clientName,
				"mode", w.mode,
				"channel", notifyChannel,
				"status", "connected")
		case pq.ListenerEventDisconnected:
			slog.Warn("LISTEN connection lost, will reconnect",
				"client", w.clientName,
				"mode", w.mode,
				"channel", notifyChannel,
				"status", "disconnected",
				"error", err)
		case pq.ListenerEventReconnected:
			slog.Info("LISTEN connection restored",
				"client", w.clientName,
				"mode", w.mode,
				"channel", notifyChannel,
				"status", "reconnected")
		case pq.ListenerEventConnectionAttemptFailed:
			slog.Error("LISTEN connection attempt failed",
				"client", w.clientName,
				"mode", w.mode,
				"channel", notifyChannel,
				"status", "connection-failed",
				"error", err)
		}
	}

	// Create listener with automatic reconnection
	// minReconnectInterval: 10s, maxReconnectInterval: 1min
	w.listener = pq.NewListener(w.connString, 10*time.Second, time.Minute, eventCallback)

	// Subscribe to the notification channel
	if err := w.listener.Listen(notifyChannel); err != nil {
		w.listener.Close()

		return fmt.Errorf("failed to LISTEN on channel %s: %w", notifyChannel, err)
	}

	slog.Info("Successfully subscribed to NOTIFY channel",
		"client", w.clientName,
		"channel", notifyChannel)

	return nil
}

// closeListener closes the LISTEN connection
func (w *PostgreSQLChangeStreamWatcher) closeListener() {
	if w.listener != nil {
		if err := w.listener.Close(); err != nil {
			slog.Warn("Error closing LISTEN connection",
				"client", w.clientName,
				"error", err)
		} else {
			slog.Info("Closed LISTEN connection",
				"client", w.clientName)
		}
	}
}

// listenLoop is the main event loop for receiving NOTIFY messages
func (w *PostgreSQLChangeStreamWatcher) listenLoop(ctx context.Context) {
	slog.Info("Started LISTEN loop",
		"client", w.clientName)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping LISTEN loop",
				"client", w.clientName)

			return

		case <-w.stopCh:
			slog.Info("Stop channel triggered, stopping LISTEN loop",
				"client", w.clientName)

			return

		case notification := <-w.listener.Notify:
			if notification != nil {
				// Update last notify time for adaptive fallback
				w.mu.Lock()
				w.lastNotifyTime = time.Now()
				w.mu.Unlock()

				// Handle the notification
				if err := w.handleNotification(ctx, notification); err != nil {
					slog.Error("Error handling notification",
						"client", w.clientName,
						"error", err)
				}
			}

		case <-time.After(90 * time.Second):
			// Keepalive ping to ensure connection is alive
			go func() {
				if err := w.listener.Ping(); err != nil {
					slog.Warn("LISTEN ping failed",
						"client", w.clientName,
						"error", err)
				}
			}()
		}
	}
}

// handleNotification processes a single NOTIFY message
func (w *PostgreSQLChangeStreamWatcher) handleNotification(ctx context.Context, notification *pq.Notification) error {
	// Parse the notification payload
	var payload notificationPayload
	if err := json.Unmarshal([]byte(notification.Extra), &payload); err != nil {
		return fmt.Errorf("failed to parse notification payload: %w", err)
	}

	slog.Debug("Received NOTIFY",
		"client", w.clientName,
		"changelogID", payload.ID,
		"table", payload.Table,
		"operation", payload.Operation)

	// Fetch the full event from the changelog table
	// We only store minimal info in the NOTIFY payload (8KB limit)
	w.mu.RLock()
	lastEventID := w.lastEventID
	w.mu.RUnlock()

	// Only fetch if this event is newer than what we've already processed
	if payload.ID <= lastEventID {
		slog.Debug("Skipping already-processed event from NOTIFY",
			"client", w.clientName,
			"notifiedID", payload.ID,
			"lastEventID", lastEventID)

		return nil
	}

	// Fetch any events we might have missed plus this one
	// This handles the case where we got a notification but missed some events
	if err := w.fetchNewChanges(ctx); err != nil {
		return fmt.Errorf("failed to fetch changes after NOTIFY: %w", err)
	}

	return nil
}

// adaptiveFallbackPoller implements adaptive polling based on NOTIFY health
func (w *PostgreSQLChangeStreamWatcher) adaptiveFallbackPoller(ctx context.Context, stopCh chan struct{}) {
	// Start with infrequent polling (30s) assuming NOTIFY is working
	checkInterval := 30 * time.Second
	ticker := time.NewTicker(checkInterval)

	defer ticker.Stop()

	slog.Info("Started adaptive fallback poller",
		"client", w.clientName,
		"initialInterval", checkInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			// Check when we last received a NOTIFY
			w.mu.RLock()
			timeSinceLastNotify := time.Since(w.lastNotifyTime)
			lastEventID := w.lastEventID
			w.mu.RUnlock()

			// Determine if NOTIFY seems to be working
			if timeSinceLastNotify > 30*time.Second {
				// NOTIFY might be silent or broken, poll more aggressively
				newInterval := 5 * time.Second

				if checkInterval != newInterval {
					slog.Warn("HYBRID MODE: NOTIFY appears silent, increasing fallback poll frequency",
						"client", w.clientName,
						"mode", "hybrid-active-fallback",
						"timeSinceLastNotify", timeSinceLastNotify,
						"oldInterval", checkInterval,
						"newInterval", newInterval,
						"reason", "No NOTIFY received for >30s")
					checkInterval = newInterval
					ticker.Reset(checkInterval)
				}

				// Perform fallback poll
				slog.Debug("Executing fallback poll (NOTIFY silent)",
					"client", w.clientName,
					"mode", "hybrid-polling",
					"lastEventID", lastEventID)

				if err := w.fetchNewChanges(ctx); err != nil {
					slog.Error("Fallback poll failed",
						"client", w.clientName,
						"error", err)
				}
			} else {
				// NOTIFY is working well, reduce fallback polling
				newInterval := 30 * time.Second
				if checkInterval != newInterval {
					slog.Info("HYBRID MODE: NOTIFY is healthy, reducing fallback poll frequency",
						"client", w.clientName,
						"mode", "hybrid-notify-active",
						"timeSinceLastNotify", timeSinceLastNotify,
						"oldInterval", checkInterval,
						"newInterval", newInterval,
						"reason", "NOTIFY working well")
					checkInterval = newInterval
					ticker.Reset(checkInterval)
				}
			}
		}
	}
}

// fetchNewChanges fetches new changes from the changelog table.
// Uses two-layer filtering:
// 1. Server-side: SQL WHERE clause reduces volume at the database level
// 2. Application-side: PipelineFilter handles edge cases (in sendEventsToChannel)
func (w *PostgreSQLChangeStreamWatcher) fetchNewChanges(ctx context.Context) error {
	w.mu.RLock()
	lastTimestamp := w.lastTimestamp
	lastEventID := w.lastEventID
	w.mu.RUnlock()

	// Build server-side SQL filter from pipeline (if configured)
	sqlFilterClause, sqlFilterArgs := w.buildSQLFilter()

	// Base query: Timestamp-based with ID tie-breaking (like MongoDB)
	// Server-side filter is appended if available
	baseQuery := `
		SELECT id, record_id, operation, old_values, new_values, changed_at
		FROM datastore_changelog
		WHERE table_name = $1
		  AND (
		      changed_at > $2
		      OR (changed_at = $2 AND id > $3)
		  )`

	// Append server-side filter if available
	query := baseQuery + sqlFilterClause + `
		ORDER BY changed_at ASC, id ASC
		LIMIT 100
	`

	// Build args: base args + filter args
	args := []interface{}{w.tableName, lastTimestamp, lastEventID}
	args = append(args, sqlFilterArgs...)

	rows, err := w.db.QueryContext(ctx, query, args...)
	if err != nil {
		slog.Error("Failed to query changelog", "client", w.clientName, "error", err)

		return fmt.Errorf("failed to query changelog: %w", err)
	}
	defer rows.Close()

	events, err := w.processChangelogRows(rows)
	if err != nil {
		slog.Error("Failed to process changelog rows", "client", w.clientName, "error", err)

		return err
	}

	slog.Debug("Fetched events from changelog", "client", w.clientName, "eventCount", len(events))

	return w.sendEventsToChannel(ctx, events)
}

// buildSQLFilter builds a SQL WHERE clause from the pipeline filter.
// Returns empty string and nil args if no filter is configured or conversion fails.
// The application-side PipelineFilter remains as a fallback.
func (w *PostgreSQLChangeStreamWatcher) buildSQLFilter() (string, []interface{}) {
	if w.pipeline == nil {
		return "", nil
	}

	// Build SQL filter from pipeline
	// Start at arg index 4 since $1=tableName, $2=timestamp, $3=eventID
	builder := NewSQLFilterBuilder(3)

	if err := builder.BuildFromPipeline(w.pipeline); err != nil {
		slog.Debug("Failed to build SQL filter from pipeline, using application-side filter only",
			"client", w.clientName,
			"error", err)

		return "", nil
	}

	if !builder.HasConditions() {
		return "", nil
	}

	clause := builder.GetWhereClauseWithAnd()
	args := builder.GetArgs()

	slog.Debug("Built server-side SQL filter", "client", w.clientName)

	return clause, args
}

// processChangelogRows processes changelog rows and builds events
func (w *PostgreSQLChangeStreamWatcher) processChangelogRows(rows *sql.Rows) ([]datastore.EventWithToken, error) {
	var events []datastore.EventWithToken

	for rows.Next() {
		var (
			id                   int64
			recordID             string
			operation            string
			oldValues, newValues sql.NullString
			changedAt            time.Time
		)

		if err := rows.Scan(&id, &recordID, &operation, &oldValues, &newValues, &changedAt); err != nil {
			slog.Error("Failed to scan changelog row", "client", w.clientName, "error", err)

			return nil, fmt.Errorf("failed to scan changelog row: %w", err)
		}

		event := w.buildEventDocument(id, recordID, operation, oldValues, newValues, changedAt)
		token := []byte(fmt.Sprintf("%d", id))

		eventWithToken := datastore.EventWithToken{
			Event:       event,
			ResumeToken: token,
		}

		events = append(events, eventWithToken)

		// NOTE: Do NOT update lastEventID here!
		// lastEventID should only be updated in sendEventsToChannel() after the event
		// is successfully sent to the channel. This ensures that:
		// 1. Filtered events don't advance the resume position
		// 2. Events are only marked as "seen" after they're actually delivered
		// 3. MarkProcessed() with empty token uses the correct position
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating changelog rows: %w", err)
	}

	slog.Debug("Finished processing changelog rows", "client", w.clientName, "totalEvents", len(events))

	return events, nil
}

// buildEventDocument builds an event document from changelog row data
func (w *PostgreSQLChangeStreamWatcher) buildEventDocument(
	id int64,
	recordID string,
	operation string,
	oldValues, newValues sql.NullString,
	changedAt time.Time,
) map[string]interface{} {
	event := map[string]interface{}{
		"_id": map[string]interface{}{
			// Use changelog ID (not recordID) for _data field to maintain consistency
			// with resume tokens and to ensure GetDocumentID() returns an int-parseable value
			// that can be used for metrics and resume position tracking.
			"_data": fmt.Sprintf("%d", id), // Use changelog sequence ID
		},
		"operationType": mapOperation(operation),
		"clusterTime":   changedAt,
		"fullDocument":  nil,
	}

	w.addDocumentDataToEvent(event, recordID, operation, oldValues, newValues)

	return event
}

// addDocumentDataToEvent adds document data to event based on operation type
//
//nolint:cyclop,gocognit,nestif // Event processing requires operation-specific handling
func (w *PostgreSQLChangeStreamWatcher) addDocumentDataToEvent(
	event map[string]interface{},
	recordID string,
	operation string,
	oldValues, newValues sql.NullString,
) {
	switch operation {
	case "INSERT":
		if newValues.Valid {
			var doc map[string]interface{}
			if err := json.Unmarshal([]byte(newValues.String), &doc); err == nil {
				// For health_events table, extract the inner document field
				if w.tableName == healthEventsTable {
					if innerDoc, ok := doc["document"].(map[string]interface{}); ok {
						// Add the record ID from the database column (not from JSONB document)
						innerDoc["id"] = recordID

						event["fullDocument"] = innerDoc
					} else {
						event["fullDocument"] = doc
					}
				} else {
					event["fullDocument"] = doc
				}
			}
		}
	case "UPDATE":
		// For UPDATE operations, add both fullDocument and updateDescription
		if newValues.Valid {
			var newDoc map[string]interface{}
			if err := json.Unmarshal([]byte(newValues.String), &newDoc); err == nil {
				// For health_events table, extract the inner document field for comparison
				var newDocForEvent, newDocForComparison map[string]interface{}

				if w.tableName == healthEventsTable {
					if innerDoc, ok := newDoc["document"].(map[string]interface{}); ok {
						// Use innerDoc AS-IS for comparison (without top-level id)
						newDocForComparison = innerDoc

						// Create a copy with id for fullDocument
						newDocForEvent = make(map[string]interface{})
						for k, v := range innerDoc {
							newDocForEvent[k] = v
						}

						// Add the record ID from the database column (not from JSONB document)
						newDocForEvent["id"] = recordID
					} else {
						newDocForEvent = newDoc
						newDocForComparison = newDoc
					}
				} else {
					newDocForEvent = newDoc
					newDocForComparison = newDoc
				}

				event["fullDocument"] = newDocForEvent

				// Add updateDescription to match MongoDB changestream format
				if oldValues.Valid {
					var oldDoc map[string]interface{}
					if err := json.Unmarshal([]byte(oldValues.String), &oldDoc); err == nil {
						// For health_events table, extract the inner document field for comparison
						var oldDocForComparison map[string]interface{}

						if w.tableName == healthEventsTable {
							if innerDoc, ok := oldDoc["document"].(map[string]interface{}); ok {
								oldDocForComparison = innerDoc
							} else {
								oldDocForComparison = oldDoc
							}
						} else {
							oldDocForComparison = oldDoc
						}

						updatedFields := w.findUpdatedFields(oldDocForComparison, newDocForComparison)

						if len(updatedFields) > 0 {
							event["updateDescription"] = map[string]interface{}{
								"updatedFields": updatedFields,
							}
						} else {
							slog.Debug("No updatedFields found", "client", w.clientName, "recordID", recordID)
						}
					}
				}
			}
		}
	case "DELETE":
		if oldValues.Valid {
			var doc map[string]interface{}
			if err := json.Unmarshal([]byte(oldValues.String), &doc); err == nil {
				// For health_events table, extract the inner document field
				if w.tableName == healthEventsTable {
					if innerDoc, ok := doc["document"].(map[string]interface{}); ok {
						event["fullDocumentBeforeChange"] = innerDoc
					} else {
						event["fullDocumentBeforeChange"] = doc
					}
				} else {
					event["fullDocumentBeforeChange"] = doc
				}
			}
		}
	}
}

// findUpdatedFields compares old and new documents to find changed fields
// Returns a flattened map with dot-notation keys to match MongoDB changestream format
func (w *PostgreSQLChangeStreamWatcher) findUpdatedFields(
	oldDoc, newDoc map[string]interface{},
) map[string]interface{} {
	updatedFields := make(map[string]interface{})

	// Compare all fields in newDoc with oldDoc
	for key, newValue := range newDoc {
		oldValue, exists := oldDoc[key]

		// Field is updated if it didn't exist before or if the value changed
		if !exists || !w.valuesEqual(oldValue, newValue) {
			// For nested objects, flatten them with dot notation
			// e.g., healtheventstatus: {nodequarantined: "Quarantined"} becomes
			// "healtheventstatus.nodequarantined": "Quarantined"
			if newValueMap, ok := newValue.(map[string]interface{}); ok {
				w.flattenMap("", key, newValueMap, oldValue, updatedFields)
			} else {
				updatedFields[key] = newValue
			}
		}
	}

	return updatedFields
}

// flattenMap recursively flattens nested maps using dot notation
func (w *PostgreSQLChangeStreamWatcher) flattenMap(
	parentPrefix, currentKey string,
	currentValue map[string]interface{},
	oldValue interface{},
	result map[string]interface{},
) {
	var oldMap map[string]interface{}
	if oldValue != nil {
		oldMap, _ = oldValue.(map[string]interface{})
	}

	prefix := currentKey
	if parentPrefix != "" {
		prefix = parentPrefix + "." + currentKey
	}

	for k, v := range currentValue {
		fullKey := prefix + "." + k

		var oldV interface{}
		if oldMap != nil {
			oldV = oldMap[k]
		}

		// Recursively flatten nested maps
		if vMap, ok := v.(map[string]interface{}); ok {
			w.flattenMap(prefix, k, vMap, oldV, result)
		} else if !w.valuesEqual(oldV, v) {
			// Only include if the value actually changed
			result[fullKey] = v
		}
	}
}

// valuesEqual compares two values for equality
//
//nolint:cyclop,gocognit,nestif // Deep equality comparison requires type checking
func (w *PostgreSQLChangeStreamWatcher) valuesEqual(v1, v2 interface{}) bool {
	// Handle nil cases
	if v1 == nil && v2 == nil {
		return true
	}

	if v1 == nil || v2 == nil {
		return false
	}

	// For maps, do deep comparison
	if m1, ok1 := v1.(map[string]interface{}); ok1 {
		if m2, ok2 := v2.(map[string]interface{}); ok2 {
			if len(m1) != len(m2) {
				return false
			}

			for k, val1 := range m1 {
				val2, exists := m2[k]
				if !exists || !w.valuesEqual(val1, val2) {
					return false
				}
			}

			return true
		}

		return false
	}

	// For slices, do deep comparison
	if s1, ok1 := v1.([]interface{}); ok1 {
		if s2, ok2 := v2.([]interface{}); ok2 {
			if len(s1) != len(s2) {
				return false
			}

			for i, val1 := range s1 {
				if !w.valuesEqual(val1, s2[i]) {
					return false
				}
			}

			return true
		}

		return false
	}

	// For primitive types, use direct comparison
	return v1 == v2
}

// extractEventTimestamp extracts the timestamp from an event's clusterTime field.
// Falls back to current time if clusterTime is not available.
func (w *PostgreSQLChangeStreamWatcher) extractEventTimestamp(
	eventData map[string]interface{},
	eventID int64,
) time.Time {
	if clusterTime, exists := eventData["clusterTime"]; exists {
		if ts, ok := clusterTime.(time.Time); ok {
			return ts
		}
	}

	// Fallback: if clusterTime is not available, use current time
	slog.Debug("No clusterTime in event, using current time", "client", w.clientName)

	return time.Now()
}

// sendEventsToChannel sends events to the channel
// Sends each event to the events channel sequentially
// If a pipeline filter is configured, events are filtered before sending
//
//nolint:cyclop // Event processing requires sequential steps
func (w *PostgreSQLChangeStreamWatcher) sendEventsToChannel(
	ctx context.Context,
	events []datastore.EventWithToken,
) error {
	sentCount := 0
	filteredCount := 0

	for _, event := range events {
		// Extract event ID and timestamp from event for tracking
		// CRITICAL FIX: Update position BEFORE filtering to prevent deadlock
		// This ensures the changestream position advances even for filtered events,
		// matching MongoDB's behavior where resume tokens advance independently of filtering.
		eventIDStr := string(event.ResumeToken)

		eventID, parseErr := strconv.ParseInt(eventIDStr, 10, 64)
		if parseErr != nil {
			slog.Error("Failed to parse event ID from resume token", "client", w.clientName, "error", parseErr)
		}

		// Update position if we successfully parsed the event ID
		if parseErr == nil {
			eventTimestamp := w.extractEventTimestamp(event.Event, eventID)

			w.mu.Lock()
			w.lastEventID = eventID
			w.lastTimestamp = eventTimestamp
			w.mu.Unlock()

			slog.Debug("Advanced position before filtering", "client", w.clientName, "eventID", eventID)
		}

		// Apply pipeline filter if configured
		if w.pipelineFilter != nil {
			if !w.pipelineFilter.MatchesEvent(event) {
				slog.Debug("Event filtered out by pipeline", "client", w.clientName, "eventID", eventID)

				filteredCount++

				continue // Skip events that don't match the pipeline
			}
		}

		select {
		case w.events <- event:
			sentCount++
		case <-ctx.Done():
			slog.Debug("Context cancelled while sending event", "client", w.clientName)

			return ctx.Err()
		case <-w.stopCh:
			slog.Debug("Stop channel triggered while sending event", "client", w.clientName)

			return nil
		}
	}

	if len(events) > 0 {
		if w.pipelineFilter != nil {
			slog.Debug("Sent filtered change events",
				"sent", sentCount,
				"filtered_out", filteredCount,
				"total", len(events),
				"table", w.tableName)
		} else {
			slog.Debug("Sent change events", "count", sentCount, "table", w.tableName)
		}
	}

	return nil
}

// parseTimestampFromToken extracts the timestamp from a resume token map.
func parseTimestampFromToken(token map[string]interface{}) (time.Time, bool) {
	timestampVal, exists := token["timestamp"]
	if !exists {
		return time.Time{}, false
	}

	timestampStr, ok := timestampVal.(string)
	if !ok {
		return time.Time{}, false
	}

	parsedTime, err := time.Parse(time.RFC3339Nano, timestampStr)
	if err != nil {
		return time.Time{}, false
	}

	return parsedTime, true
}

// parseEventIDFromToken extracts the eventID from a resume token map.
func parseEventIDFromToken(token map[string]interface{}) (int64, bool) {
	eventIDVal, exists := token["eventID"]
	if !exists {
		return 0, false
	}

	eventIDFloat, ok := eventIDVal.(float64)
	if !ok {
		return 0, false
	}

	return int64(eventIDFloat), true
}

// loadResumePosition loads the last processed position from resume tokens table
// Uses timestamp-based resume (like MongoDB) for reliable replay of time-window events
//
//nolint:cyclop // Resume position loading has necessary complexity for backward compatibility
func (w *PostgreSQLChangeStreamWatcher) loadResumePosition(ctx context.Context) error {
	query := `SELECT resume_token FROM resume_tokens WHERE client_name = $1`

	var tokenJSON []byte

	err := w.db.QueryRowContext(ctx, query, w.clientName).Scan(&tokenJSON)
	if err != nil {
		return w.handleNoResumeToken(err)
	}

	var token map[string]interface{}
	if err := json.Unmarshal(tokenJSON, &token); err != nil {
		return fmt.Errorf("failed to unmarshal resume token: %w", err)
	}

	// Parse timestamp and eventID from token
	if ts, ok := parseTimestampFromToken(token); ok {
		w.lastTimestamp = ts
	}

	if eventID, ok := parseEventIDFromToken(token); ok {
		w.lastEventID = eventID
	}

	// Backward compatibility: convert old ID-based token to timestamp
	if w.lastTimestamp.IsZero() && w.lastEventID > 0 {
		w.convertOldTokenToTimestamp(ctx)
	}

	slog.Info("Loaded resume position",
		"client", w.clientName,
		"timestamp", w.lastTimestamp,
		"eventID", w.lastEventID)

	return nil
}

// handleNoResumeToken handles the case when no resume token is found.
func (w *PostgreSQLChangeStreamWatcher) handleNoResumeToken(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		// No resume token found - start from current time (like MongoDB does)
		w.lastTimestamp = time.Now()
		w.lastEventID = 0

		slog.Info("No resume token found, starting from current time",
			"client", w.clientName,
			"timestamp", w.lastTimestamp,
			"table", w.tableName)

		return nil
	}

	return fmt.Errorf("failed to load resume position: %w", err)
}

// convertOldTokenToTimestamp queries the database to get timestamp for an old ID-based token.
func (w *PostgreSQLChangeStreamWatcher) convertOldTokenToTimestamp(ctx context.Context) {
	query := `SELECT changed_at FROM datastore_changelog WHERE id = $1`

	var changedAt time.Time

	err := w.db.QueryRowContext(ctx, query, w.lastEventID).Scan(&changedAt)
	if err == nil {
		w.lastTimestamp = changedAt

		slog.Info("Converted old ID-based token to timestamp",
			"client", w.clientName,
			"eventID", w.lastEventID,
			"timestamp", w.lastTimestamp)

		return
	}

	// Can't find the event - start from current time
	w.lastTimestamp = time.Now()
	w.lastEventID = 0

	slog.Warn("Could not find event for old token, starting from current time",
		"client", w.clientName,
		"oldEventID", w.lastEventID)
}

// saveResumePosition saves the resume position to the resume tokens table
func (w *PostgreSQLChangeStreamWatcher) saveResumePosition(
	ctx context.Context,
	timestamp time.Time,
	eventID int64,
) error {
	token := map[string]interface{}{
		"timestamp": timestamp.Format(time.RFC3339Nano),
		"eventID":   eventID,
	}

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to marshal resume token: %w", err)
	}

	query := `
		INSERT INTO resume_tokens (client_name, resume_token, last_updated)
		VALUES ($1, $2, NOW())
		ON CONFLICT (client_name)
		DO UPDATE SET resume_token = EXCLUDED.resume_token, last_updated = NOW()
	`

	_, err = w.db.ExecContext(ctx, query, w.clientName, tokenJSON)
	if err != nil {
		return fmt.Errorf("failed to save resume position: %w", err)
	}

	return nil
}

// mapOperation maps PostgreSQL operations to MongoDB-style operation types
func mapOperation(pgOp string) string {
	switch pgOp {
	case "INSERT":
		return "insert"
	case "UPDATE":
		return "update"
	case "DELETE":
		return "delete"
	default:
		return "unknown"
	}
}

// GetUnprocessedEventCount returns the count of unprocessed events in the changelog
// This implements the ChangeStreamMetrics interface for observability
func (w *PostgreSQLChangeStreamWatcher) GetUnprocessedEventCount(
	ctx context.Context,
	lastProcessedID string,
) (int64, error) {
	// Parse the last processed ID
	var eventID int64

	if lastProcessedID != "" {
		var err error

		eventID, err = strconv.ParseInt(lastProcessedID, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid lastProcessedID format: %w", err)
		}
	} else {
		// If no lastProcessedID provided, use the watcher's current position
		eventID = w.lastEventID
	}

	// Count unprocessed events in the changelog for this table
	query := `
		SELECT COUNT(*)
		FROM datastore_changelog
		WHERE table_name = $1 AND id > $2 AND processed = FALSE
	`

	var count int64

	err := w.db.QueryRowContext(ctx, query, w.tableName, eventID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count unprocessed events: %w", err)
	}

	return count, nil
}

// Verify that PostgreSQLChangeStreamWatcher implements the ChangeStreamWatcher interface
var _ datastore.ChangeStreamWatcher = (*PostgreSQLChangeStreamWatcher)(nil)

// --- Backward Compatibility Adapter for client.ChangeStreamWatcher ---

// PostgreSQLEventAdapter wraps a datastore.EventWithToken and implements client.Event
// This provides backward compatibility with services using the old EventProcessor/EventWatcher
type PostgreSQLEventAdapter struct {
	eventData   map[string]interface{}
	resumeToken []byte
}

// GetDocumentID returns the changelog sequence ID for this event.
// This ID is used for:
// - Tracking the last processed position in the changestream
// - Metrics and monitoring (GetUnprocessedEventCount)
// - Resume token comparisons
//
// For PostgreSQL, this returns the datastore_changelog.id (integer) as a string,
// NOT the document's UUID. To get the document UUID, use GetRecordUUID().
//
// This maintains consistency with the resume token and ensures the returned
// value can be parsed as an integer for metrics queries.
func (e *PostgreSQLEventAdapter) GetDocumentID() (string, error) {
	// Get the changelog sequence ID from _id._data
	// After the fix at line 256, this contains the integer changelog ID (not the document UUID)
	if idData, exists := e.eventData["_id"]; exists {
		if idMap, ok := idData.(map[string]interface{}); ok {
			if dataVal, ok := idMap["_data"]; ok {
				return fmt.Sprintf("%v", dataVal), nil
			}
		}

		// Fallback: use _id directly if _data not present
		return fmt.Sprintf("%v", idData), nil
	}

	slog.Debug("No changelog sequence ID found in _id field")

	return "", fmt.Errorf("changelog sequence ID not found in event")
}

// GetRecordUUID returns the actual document UUID from the fullDocument.
// This should be used when you need the business entity ID, not for
// changestream tracking or resume tokens.
//
// For example, use this when:
// - Correlating with other systems that reference the document UUID
// - Business logic that needs the actual document identifier
// - Deduplication based on document identity
func (e *PostgreSQLEventAdapter) GetRecordUUID() (string, error) {
	if fullDoc, ok := e.eventData["fullDocument"].(map[string]interface{}); ok {
		// Try "id" field (PostgreSQL lowercase)
		if id, exists := fullDoc["id"]; exists {
			return fmt.Sprintf("%v", id), nil
		}
		// Try "_id" field (MongoDB compatibility)
		if id, exists := fullDoc["_id"]; exists {
			return fmt.Sprintf("%v", id), nil
		}

		slog.Debug("Neither 'id' nor '_id' found in fullDocument")
	} else {
		slog.Debug("fullDocument is not a map or doesn't exist")
	}

	return "", fmt.Errorf("record UUID not found in fullDocument")
}

// GetNodeName extracts the node name from the event's fullDocument
func (e *PostgreSQLEventAdapter) GetNodeName() (string, error) {
	//nolint:nestif // Nested complexity required for dual-case field name lookups
	if fullDoc, ok := e.eventData["fullDocument"].(map[string]interface{}); ok {
		if healthEvent, ok := fullDoc["healthevent"].(map[string]interface{}); ok {
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

	return "", fmt.Errorf("node name not found in event")
}

// GetResumeToken returns the resume token for this event
func (e *PostgreSQLEventAdapter) GetResumeToken() []byte {
	return e.resumeToken
}

// UnmarshalDocument unmarshals the event data into the provided interface
func (e *PostgreSQLEventAdapter) UnmarshalDocument(v interface{}) error {
	// The fullDocument contains the actual document data
	fullDoc, ok := e.eventData["fullDocument"]
	if !ok {
		return fmt.Errorf("fullDocument not found in event")
	}

	// Convert to map for easier manipulation
	docMap, ok := fullDoc.(map[string]interface{})
	if !ok {
		slog.Error("fullDocument is not a map", "type", fmt.Sprintf("%T", fullDoc))

		return fmt.Errorf("fullDocument is not a map[string]interface{}")
	}

	// The PostgreSQL provider stores the document in a nested "document" field
	// Extract just the document field which contains the actual HealthEventWithStatus
	// but preserve the top-level id field
	actualDoc := e.extractActualDocument(docMap)

	// Transform lowercase keys to match the struct field names
	// This handles the case where PostgreSQL stores lowercase JSON field names
	// but Go struct tags may expect different casing
	transformedDoc := transformJSONKeys(actualDoc)

	// Use JSON marshaling/unmarshaling for type conversion
	jsonData, err := json.Marshal(transformedDoc)
	if err != nil {
		return fmt.Errorf("failed to marshal event document: %w", err)
	}

	if err := json.Unmarshal(jsonData, v); err != nil {
		slog.Error("Unmarshal failed", "error", err)

		return fmt.Errorf("failed to unmarshal event document: %w", err)
	}

	return nil
}

// extractActualDocument extracts the actual document from the nested structure
// and preserves the top-level id field from the database row
func (e *PostgreSQLEventAdapter) extractActualDocument(docMap map[string]interface{}) map[string]interface{} {
	if nestedDoc, ok := docMap["document"].(map[string]interface{}); ok {
		// Preserve the id field from the top-level docMap
		if id, hasID := docMap["id"]; hasID {
			nestedDoc["_id"] = id
		}

		return nestedDoc
	}
	// Fall back to using the whole docMap if there's no nested document
	return docMap
}

// transformJSONKeys transforms lowercase JSON keys to match Go struct field names
// This is needed because PostgreSQL stores lowercase JSON field names from bson tags
// but protobuf fields need specific casing for proper unmarshaling
func transformJSONKeys(doc map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for key, value := range doc {
		// Handle nested maps recursively
		if nestedMap, ok := value.(map[string]interface{}); ok {
			value = transformJSONKeys(nestedMap)
		}

		// Apply specific transformations for known fields
		// Map lowercase keys from bson tags to proper JSON keys
		transformedKey := getTransformedKey(key)
		result[transformedKey] = value
	}

	return result
}

// getTransformedKey returns the transformed key for known fields
func getTransformedKey(key string) string {
	keyMap := map[string]string{
		"healthevent":              "healthevent",
		"healtheventstatus":        "healtheventstatus",
		"createdat":                "createdAt",
		"nodequarantined":          "nodequarantined",
		"userpodsevictionstatus":   "userpodsevictionstatus",
		"faultremediated":          "faultremediated",
		"lastremediationtimestamp": "lastremediationtimestamp",
	}

	if transformedKey, ok := keyMap[key]; ok {
		return transformedKey
	}

	// Keep other keys as-is
	return key
}

// Verify PostgreSQLEventAdapter implements client.Event interface at compile time
var _ client.Event = (*PostgreSQLEventAdapter)(nil)

// PostgreSQLChangeStreamAdapter wraps PostgreSQLChangeStreamWatcher and implements client.ChangeStreamWatcher
// This provides backward compatibility with services using the old EventProcessor/EventWatcher
type PostgreSQLChangeStreamAdapter struct {
	watcher       *PostgreSQLChangeStreamWatcher
	eventChan     chan client.Event
	stopConverter chan struct{}
	initOnce      sync.Once
}

// NewPostgreSQLChangeStreamAdapter creates a new adapter for backward compatibility
func NewPostgreSQLChangeStreamAdapter(watcher *PostgreSQLChangeStreamWatcher) *PostgreSQLChangeStreamAdapter {
	return &PostgreSQLChangeStreamAdapter{
		watcher:       watcher,
		stopConverter: make(chan struct{}),
	}
}

// Events returns a channel of client.Event
func (a *PostgreSQLChangeStreamAdapter) Events() <-chan client.Event {
	a.initOnce.Do(func() {
		a.eventChan = make(chan client.Event, 100)

		go func() {
			defer close(a.eventChan)

			for {
				select {
				case eventWithToken, ok := <-a.watcher.Events():
					if !ok {
						return // Channel closed
					}

					// Convert datastore.EventWithToken to client.Event
					adapter := &PostgreSQLEventAdapter{
						eventData:   eventWithToken.Event,
						resumeToken: eventWithToken.ResumeToken,
					}

					select {
					case a.eventChan <- adapter:
					case <-a.stopConverter:
						return
					}
				case <-a.stopConverter:
					return
				}
			}
		}()
	})

	return a.eventChan
}

// Start starts the underlying watcher
func (a *PostgreSQLChangeStreamAdapter) Start(ctx context.Context) {
	a.watcher.Start(ctx)
}

// MarkProcessed marks events as processed
func (a *PostgreSQLChangeStreamAdapter) MarkProcessed(ctx context.Context, token []byte) error {
	return a.watcher.MarkProcessed(ctx, token)
}

// Close closes the adapter and underlying watcher
func (a *PostgreSQLChangeStreamAdapter) Close(ctx context.Context) error {
	close(a.stopConverter)

	return a.watcher.Close(ctx)
}

// PostgreSQLChangeStreamWatcherWithUnwrap wraps PostgreSQLChangeStreamWatcher
// and provides the Unwrap() method without creating interface conflicts.
// This wrapper implements datastore.ChangeStreamWatcher and can be unwrapped to client.ChangeStreamWatcher.
type PostgreSQLChangeStreamWatcherWithUnwrap struct {
	watcher *PostgreSQLChangeStreamWatcher
	adapter *PostgreSQLChangeStreamAdapter
}

// NewPostgreSQLChangeStreamWatcherWithUnwrap creates a wrapper that supports unwrapping
func NewPostgreSQLChangeStreamWatcherWithUnwrap(
	watcher *PostgreSQLChangeStreamWatcher,
) *PostgreSQLChangeStreamWatcherWithUnwrap {
	return &PostgreSQLChangeStreamWatcherWithUnwrap{
		watcher: watcher,
		adapter: NewPostgreSQLChangeStreamAdapter(watcher),
	}
}

// Events implements datastore.ChangeStreamWatcher by delegating to the wrapped watcher
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) Events() <-chan datastore.EventWithToken {
	return w.watcher.Events()
}

// Start implements datastore.ChangeStreamWatcher by delegating to the wrapped watcher
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) Start(ctx context.Context) {
	w.watcher.Start(ctx)
}

// MarkProcessed implements datastore.ChangeStreamWatcher by delegating to the wrapped watcher
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) MarkProcessed(ctx context.Context, token []byte) error {
	return w.watcher.MarkProcessed(ctx, token)
}

// Close implements datastore.ChangeStreamWatcher by delegating to the wrapped watcher
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) Close(ctx context.Context) error {
	return w.watcher.Close(ctx)
}

// Unwrap returns the adapter as client.ChangeStreamWatcher for backward compatibility
// This allows services to unwrap the PostgreSQL watcher to the legacy interface
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) Unwrap() client.ChangeStreamWatcher {
	return w.adapter
}
