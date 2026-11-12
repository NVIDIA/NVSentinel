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
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

const (
	healthEventsTable = "health_events"
	// PostgreSQL NOTIFY channel for instant change notifications
	notifyChannel = "nvsentinel_changes"

	recentEventIDLimit  = 10000
	recentEventIDTTL    = 5 * time.Minute
	resumeRetryMaxDelay = 30 * time.Second
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

const (
	// changelogBatchSize is the most changelog rows one poll reads.
	changelogBatchSize = 100
	// notifiedIDLimit bounds the notified changelog ids kept between two lookups of the fetch
	// loop. Ids that arrive while the list is full are dropped, see fetchNotifiedRows.
	notifiedIDLimit = 1000
	// fetchRetryDelay is how long the fetch loop waits before it makes a failed pass again. It
	// bounds how long a late row waits after a transient read error, and keeps the error logs to
	// one failed pass a second while the database is unhealthy.
	fetchRetryDelay = time.Second
	// notifySilentAfter is how long the LISTEN connection can go without a notification before
	// NOTIFY counts as silent. The fallback poller then polls on its own, and the fetch loop no
	// longer moves a filtered watcher up to the newest row, see mayMoveToNewestRow.
	notifySilentAfter = 30 * time.Second
)

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
	mu             sync.RWMutex // Protects lastEventID, lastTimestamp, highestReadEventID, and lastNotifyTime
	pollInterval   time.Duration
	pipelineFilter *PipelineFilter // Application-side filter (fallback for edge cases)
	pipeline       any             // Raw pipeline for SQL filter building
	recentEventIDs map[int64]time.Time
	recentEventSeq []int64

	// highestReadEventID is the highest changelog id this watcher has read, resumed from, or
	// acknowledged past. It is never below lastEventID. The cursor moves in (changed_at, id)
	// order, so its id can drop below ids already read, and an ack for such a row must not
	// need the row's changed_at: the row can have been purged by then.
	highestReadEventID int64

	resumeRetryInterval time.Duration // Initial legacy lookup retry delay; overridden in tests

	// Hybrid mode fields
	mode           ChangeStreamMode
	listener       *pq.Listener // LISTEN connection for notifications
	lastNotifyTime time.Time    // Last time we received a NOTIFY
	connString     string       // Connection string for LISTEN

	// lag carries the two timestamps change_stream_lag_seconds is derived from. See ADR-054.
	lag lagstate.Tracker

	// handleNotification hands notified changelog ids to the fetch loop through these, so the
	// goroutine that reads the LISTEN connection never runs a query or waits for the consumer.
	// mu guards all of them but wake.
	wake        chan struct{}      // holds at most one wake-up, which covers any number of notifications
	notifiedIDs []int64            // notified ids in the order they arrived, at most notifiedIDLimit
	unreadIDs   map[int64]struct{} // the notified ids no read has returned yet
	droppedIDs  int                // notified ids not kept since the last lookup, because notifiedIDs was full

	// fetchMu lets one goroutine read the changelog and deliver what it read at a time. The
	// fallback poller and the fetch loop would otherwise read the same rows from the same
	// position and deliver them twice.
	fetchMu sync.Mutex

	// retryDelay is fetchRetryDelay, which tests shorten.
	retryDelay time.Duration
}

// LagState reports when this poller last saw a poll return no rows and the changed_at of the
// newest row it read. Either may be zero before the first poll completes, which callers must
// treat as "unknown" rather than as caught up: see ADR-054.
func (w *PostgreSQLChangeStreamWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.lag.LagState()
}

