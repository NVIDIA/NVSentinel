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
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// PostgreSQLMaintenanceEventStore implements MaintenanceEventStore for PostgreSQL
type PostgreSQLMaintenanceEventStore struct {
	db *sql.DB
}

// NewPostgreSQLMaintenanceEventStore creates a new PostgreSQL maintenance event store
func NewPostgreSQLMaintenanceEventStore(db *sql.DB) *PostgreSQLMaintenanceEventStore {
	return &PostgreSQLMaintenanceEventStore{db: db}
}

// UpsertMaintenanceEvent upserts a maintenance event
func (p *PostgreSQLMaintenanceEventStore) UpsertMaintenanceEvent(
	ctx context.Context, event *model.MaintenanceEvent,
) error {
	// Marshal the event to JSON for document storage
	documentJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal maintenance event: %w", err)
	}

	query := `
		INSERT INTO maintenance_events (
			event_id, csp, cluster_name, node_name, status, csp_status,
			scheduled_start_time, actual_end_time, event_received_timestamp,
			last_updated_timestamp, document
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		)
		ON CONFLICT (event_id)
		DO UPDATE SET
			status = EXCLUDED.status,
			csp_status = EXCLUDED.csp_status,
			scheduled_start_time = EXCLUDED.scheduled_start_time,
			actual_end_time = EXCLUDED.actual_end_time,
			last_updated_timestamp = EXCLUDED.last_updated_timestamp,
			document = EXCLUDED.document,
			updated_at = NOW()
	`

	_, err = p.db.ExecContext(ctx, query,
		event.EventID,
		string(event.CSP),
		event.ClusterName,
		event.NodeName,
		string(event.Status),
		event.CSPStatus,
		event.ScheduledStartTime,
		event.ActualEndTime,
		event.EventReceivedTimestamp,
		event.LastUpdatedTimestamp,
		documentJSON,
	)
	if err != nil {
		return fmt.Errorf("failed to upsert maintenance event: %w", err)
	}

	slog.Debug("Successfully upserted maintenance event", "eventID", event.EventID)

	return nil
}

// FindEventsToTriggerQuarantine finds events ready for quarantine triggering
func (p *PostgreSQLMaintenanceEventStore) FindEventsToTriggerQuarantine(
	ctx context.Context, triggerTimeLimit time.Duration,
) ([]model.MaintenanceEvent, error) {
	query := `
		SELECT document FROM maintenance_events
		WHERE status = $1
		AND scheduled_start_time IS NOT NULL
		AND scheduled_start_time <= $2
		ORDER BY scheduled_start_time ASC
	`

	triggerTime := time.Now().Add(triggerTimeLimit)

	rows, err := p.db.QueryContext(ctx, query, string(model.StatusDetected), triggerTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for quarantine: %w", err)
	}

	defer rows.Close()

	var events []model.MaintenanceEvent

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan maintenance event: %w", err)
		}

		var event model.MaintenanceEvent
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal maintenance event: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating maintenance event rows: %w", err)
	}

	return events, nil
}

// FindEventsToTriggerHealthy finds events ready for healthy triggering
func (p *PostgreSQLMaintenanceEventStore) FindEventsToTriggerHealthy(
	ctx context.Context, healthyDelay time.Duration,
) ([]model.MaintenanceEvent, error) {
	query := `
		SELECT document FROM maintenance_events
		WHERE status = $1
		AND actual_end_time IS NOT NULL
		AND actual_end_time <= $2
		ORDER BY actual_end_time ASC
	`

	healthyTime := time.Now().Add(-healthyDelay)

	rows, err := p.db.QueryContext(ctx, query, string(model.StatusQuarantineTriggered), healthyTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for healthy trigger: %w", err)
	}

	defer rows.Close()

	var events []model.MaintenanceEvent

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan maintenance event: %w", err)
		}

		var event model.MaintenanceEvent
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal maintenance event: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating maintenance event rows: %w", err)
	}

	return events, nil
}

