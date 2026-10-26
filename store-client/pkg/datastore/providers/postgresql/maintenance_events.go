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

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// PostgreSQLMaintenanceEventStore implements MaintenanceEventStore for PostgreSQL
type PostgreSQLMaintenanceEventStore struct {
	db *sql.DB
}

// NewPostgreSQLMaintenanceEventStore creates a new PostgreSQL maintenance event store
func NewPostgreSQLMaintenanceEventStore(db *sql.DB) *PostgreSQLMaintenanceEventStore {
	return &PostgreSQLMaintenanceEventStore{db: db}
}

// upsertMaintenanceEventWithResult upserts a maintenance event and returns whether it was an INSERT.
// This is used internally by database_client.go to return accurate UpdateResult counts.
func (p *PostgreSQLMaintenanceEventStore) upsertMaintenanceEventWithResult(
	ctx context.Context, event *model.MaintenanceEvent,
) (wasInserted bool, err error) {
	// Marshal the event to JSON for document storage
	documentJSON, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("failed to marshal maintenance event: %w", err)
	}

	// Build the preserved status value for use in both column and document updates.
	// This ensures consistency between the status column and document.status field.
	// RETURNING (xmax = 0) tells us if it was INSERT (true) or UPDATE (false)
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
			-- Preserve trigger engine statuses ONLY during ongoing maintenance (not when completing)
			-- When MAINTENANCE_COMPLETE arrives, we MUST update status so trigger engine can find it
			-- and send the HEALTHY event to uncordon the node
			status = CASE
				WHEN maintenance_events.status IN ('QUARANTINE_TRIGGERED', 'HEALTHY_TRIGGERED', 'NODE_READINESS_TIMEOUT')
				     AND EXCLUDED.status NOT IN ('MAINTENANCE_COMPLETE')
				THEN maintenance_events.status
				ELSE EXCLUDED.status
			END,
			csp_status = EXCLUDED.csp_status,
			scheduled_start_time = EXCLUDED.scheduled_start_time,
			actual_end_time = EXCLUDED.actual_end_time,
			last_updated_timestamp = EXCLUDED.last_updated_timestamp,
			-- Update document.status to match the column status determined above
			document = jsonb_set(
				EXCLUDED.document, 
				'{status}', 
				to_jsonb(CASE
					WHEN maintenance_events.status IN ('QUARANTINE_TRIGGERED', 'HEALTHY_TRIGGERED', 'NODE_READINESS_TIMEOUT')
					     AND EXCLUDED.status NOT IN ('MAINTENANCE_COMPLETE')
					THEN maintenance_events.status
					ELSE EXCLUDED.status
				END)
			),
			updated_at = NOW()
		RETURNING (xmax = 0)
	`

	err = p.db.QueryRowContext(ctx, query,
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
	).Scan(&wasInserted)
	if err != nil {
		return false, fmt.Errorf("failed to upsert maintenance event: %w", err)
	}

	slog.Debug("Successfully upserted maintenance event", "eventID", event.EventID, "wasInserted", wasInserted)

	return wasInserted, nil
}