// NewPostgreSQLChangeStreamWatcher creates a new PostgreSQL change stream watcher
func NewPostgreSQLChangeStreamWatcher(
	db *sql.DB, clientName string, tableName string, connString string, mode ChangeStreamMode,
) *PostgreSQLChangeStreamWatcher {
	return &PostgreSQLChangeStreamWatcher{
		db:             db,
		clientName:     clientName,
		tableName:      tableName,
		connString:     connString,
		mode:           mode,
		events:         make(chan datastore.EventWithToken, 100),
		stopCh:         make(chan struct{}),
		pollInterval:   10 * time.Millisecond, // Aggressive poll interval (used in polling and fallback modes)
		recentEventIDs: make(map[int64]time.Time),
		wake:           make(chan struct{}, 1),
		retryDelay:     fetchRetryDelay,
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

	// Preserve replay after load errors; only cancellation prevents startup.
	if ctx.Err() != nil {
		return
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

		timestamp, err = w.timestampForAck(ctx, eventID)
		if err != nil {
			return fmt.Errorf("resolve acknowledgement timestamp: %w", err)
		}
	}

	processedEventID := eventID

	// Mark rows up to and including this id as processed. Rows are delivered in (changed_at, id)
	// order, not by id, so with out-of-order rows this can also mark a lower-id row that hasn't
	// been delivered yet. Today that only skews GetUnprocessedEventCount, because
	// cleanup_changelog() is never called (#2170) and ADR-068 doesn't delete by this flag.
	// Retention must not delete by processed without revisiting this.
	query := `
		UPDATE datastore_changelog
		SET processed = TRUE
		WHERE id <= $1 AND table_name = $2 AND processed = FALSE
	`

	_, err := w.db.ExecContext(ctx, query, processedEventID, w.tableName)
	if err != nil {
		return fmt.Errorf("failed to mark event as processed: %w", err)
	}

	// Serialize resume position selection and persistence so concurrent
	// MarkProcessed calls cannot persist an older bookmark after a newer one.
	w.mu.Lock()

	// Move the cursor only forward in (changed_at, id) order. The id alone isn't enough, since a
	// row can have a higher id than the cursor and an earlier changed_at. advancePosition has
	// already put the cursor at or past every row this watcher delivered, so an ack only moves it
	// for a row the watcher never delivered, such as one from before a restart.
	if positionAfter(timestamp, processedEventID, w.lastTimestamp, w.lastEventID) {
		w.lastEventID = processedEventID
		w.lastTimestamp = timestamp
	}

	if processedEventID > w.highestReadEventID {
		w.highestReadEventID = processedEventID
	}

	resumeEventID := w.lastEventID
	resumeTimestamp := w.lastTimestamp
	saveErr := w.saveResumePosition(ctx, resumeTimestamp, resumeEventID)

	w.mu.Unlock()

	if saveErr != nil {
		slog.Error("Failed to save resume position", "error", saveErr)
	}

	slog.Debug("Marked events processed",
		"client", w.clientName,
		"eventID", processedEventID,
		"resumeEventID", resumeEventID,
		"timestamp", resumeTimestamp,
		"table", w.tableName)

	return nil
}

// timestampForAck only looks up a timestamp when the ack is ahead of every row read so far.
func (w *PostgreSQLChangeStreamWatcher) timestampForAck(ctx context.Context, eventID int64) (time.Time, error) {
	w.mu.RLock()
	highestReadEventID := w.highestReadEventID
	w.mu.RUnlock()

	// An ack for a row this watcher has read doesn't need the row's changed_at. The cursor is
	// already at or past the row, so the ack can't move it, and the row may have been purged.
	// The cursor's own id is not enough here: it moves in (changed_at, id) order and can sit
	// below ids already read, and those acks must keep working too.
	if eventID <= highestReadEventID {
		return time.Time{}, nil
	}

	query := `SELECT changed_at FROM datastore_changelog WHERE id = $1`

	var timestamp time.Time

	if err := w.db.QueryRowContext(ctx, query, eventID).Scan(&timestamp); err != nil {
		return time.Time{}, fmt.Errorf("failed to get timestamp for event %d: %w", eventID, err)
	}

	return timestamp, nil
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

	// Notifications only record ids. The fetch loop reads the changelog, so a slow consumer
	// holds up the fetch loop and never the reading of the LISTEN connection.
	stopFetchLoop := w.startFetchLoop(ctx)
	defer stopFetchLoop()

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

	if payload.Table != w.tableName {
		return nil
	}

	// This runs on the goroutine that reads the LISTEN connection, so it never queries the
	// database or waits for the consumer. It records the id and wakes the fetch loop.
	// addNotifiedID leaves out a row a poll has read, whatever its id: the cursor follows
	// (changed_at, id), so a row already read can have a higher id than the cursor.
	w.addNotifiedID(payload.ID)

	return nil
}

// queryNotifiedRows reads the changelog rows of this watcher's table with the given ids that are
// at or behind the read position, in the order the poll reads them. The bound is the poll's own
// comparison turned around, so a row past the position, which the poll reads in order, is neither
// read here nor counted as read for the lag metric. A row not visible yet or of another table is
// not returned either.
func (w *PostgreSQLChangeStreamWatcher) queryNotifiedRows(
	ctx context.Context, ids []int64,
) ([]datastore.EventWithToken, error) {
	w.mu.RLock()
	lastTimestamp, lastEventID := w.lastTimestamp, w.lastEventID
	w.mu.RUnlock()

	query := `
		SELECT id, record_id, operation, old_values, new_values, changed_at
		FROM datastore_changelog
		WHERE table_name = $1 AND id = ANY($2)
		  AND (changed_at, id) <= ($3, $4)
		ORDER BY changed_at ASC, id ASC
	`

	rows, err := w.db.QueryContext(ctx, query, w.tableName, pq.Array(ids), lastTimestamp, lastEventID)
	if err != nil {
		return nil, fmt.Errorf("failed to query notified changelog rows: %w", err)
	}
	defer rows.Close()

	events, err := w.processChangelogRows(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to process notified changelog rows: %w", err)
	}

	return events, nil
}

// addNotifiedID records a notified changelog id for the fetch loop and wakes it. It never blocks.
// When notifiedIDLimit ids are already waiting, the id is dropped and counted instead. A row a
// poll has read by now is left out, under the lock the read is recorded with, so no pass looks it
// up and delivers it a second time. Only a recorded id wakes the fetch loop: an id left out needs
// no pass, and one that is dropped or already waiting comes while ids wait, so a pass that takes
// them is already due.
func (w *PostgreSQLChangeStreamWatcher) addNotifiedID(id int64) {
	w.mu.Lock()

	_, read := w.recentEventIDs[id]
	_, waiting := w.unreadIDs[id]
	recorded := false

	if !read && !waiting {
		if len(w.notifiedIDs) < notifiedIDLimit {
			if w.unreadIDs == nil {
				w.unreadIDs = make(map[int64]struct{})
			}

			w.unreadIDs[id] = struct{}{}
			w.notifiedIDs = append(w.notifiedIDs, id)
			recorded = true
		} else {
			w.droppedIDs++
		}
	}

	w.mu.Unlock()

	if recorded {
		w.wakeFetchLoop()
	}
}

// takeNotifiedIDs returns the notified ids no read has returned yet, in arrival order, with the
// number dropped since the last call, and starts a new list.
func (w *PostgreSQLChangeStreamWatcher) takeNotifiedIDs() ([]int64, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	ids := make([]int64, 0, len(w.unreadIDs))

	for _, id := range w.notifiedIDs {
		if _, unread := w.unreadIDs[id]; unread {
			ids = append(ids, id)
		}
	}

	dropped := w.droppedIDs
	w.notifiedIDs = nil
	w.droppedIDs = 0
	clear(w.unreadIDs)

	return ids, dropped
}

// wakeFetchLoop asks the fetch loop for a pass. One pending wake-up covers any number of calls.
func (w *PostgreSQLChangeStreamWatcher) wakeFetchLoop() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// startFetchLoop starts the goroutine that reads the changelog for the LISTEN loop. The returned
// function stops it and waits for it to return.
func (w *PostgreSQLChangeStreamWatcher) startFetchLoop(ctx context.Context) func() {
	fetchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		w.fetchLoop(fetchCtx)
	}()

	return func() {
		cancel()
		<-done
	}
}