// UpdateEventStatus updates the status of a maintenance event
func (p *PostgreSQLMaintenanceEventStore) UpdateEventStatus(
	ctx context.Context, eventID string, newStatus model.InternalStatus,
) error {
	query := `
		UPDATE maintenance_events
		SET status = $1, last_updated_timestamp = $2, updated_at = NOW()
		WHERE event_id = $3
	`

	result, err := p.db.ExecContext(ctx, query, string(newStatus), time.Now(), eventID)
	if err != nil {
		return fmt.Errorf("failed to update maintenance event status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("maintenance event not found: %s", eventID)
	}

	slog.Debug("Successfully updated maintenance event status", "eventID", eventID, "status", newStatus)

	return nil
}

// GetLastProcessedEventTimestampByCSP gets the last processed event timestamp for a CSP
func (p *PostgreSQLMaintenanceEventStore) GetLastProcessedEventTimestampByCSP(
	ctx context.Context, clusterName string, cspType model.CSP, cspNameForLog string,
) (timestamp time.Time, found bool, err error) {
	query := `
		SELECT event_received_timestamp FROM maintenance_events
		WHERE csp = $1 AND cluster_name = $2
		ORDER BY event_received_timestamp DESC
		LIMIT 1
	`

	var ts time.Time

	err = p.db.QueryRowContext(ctx, query, string(cspType), clusterName).Scan(&ts)
	if err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, fmt.Errorf("failed to get last processed event timestamp: %w", err)
	}

	return ts, true, nil
}

// FindLatestActiveEventByNodeAndType finds the latest active event for a node and type
func (p *PostgreSQLMaintenanceEventStore) FindLatestActiveEventByNodeAndType(
	ctx context.Context,
	nodeName string,
	maintenanceType model.MaintenanceType,
	statuses []model.InternalStatus,
) (*model.MaintenanceEvent, bool, error) {
	if len(statuses) == 0 {
		return nil, false, nil
	}

	// Build status list for IN clause
	statusStrings := make([]interface{}, len(statuses))
	placeholders := make([]string, len(statuses))

	for i, status := range statuses {
		statusStrings[i] = string(status)
		placeholders[i] = "$" + strconv.Itoa(i+3) // Start from $3 since $1 and $2 are node_name and maintenance_type
	}

	var queryBuilder strings.Builder

	queryBuilder.WriteString(`
		SELECT document FROM maintenance_events
		WHERE node_name = $1
		AND document->>'maintenanceType' = $2
		AND status IN (`)
	queryBuilder.WriteString(strings.Join(placeholders, ","))
	queryBuilder.WriteString(`)
		ORDER BY event_received_timestamp DESC
		LIMIT 1
	`)

	query := queryBuilder.String()
	params := []interface{}{nodeName, string(maintenanceType)}

	params = append(params, statusStrings...)

	var documentJSON []byte

	err := p.db.QueryRowContext(ctx, query, params...).Scan(&documentJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("failed to find latest active event: %w", err)
	}

	var event model.MaintenanceEvent
	if err := json.Unmarshal(documentJSON, &event); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal maintenance event: %w", err)
	}

	return &event, true, nil
}

// FindLatestOngoingEventByNode finds the latest ongoing event for a node
func (p *PostgreSQLMaintenanceEventStore) FindLatestOngoingEventByNode(
	ctx context.Context, nodeName string,
) (*model.MaintenanceEvent, bool, error) {
	ongoingStatuses := []model.InternalStatus{
		model.StatusDetected,
		model.StatusQuarantineTriggered,
		model.StatusMaintenanceOngoing,
	}

	return p.FindLatestActiveEventByNodeAndType(ctx, nodeName, "", ongoingStatuses)
}

// FindActiveEventsByStatuses finds active events by statuses for a CSP
func (p *PostgreSQLMaintenanceEventStore) FindActiveEventsByStatuses(
	ctx context.Context, csp model.CSP, statuses []string,
) ([]model.MaintenanceEvent, error) {
	if len(statuses) == 0 {
		return []model.MaintenanceEvent{}, nil
	}

	// Build status list for IN clause
	statusParams := make([]interface{}, len(statuses))
	placeholders := make([]string, len(statuses))

	for i, status := range statuses {
		statusParams[i] = status
		placeholders[i] = "$" + strconv.Itoa(i+2) // Start from $2 since $1 is csp
	}

	var queryBuilder strings.Builder

	queryBuilder.WriteString(`
		SELECT document FROM maintenance_events
		WHERE csp = $1
		AND status IN (`)
	queryBuilder.WriteString(strings.Join(placeholders, ","))
	queryBuilder.WriteString(`)
		ORDER BY event_received_timestamp DESC
	`)

	query := queryBuilder.String()
	params := []interface{}{string(csp)}

	params = append(params, statusParams...)

	rows, err := p.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query active events by statuses: %w", err)
	}
	defer rows.Close()

	var events []model.MaintenanceEvent

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan maintenance event: %w", err)
		}

		var event model.MaintenanceEvent
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal maintenance event: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating maintenance event rows: %w", err)
	}

	return events, nil
}

// Verify that PostgreSQLMaintenanceEventStore implements the MaintenanceEventStore interface
var _ datastore.MaintenanceEventStore = (*PostgreSQLMaintenanceEventStore)(nil)