// fetchLoop makes a pass right away and then whenever it is woken: by handleNotification, or by
// a poll that read a full batch. A consumer that is slow to take events slows this loop down,
// while the LISTEN connection keeps being read. After a failed pass it makes another one after
// retryDelay unless it is woken sooner: a late row is only delivered by the lookup of its
// notified id, and on a quiet table no other notification may come.
func (w *PostgreSQLChangeStreamWatcher) fetchLoop(ctx context.Context) {
	// retry is stopped until a pass fails.
	retry := time.NewTimer(w.retryDelay)
	retry.Stop()

	defer retry.Stop()

	for {
		// The first pass reads what was committed before LISTEN was acknowledged, which no
		// notification announces.
		if err := w.readChangelog(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Failed to read the changelog", "client", w.clientName, "error", err)
			retry.Reset(w.retryDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-w.wake:
			retry.Stop()
		case <-retry.C:
		}
	}
}

// readChangelog polls, looks up the notified ids, and repeats until a poll leaves the read position
// where it was. Every row committed past the position is read in (changed_at, id) order, and a full
// batch is followed by the next poll right away, not by the next notification or fallback tick.
// The lookup after every poll empties the list of notified ids, so during a long catch-up the list
// holds what was notified during one poll, not during the whole catch-up, and a late row's id is
// not dropped because the list filled up meanwhile. An id notified after a lookup woke the fetch
// loop, so the next pass takes it.
func (w *PostgreSQLChangeStreamWatcher) readChangelog(ctx context.Context) error {
	for {
		w.mu.RLock()
		lastTimestamp, lastEventID := w.lastTimestamp, w.lastEventID
		w.mu.RUnlock()

		if err := w.fetchNewChangesFor(ctx, fetchLoopReader); err != nil {
			return fmt.Errorf("failed to poll the changelog: %w", err)
		}

		if err := w.fetchNotifiedRows(ctx); err != nil {
			return err
		}

		if !w.positionMovedFrom(lastTimestamp, lastEventID) || ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// positionMovedFrom reports whether the read position is no longer (lastTimestamp, lastEventID).
func (w *PostgreSQLChangeStreamWatcher) positionMovedFrom(lastTimestamp time.Time, lastEventID int64) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return !w.lastTimestamp.Equal(lastTimestamp) || w.lastEventID != lastEventID
}

// fetchNotifiedRows looks up every notified row no read has returned yet and delivers the ones at
// or behind the read position, in the order the poll reads them. A row whose transaction committed
// after the read position had passed it is only found this way, and it is delivered without moving
// the position back. A row past the position is left to the polls, which read it in order.
// Delivering it here would move the position over rows that committed meanwhile and that no read
// has returned, and one of those whose id is dropped would never be delivered.
func (w *PostgreSQLChangeStreamWatcher) fetchNotifiedRows(ctx context.Context) error {
	// Taking the ids and fetching them is one read: a poll in between could deliver a taken row
	// that the lookup then delivers again.
	w.fetchMu.Lock()
	defer w.fetchMu.Unlock()

	// Close does not wait for the fetch loop, and the loop can still start a pass when a wake
	// or the retry is ready together with the stop. Nothing may be looked up or delivered then.
	select {
	case <-w.stopCh:
		return nil
	default:
	}

	ids, dropped := w.takeNotifiedIDs()

	if dropped > 0 {
		slog.Warn("Fetch loop fell behind notifications, dropped notified changelog ids",
			"client", w.clientName, "dropped", dropped, "limit", notifiedIDLimit)
	}

	if len(ids) == 0 {
		return nil
	}

	// One query looks every id up, so no lookup starts after the stop check above.
	events, err := w.queryNotifiedRows(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// The pass is retried, so the ids stay pending. A late row is only ever delivered by its
		// lookup, so dropping them here would lose it.
		w.keepNotifiedIDs(ids, dropped)

		return fmt.Errorf("failed to fetch %d notified changelog rows: %w", len(ids), err)
	}

	if err := w.sendEventsToChannel(ctx, events); err != nil {
		return fmt.Errorf("failed to deliver notified changelog rows: %w", err)
	}

	return nil
}

// keepNotifiedIDs puts the ids of a failed lookup back in front of the ones notified since, and
// adds the lookup's dropped count back, so the retry sees the same state. An id notified again in
// the meantime is listed once, and the list is cut at notifiedIDLimit as before.
func (w *PostgreSQLChangeStreamWatcher) keepNotifiedIDs(ids []int64, dropped int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.unreadIDs == nil {
		w.unreadIDs = make(map[int64]struct{})
	}

	kept := make([]int64, 0, len(ids)+len(w.notifiedIDs))

	for _, id := range ids {
		if _, waiting := w.unreadIDs[id]; waiting {
			continue
		}

		kept = append(kept, id)
		w.unreadIDs[id] = struct{}{}
	}

	kept = append(kept, w.notifiedIDs...)

	if len(kept) > notifiedIDLimit {
		for _, id := range kept[notifiedIDLimit:] {
			delete(w.unreadIDs, id)
		}

		dropped += len(kept) - notifiedIDLimit
		kept = kept[:notifiedIDLimit]
	}

	w.notifiedIDs = kept
	w.droppedIDs += dropped
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
			if timeSinceLastNotify > notifySilentAfter {
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

				if err := w.fetchNewChangesFor(ctx, fallbackReader); err != nil {
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

// changelogReader names the loop a poll is made for. Only the fetch loop's polls may move a
// filtered watcher up to the newest row, see mayMoveToNewestRow.
type changelogReader int

const (
	// pollingReader is pollForChanges, in polling mode or in hybrid mode without a LISTEN
	// connection.
	pollingReader changelogReader = iota
	// fallbackReader is the adaptive fallback poller, which polls once NOTIFY has gone silent.
	fallbackReader
	// fetchLoopReader is the fetch loop next to the LISTEN loop, which looks the notified ids up
	// after every poll.
	fetchLoopReader
)

// fetchNewChanges is the poll pollForChanges makes. It never moves the read position past a row
// it has not read.
func (w *PostgreSQLChangeStreamWatcher) fetchNewChanges(ctx context.Context) error {
	return w.fetchNewChangesFor(ctx, pollingReader)
}

// fetchNewChangesFor fetches new changes from the changelog table for the loop reader names.
// Uses two-layer filtering:
// 1. Server-side: SQL WHERE clause reduces volume at the database level
// 2. Application-side: PipelineFilter handles edge cases (in sendEventsToChannel)
func (w *PostgreSQLChangeStreamWatcher) fetchNewChangesFor(ctx context.Context, reader changelogReader) error {
	w.fetchMu.Lock()
	defer w.fetchMu.Unlock()

	// A reader that waited for another one must not start a read after Close.
	select {
	case <-w.stopCh:
		return nil
	default:
	}

	w.mu.RLock()
	lastTimestamp := w.lastTimestamp
	lastEventID := w.lastEventID
	w.mu.RUnlock()

	// Build server-side SQL filter from pipeline (if configured)
	sqlFilterClause, sqlFilterArgs := w.buildSQLFilter()

	// The filter hides the rows it rejects, so the position only moves on rows the poll returns.
	// It would stay at the last row the filter accepted, and every poll would scan every row
	// since. Where moving up to the newest row is safe, that row is read before the poll.
	headTS, headID, headKnown, err := w.readChangelogHead(ctx, w.mayMoveToNewestRow(reader, sqlFilterClause != ""))
	if err != nil {
		return err
	}

	// Base query: Timestamp-based with ID tie-breaking (like MongoDB)
	// Server-side filter is appended if available
	//
	// The row comparison means changed_at > $2 OR (changed_at = $2 AND id > $3),
	// but PostgreSQL can use it as a bound on idx_changelog_resume_all: the scan
	// starts at the resume position and stops at the LIMIT. With the OR form the
	// scan starts at the oldest row of the table, so a consumer that is behind
	// reads every row before its position on each poll.
	baseQuery := `
		SELECT id, record_id, operation, old_values, new_values, changed_at
		FROM datastore_changelog
		WHERE table_name = $1
		  AND (changed_at, id) > ($2, $3)`

	// Append server-side filter if available
	//nolint:gosec // G202 - sqlFilterClause uses parameterized $N placeholders, not raw user input
	query := baseQuery + sqlFilterClause + `
		ORDER BY changed_at ASC, id ASC
		LIMIT ` + strconv.Itoa(changelogBatchSize) + `
	`

	// Build args: base args + filter args
	args := []any{w.tableName, lastTimestamp, lastEventID}
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

	// A full batch can leave more rows to read. Once it is delivered, wake the fetch loop, which
	// reads until it is caught up, so the fallback poller does not leave a backlog at one batch per
	// tick. A batch that left the read position where it was would only come back again.
	if len(events) == changelogBatchSize {
		defer func() {
			if w.positionMovedFrom(lastTimestamp, lastEventID) {
				w.wakeFetchLoop()
			}
		}()
	}

	// A poll that returned nothing is the empty batch: the changelog holds no row past this
	// consumer's position that its server-side filter would accept. Rows are recorded in
	// processChangelogRows as they are scanned.
	if len(events) == 0 {
		w.lag.RecordCaughtUp(time.Now())
	}

	if err := w.sendEventsToChannel(ctx, events); err != nil {
		return err
	}

	// The move is made inside this read, before the lookup after the poll takes any id, so that
	// lookup asks for every row the move went past.
	w.moveToNewestRowAfterShortBatch(headTS, headID, headKnown, len(events))

	return nil
}

// mayMoveToNewestRow reports whether a short batch of this poll may move a watcher with a
// server-side filter up to the newest row of its table. A row the filter accepts can still be in
// an open transaction while newer rows commit, and once the position has moved past it, only the
// lookup of its notified id delivers it. So only the fetch loop moves, since it looks the notified
// ids up after every poll, and only while the LISTEN connection delivers notifications.
// pollForChanges has no LISTEN connection, and the fallback poller polls because NOTIFY has gone
// silent. Their position stays at the last row they read, and their next poll reads such a row.
func (w *PostgreSQLChangeStreamWatcher) mayMoveToNewestRow(reader changelogReader, filtered bool) bool {
	if reader != fetchLoopReader || !filtered {
		return false
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	return !w.lastNotifyTime.IsZero() && time.Since(w.lastNotifyTime) <= notifySilentAfter
}

// readChangelogHead returns the position of the newest changelog row of this watcher's table, and
// false when it is not wanted or the table has none. It is its own statement, read before the poll.
// Every row it can see is visible to the poll too, so a short batch holds every one of them that
// the filter accepts. Reading it in the poll's snapshot would not narrow anything: a row that
// commits after the poll's snapshot depends on its NOTIFY either way, and reading the newest row
// first keeps the set of such rows the smallest.
func (w *PostgreSQLChangeStreamWatcher) readChangelogHead(
	ctx context.Context, wanted bool,
) (time.Time, int64, bool, error) {
	if !wanted {
		return time.Time{}, 0, false, nil
	}

	var (
		changedAt time.Time
		id        int64
	)

	err := w.db.QueryRowContext(ctx, `
		SELECT changed_at, id
		FROM datastore_changelog
		WHERE table_name = $1
		ORDER BY changed_at DESC, id DESC
		LIMIT 1
	`, w.tableName).Scan(&changedAt, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}

	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("failed to read the newest changelog row: %w", err)
	}

	return changedAt, id, true, nil
}

// moveToNewestRowAfterShortBatch moves the read position forward to (ts, id), the newest row read
// before the poll, when the poll returned fewer than changelogBatchSize rows. Such a batch holds
// every row the poll could see past the position that the filter accepts, so each row up to
// (ts, id) is read by now, rejected by the filter, or was not committed when the poll started. A
// row of the last kind comes from the lookup of its notified id, so the position does not move
// while notified ids are being dropped: such a row's id can be one of them, and the next poll
// reads the row only if the position is still behind it. The position never moves back, and
// does not move once the watcher is closed.
func (w *PostgreSQLChangeStreamWatcher) moveToNewestRowAfterShortBatch(
	ts time.Time, id int64, known bool, batch int,
) {
	if !known || batch >= changelogBatchSize {
		return
	}

	select {
	case <-w.stopCh:
		return
	default:
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.droppedIDs > 0 || len(w.notifiedIDs) >= notifiedIDLimit {
		return
	}

	if positionAfter(ts, id, w.lastTimestamp, w.lastEventID) {
		w.lastTimestamp, w.lastEventID = ts, id

		// highestReadEventID is never below lastEventID.
		if id > w.highestReadEventID {
			w.highestReadEventID = id
		}
	}
}

// buildSQLFilter builds a SQL WHERE clause from the pipeline filter.
// Returns empty string and nil args if no filter is configured or conversion fails.
// The application-side PipelineFilter remains as a fallback.
func (w *PostgreSQLChangeStreamWatcher) buildSQLFilter() (string, []any) {
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

	if !builder.IsComplete() {
		slog.Debug("SQL filter conversion incomplete, using application-side filter only",
			"client", w.clientName)

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

		// Rows come back ordered by changed_at, and the tracker keeps the maximum, so this ends
		// up holding the newest row read regardless of ordering.
		w.lag.RecordEventRead(changedAt)

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
) map[string]any {
	event := map[string]any{
		"_id": map[string]any{
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
func (w *PostgreSQLChangeStreamWatcher) addDocumentDataToEvent(
	event map[string]any,
	recordID string,
	operation string,
	oldValues, newValues sql.NullString,
) {
	switch operation {
	case "INSERT":
		if doc, innerExtracted, ok := w.parseDocumentValues(newValues); ok {
			if innerExtracted {
				doc["id"] = recordID
			}

			event["fullDocument"] = doc
		}
	case "UPDATE":
		w.handleUpdateEvent(event, recordID, oldValues, newValues)
	case "DELETE":
		if doc, innerExtracted, ok := w.parseDocumentValues(oldValues); ok {
			if innerExtracted {
				doc["id"] = recordID
			}

			event["fullDocumentBeforeChange"] = doc
		}
	}
}

func (w *PostgreSQLChangeStreamWatcher) handleUpdateEvent(
	event map[string]any,
	recordID string,
	oldValues, newValues sql.NullString,
) {
	newDoc, innerExtracted, ok := w.parseDocumentValues(newValues)
	if !ok {
		return
	}

	// Shallow copy: newDocForEvent gets the record ID while newDoc stays unmodified
	// for comparison in addUpdateDescription → findUpdatedFields (read-only contract).
	newDocForEvent := make(map[string]any)
	maps.Copy(newDocForEvent, newDoc)

	if innerExtracted {
		newDocForEvent["id"] = recordID
	}

	event["fullDocument"] = newDocForEvent

	w.addUpdateDescription(event, oldValues, newDoc, recordID)
}

func (w *PostgreSQLChangeStreamWatcher) addUpdateDescription(
	event map[string]any,
	oldValues sql.NullString,
	newDocForComparison map[string]any,
	recordID string,
) {
	oldDoc, _, ok := w.parseDocumentValues(oldValues)
	if !ok {
		return
	}

	updatedFields := w.findUpdatedFields(oldDoc, newDocForComparison)
	if len(updatedFields) > 0 {
		event["updateDescription"] = map[string]any{
			"updatedFields": updatedFields,
		}
	} else {
		slog.Debug("No updatedFields found", "client", w.clientName, "recordID", recordID)
	}
}

// parseDocumentValues unmarshals a SQL NullString into a document map,
// extracting the inner "document" field for health_events table rows.
// innerExtracted is true only when a health_events inner "document" was
// successfully unwrapped; callers use this to decide whether to inject
// the record ID.
func (w *PostgreSQLChangeStreamWatcher) parseDocumentValues(
	values sql.NullString,
) (doc map[string]any, innerExtracted bool, ok bool) {
	if !values.Valid {
		return nil, false, false
	}

	if err := json.Unmarshal([]byte(values.String), &doc); err != nil {
		slog.Error("Failed to unmarshal document values",
			"client", w.clientName, "table", w.tableName, "error", err)

		return nil, false, false
	}

	outerDoc := doc

	doc, innerExtracted = w.extractInnerDocument(doc)
	if innerExtracted {
		if databaseCreatedAt, exists := outerDoc["created_at"]; exists {
			doc = maps.Clone(doc)
			doc["createdAt"] = databaseCreatedAt
		}
	}

	return doc, innerExtracted, true
}

func (w *PostgreSQLChangeStreamWatcher) extractInnerDocument(
	doc map[string]any,
) (map[string]any, bool) {
	if w.tableName != healthEventsTable {
		return doc, false
	}

	if innerDoc, ok := doc["document"].(map[string]any); ok {
		return innerDoc, true
	}

	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}

	slog.Warn("health_events row missing expected 'document' field",
		"client", w.clientName, "keys", keys)

	return doc, false
}

// findUpdatedFields compares old and new documents to find changed fields
// Returns a flattened map with dot-notation keys to match MongoDB changestream format
func (w *PostgreSQLChangeStreamWatcher) findUpdatedFields(
	oldDoc, newDoc map[string]any,
) map[string]any {
	updatedFields := make(map[string]any)

	// Compare all fields in newDoc with oldDoc
	for key, newValue := range newDoc {
		oldValue, exists := oldDoc[key]

		// Field is updated if it didn't exist before or if the value changed
		if !exists || !w.valuesEqual(oldValue, newValue) {
			// For nested objects, flatten them with dot notation
			// e.g., healtheventstatus: {nodequarantined: "Quarantined"} becomes
			// "healtheventstatus.nodequarantined": "Quarantined"
			if newValueMap, ok := newValue.(map[string]any); ok {
				w.flattenMap("", key, newValueMap, oldValue, updatedFields)
			} else {
				updatedFields[key] = newValue
			}
		}
	}

	for key := range oldDoc {
		if _, exists := newDoc[key]; !exists {
			updatedFields[key] = nil
		}
	}

	return updatedFields
}

// flattenMap recursively flattens nested maps using dot notation
func (w *PostgreSQLChangeStreamWatcher) flattenMap(
	parentPrefix, currentKey string,
	currentValue map[string]any,
	oldValue any,
	result map[string]any,
) {
	var oldMap map[string]any
	if oldValue != nil {
		oldMap, _ = oldValue.(map[string]any)
	}

	prefix := currentKey
	if parentPrefix != "" {
		prefix = parentPrefix + "." + currentKey
	}

	if len(currentValue) == 0 && oldMap == nil {
		result[prefix] = currentValue

		return
	}

	for k, v := range currentValue {
		fullKey := prefix + "." + k

		var oldV any

		existed := false
		if oldMap != nil {
			oldV, existed = oldMap[k]
		}

		// Recursively flatten nested maps
		if vMap, ok := v.(map[string]any); ok {
			w.flattenMap(prefix, k, vMap, oldV, result)
		} else if !existed || !w.valuesEqual(oldV, v) {
			// Only include if the value actually changed
			result[fullKey] = v
		}
	}

	recordRemovedMapFields(prefix, oldMap, currentValue, result)
}

func recordRemovedMapFields(
	prefix string,
	oldValue, currentValue map[string]any,
	result map[string]any,
) {
	for key := range oldValue {
		if _, exists := currentValue[key]; !exists {
			result[prefix+"."+key] = nil
		}
	}
}

// valuesEqual compares two values for equality, recursing into maps and slices.
func (w *PostgreSQLChangeStreamWatcher) valuesEqual(v1, v2 any) bool {
	if v1 == nil || v2 == nil {
		return v1 == v2
	}

	switch val1 := v1.(type) {
	case map[string]any:
		val2, ok := v2.(map[string]any)
		if !ok {
			return false
		}

		return w.mapsEqual(val1, val2)
	case []any:
		val2, ok := v2.([]any)
		if !ok {
			return false
		}

		return w.slicesEqual(val1, val2)
	default:
		return v1 == v2
	}
}

func (w *PostgreSQLChangeStreamWatcher) mapsEqual(m1, m2 map[string]any) bool {
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

func (w *PostgreSQLChangeStreamWatcher) slicesEqual(s1, s2 []any) bool {
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

// extractEventTimestamp extracts the timestamp from an event's clusterTime field.
// Falls back to current time if clusterTime is not available.
func (w *PostgreSQLChangeStreamWatcher) extractEventTimestamp(
	eventData map[string]any,
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
func (w *PostgreSQLChangeStreamWatcher) sendEventsToChannel(
	ctx context.Context,
	events []datastore.EventWithToken,
) error {
	sentCount := 0
	filteredCount := 0

	for _, event := range events {
		// Extract event ID and timestamp from event for tracking
		// CRITICAL FIX: Update position BEFORE filtering to prevent deadlock.
		// This matches MongoDB's behavior where resume tokens advance independently of filtering.
		eventID := w.advancePosition(event)

		if w.pipelineFilter != nil && !w.pipelineFilter.MatchesEvent(event) {
			slog.Debug("Event filtered out by pipeline", "client", w.clientName, "eventID", eventID)

			filteredCount++

			continue
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

// advancePosition parses the event's resume token and updates the watcher's
// position so the changestream advances even for filtered events.
func (w *PostgreSQLChangeStreamWatcher) advancePosition(event datastore.EventWithToken) int64 {
	eventIDStr := string(event.ResumeToken)

	eventID, err := strconv.ParseInt(eventIDStr, 10, 64)
	if err != nil {
		w.mu.RLock()
		current := w.lastEventID
		w.mu.RUnlock()

		slog.Error("Failed to parse event ID from resume token",
			"client", w.clientName, "error", err, "currentEventID", current)

		return current
	}

	eventTimestamp := w.extractEventTimestamp(event.Event, eventID)

	w.mu.Lock()
	w.recordRecentEventIDLocked(eventID, time.Now())

	if eventID > w.highestReadEventID {
		w.highestReadEventID = eventID
	}

	// The poll reads rows in (changed_at, id) order, so the cursor has to move in that order.
	// A row can have a higher id and an earlier changed_at than rows read after it: changed_at
	// is when its transaction started and the id is taken when its statement ends. Moving by id
	// alone pinned the cursor on such a row, and every later poll read the same rows again.
	// A row behind the cursor, such as a late commit fetched from its NOTIFY, is still
	// delivered but does not move the cursor back.
	if positionAfter(eventTimestamp, eventID, w.lastTimestamp, w.lastEventID) {
		w.lastEventID = eventID
		w.lastTimestamp = eventTimestamp
	}

	w.mu.Unlock()

	slog.Debug("Advanced position before filtering", "client", w.clientName, "eventID", eventID)

	return eventID
}

// positionAfter reports whether (ts, id) comes after (otherTS, otherID) in the order the poll
// reads the changelog: changed_at first, then id.
func positionAfter(ts time.Time, id int64, otherTS time.Time, otherID int64) bool {
	if c := ts.Compare(otherTS); c != 0 {
		return c > 0
	}

	return id > otherID
}

func (w *PostgreSQLChangeStreamWatcher) recordRecentEventIDLocked(eventID int64, now time.Time) {
	// A row that was read is no longer one for the fetch loop to look up by id.
	delete(w.unreadIDs, eventID)

	if w.recentEventIDs == nil {
		w.recentEventIDs = make(map[int64]time.Time)
	}

	if _, exists := w.recentEventIDs[eventID]; !exists {
		w.recentEventSeq = append(w.recentEventSeq, eventID)
	}

	w.recentEventIDs[eventID] = now
	w.pruneRecentEventIDsLocked(now)
}

func (w *PostgreSQLChangeStreamWatcher) pruneRecentEventIDsLocked(now time.Time) {
	cutoff := now.Add(-recentEventIDTTL)
	overflow := len(w.recentEventSeq) - recentEventIDLimit
	writeIdx := 0

	for readIdx, eventID := range w.recentEventSeq {
		seenAt, exists := w.recentEventIDs[eventID]
		if !exists {
			continue
		}

		if readIdx < overflow || seenAt.Before(cutoff) {
			delete(w.recentEventIDs, eventID)
			continue
		}

		w.recentEventSeq[writeIdx] = eventID
		writeIdx++
	}

	w.recentEventSeq = w.recentEventSeq[:writeIdx]
}

// parseTimestampFromToken extracts the timestamp from a resume token map.
func parseTimestampFromToken(token map[string]any) (time.Time, bool) {
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
func parseEventIDFromToken(token map[string]any) (int64, bool) {
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
func (w *PostgreSQLChangeStreamWatcher) loadResumePosition(ctx context.Context) error {
	query := `SELECT resume_token FROM resume_tokens WHERE client_name = $1`

	var tokenJSON []byte

	err := w.db.QueryRowContext(ctx, query, w.clientName).Scan(&tokenJSON)
	if err != nil {
		return w.handleNoResumeToken(err)
	}

	var token map[string]any
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
		if err := w.convertOldTokenToTimestamp(ctx); err != nil {
			// Conversion retries while the context is live. If it still fails,
			// clear the ID so readers can replay and advance from (zero, 0).
			w.lastEventID = 0

			return fmt.Errorf("failed to convert old resume token: %w", err)
		}
	}

	w.highestReadEventID = w.lastEventID

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
func (w *PostgreSQLChangeStreamWatcher) convertOldTokenToTimestamp(ctx context.Context) error {
	oldEventID := w.lastEventID

	query := `SELECT changed_at FROM datastore_changelog WHERE id = $1`

	var changedAt time.Time

	err := w.queryLegacyTimestamp(ctx, query, oldEventID, &changedAt)
	if err == nil {
		w.lastTimestamp = changedAt

		slog.Info("Converted old ID-based token to timestamp",
			"client", w.clientName,
			"eventID", oldEventID,
			"timestamp", w.lastTimestamp)

		return nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed to get timestamp for old event %d: %w", oldEventID, err)
	}

	// The old row may have expired. Start at the earliest remaining timestamp after
	// its ID, with ID zero to include every row at that timestamp. Replaying is safer
	// than using the current time, which would skip events accumulated before restart.
	fallbackQuery := `SELECT MIN(changed_at) FROM datastore_changelog WHERE id > $1`

	var nextTimestamp sql.NullTime

	if err := w.queryLegacyTimestamp(ctx, fallbackQuery, oldEventID, &nextTimestamp); err != nil {
		return fmt.Errorf("failed to find timestamp after old event %d: %w", oldEventID, err)
	}

	if nextTimestamp.Valid {
		w.lastTimestamp = nextTimestamp.Time
	} else {
		// No later rows remain; preserve the existing current-time startup behavior.
		w.lastTimestamp = time.Now()
	}

	w.lastEventID = 0

	slog.Warn("Could not find event for old token, using fallback timestamp",
		"client", w.clientName,
		"oldEventID", oldEventID,
		"timestamp", w.lastTimestamp)

	return nil
}

// queryLegacyTimestamp retries either legacy timestamp query until success,
// a missing row, or cancellation. The caller handles missing rows and NULL timestamps.
func (w *PostgreSQLChangeStreamWatcher) queryLegacyTimestamp(
	ctx context.Context, query string, eventID int64, timestamp any,
) error {
	interval := w.resumeRetryInterval
	if interval <= 0 {
		interval = client.DefaultRetryDelay
	}

	backoff := wait.Backoff{Duration: interval, Factor: 2, Jitter: 0.2, Steps: 32, Cap: resumeRetryMaxDelay}
	delay := backoff.DelayFunc()

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("legacy timestamp lookup cancelled: %w", err)
		}

		err := w.db.QueryRowContext(ctx, query, eventID).Scan(timestamp)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			return err
		}

		retryDelay := min(delay(), resumeRetryMaxDelay)
		slog.Warn("Failed to get legacy timestamp, retrying",
			"client", w.clientName, "eventID", eventID, "error", err, "retryDelay", retryDelay)

		select {
		case <-ctx.Done():
			return fmt.Errorf("legacy timestamp lookup cancelled: %w", ctx.Err())
		case <-time.After(retryDelay):
		}
	}
}

// saveResumePosition saves the resume position to the resume tokens table
func (w *PostgreSQLChangeStreamWatcher) saveResumePosition(
	ctx context.Context,
	timestamp time.Time,
	eventID int64,
) error {
	token := map[string]any{
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
	eventData   map[string]any
	resumeToken []byte
}

// UpdatedFields exposes the same flattened update description used by the
// provider's pipeline filter.
func (e *PostgreSQLEventAdapter) UpdatedFields() map[string]any {
	updateDescription, ok := e.eventData["updateDescription"].(map[string]any)
	if !ok {
		return nil
	}

	updatedFields, _ := updateDescription["updatedFields"].(map[string]any)

	return updatedFields
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
		if idMap, ok := idData.(map[string]any); ok {
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
	if fullDoc, ok := e.eventData["fullDocument"].(map[string]any); ok {
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
	errNotFound := errors.New("node name not found in event")

	fullDoc, ok := e.eventData["fullDocument"].(map[string]any)
	if !ok {
		return "", errNotFound
	}

	healthEvent, ok := fullDoc["healthevent"].(map[string]any)
	if !ok {
		return "", errNotFound
	}

	// Try lowercase first (MongoDB compatibility), then camelCase (PostgreSQL JSON)
	if nodeName, ok := healthEvent["nodename"].(string); ok {
		return nodeName, nil
	}

	if nodeName, ok := healthEvent["nodeName"].(string); ok {
		return nodeName, nil
	}

	return "", errNotFound
}

// GetResumeToken returns the resume token for this event
func (e *PostgreSQLEventAdapter) GetResumeToken() []byte {
	return e.resumeToken
}

// UnmarshalDocument unmarshals the event data into the provided interface
func (e *PostgreSQLEventAdapter) UnmarshalDocument(v any) error {
	// The fullDocument contains the actual document data
	fullDoc, ok := e.eventData["fullDocument"]
	if !ok {
		return fmt.Errorf("fullDocument not found in event")
	}

	// Convert to map for easier manipulation
	docMap, ok := fullDoc.(map[string]any)
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
func (e *PostgreSQLEventAdapter) extractActualDocument(docMap map[string]any) map[string]any {
	if nestedDoc, ok := docMap["document"].(map[string]any); ok {
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
func transformJSONKeys(doc map[string]any) map[string]any {
	result := make(map[string]any)

	for key, value := range doc {
		// Handle nested maps recursively
		if nestedMap, ok := value.(map[string]any); ok {
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
//
//nolint:goconst // identity/canonicalisation table; literals are the data
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

// LagState delegates to the wrapped watcher so lag survives the adapter.
func (a *PostgreSQLChangeStreamAdapter) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return a.watcher.LagState()
}

// PostgreSQLChangeStreamWatcherWithUnwrap wraps PostgreSQLChangeStreamWatcher
// and provides the Unwrap() method without creating interface conflicts.
// This wrapper implements datastore.ChangeStreamWatcher and can be unwrapped to client.ChangeStreamWatcher.
type PostgreSQLChangeStreamWatcherWithUnwrap struct {
	watcher  *PostgreSQLChangeStreamWatcher
	adapter  *PostgreSQLChangeStreamAdapter
	decision client.ResumeControlDecision
}

// NewPostgreSQLChangeStreamWatcherWithUnwrap creates a wrapper that supports unwrapping
func NewPostgreSQLChangeStreamWatcherWithUnwrap(
	watcher *PostgreSQLChangeStreamWatcher,
	decision ...client.ResumeControlDecision,
) *PostgreSQLChangeStreamWatcherWithUnwrap {
	resumeControlDecision := client.ResumeControlDecision{}
	if len(decision) > 0 {
		resumeControlDecision = decision[0]
	}

	return &PostgreSQLChangeStreamWatcherWithUnwrap{
		watcher:  watcher,
		adapter:  NewPostgreSQLChangeStreamAdapter(watcher),
		decision: resumeControlDecision,
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

// LagState delegates to the wrapped watcher so lag survives the wrapper.
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.watcher.LagState()
}

// Every type a consumer can be handed must report lag, or the assertion that looks for it
// answers for the wrapper instead of the watcher underneath. Asserted here so adding a wrapper
// without the pass-through fails the build rather than silently reporting no lag.
var (
	_ lagstate.Provider = (*PostgreSQLChangeStreamWatcher)(nil)
	_ lagstate.Provider = (*PostgreSQLChangeStreamAdapter)(nil)
	_ lagstate.Provider = (*PostgreSQLChangeStreamWatcherWithUnwrap)(nil)
)

// Unwrap returns the adapter as client.ChangeStreamWatcher for backward compatibility
// This allows services to unwrap the PostgreSQL watcher to the legacy interface
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) Unwrap() client.ChangeStreamWatcher {
	return w.adapter
}

// ResumeControlDecision returns the watcher's current resume-control startup decision.
func (w *PostgreSQLChangeStreamWatcherWithUnwrap) ResumeControlDecision() client.ResumeControlDecision {
	return w.decision
}
