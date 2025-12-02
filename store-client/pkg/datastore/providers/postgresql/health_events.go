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
	"strings"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// PostgreSQLHealthEventStore implements HealthEventStore for PostgreSQL
type PostgreSQLHealthEventStore struct {
	db *sql.DB
}

// NewPostgreSQLHealthEventStore creates a new PostgreSQL health event store
func NewPostgreSQLHealthEventStore(db *sql.DB) *PostgreSQLHealthEventStore {
	return &PostgreSQLHealthEventStore{db: db}
}

// InsertHealthEvents inserts health events into the database
func (p *PostgreSQLHealthEventStore) InsertHealthEvents(
	ctx context.Context, eventWithStatus *datastore.HealthEventWithStatus,
) error {
	documentJSON, err := json.Marshal(eventWithStatus)
	if err != nil {
		return fmt.Errorf("failed to marshal health event: %w", err)
	}

	indexFields := p.extractIndexFields(eventWithStatus)
	nodeQuarantined := p.convertNodeQuarantinedStatus(eventWithStatus.HealthEventStatus.NodeQuarantined)

	return p.insertHealthEventRecord(ctx, indexFields, nodeQuarantined, eventWithStatus, documentJSON)
}

// InsertHealthEventsWithIndexFields inserts health events with pre-extracted index fields
// This is used when index fields are extracted from the protobuf before JSON marshaling
func (p *PostgreSQLHealthEventStore) InsertHealthEventsWithIndexFields(
	ctx context.Context,
	eventWithStatus *datastore.HealthEventWithStatus,
	indexFields healthEventIndexFields,
) error {
	documentJSON, err := json.Marshal(eventWithStatus)
	if err != nil {
		return fmt.Errorf("failed to marshal health event: %w", err)
	}

	nodeQuarantined := p.convertNodeQuarantinedStatus(eventWithStatus.HealthEventStatus.NodeQuarantined)

	return p.insertHealthEventRecord(ctx, indexFields, nodeQuarantined, eventWithStatus, documentJSON)
}

// healthEventIndexFields contains fields extracted for indexing
type healthEventIndexFields struct {
	nodeName          string
	eventType         string
	severity          string
	recommendedAction string
}

// extractIndexFields extracts key fields for indexing from the health event
func (p *PostgreSQLHealthEventStore) extractIndexFields(
	eventWithStatus *datastore.HealthEventWithStatus,
) healthEventIndexFields {
	fields := healthEventIndexFields{}

	// First try to access as protobuf (the actual type used by platform-connectors)
	if protoEvent, ok := eventWithStatus.HealthEvent.(*protos.HealthEvent); ok {
		fields.nodeName = protoEvent.NodeName
		fields.eventType = protoEvent.CheckName
		fields.severity = protoEvent.ComponentClass
		fields.recommendedAction = protoEvent.RecommendedAction.String()

		return fields
	}

	// Fallback: try map interface (for backward compatibility with MongoDB)
	return p.extractFromMap(eventWithStatus.HealthEvent)
}

// extractFromMap extracts fields from map interface (MongoDB compatibility)
func (p *PostgreSQLHealthEventStore) extractFromMap(healthEvent interface{}) healthEventIndexFields {
	fields := healthEventIndexFields{}

	healthEventMap, ok := healthEvent.(map[string]interface{})
	if !ok {
		slog.Debug("Failed to extract fields - type assertion to map failed", "actualType", fmt.Sprintf("%T", healthEvent))

		return fields
	}

	if nodeNameVal, exists := healthEventMap["nodeName"]; exists {
		if nodeNameStr, ok := nodeNameVal.(string); ok {
			fields.nodeName = nodeNameStr
		}
	}

	if eventTypeVal, exists := healthEventMap["checkName"]; exists {
		if eventTypeStr, ok := eventTypeVal.(string); ok {
			fields.eventType = eventTypeStr
		}
	}

	if severityVal, exists := healthEventMap["componentClass"]; exists {
		if severityStr, ok := severityVal.(string); ok {
			fields.severity = severityStr
		}
	}

	if actionVal, exists := healthEventMap["recommendedAction"]; exists {
		if actionStr, ok := actionVal.(string); ok {
			fields.recommendedAction = actionStr
		}
	}

	slog.Debug("Extracted fields from map", "nodeName", fields.nodeName)

	return fields
}

// convertNodeQuarantinedStatus converts node quarantined status to string pointer
func (p *PostgreSQLHealthEventStore) convertNodeQuarantinedStatus(status *datastore.Status) *string {
	if status == nil {
		return nil
	}

	statusStr := string(*status)

	return &statusStr
}

// insertHealthEventRecord inserts the health event record into the database
func (p *PostgreSQLHealthEventStore) insertHealthEventRecord(
	ctx context.Context,
	fields healthEventIndexFields,
	nodeQuarantined *string,
	eventWithStatus *datastore.HealthEventWithStatus,
	documentJSON []byte,
) error {
	query := `
		INSERT INTO health_events (
			node_name, event_type, severity, recommended_action,
			node_quarantined, user_pods_eviction_status, user_pods_eviction_message,
			fault_remediated, last_remediation_timestamp, document
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10
		)
	`

	// For initial insert, use NULL for user_pods_eviction_status if it's set to InProgress
	// This allows the state machine to progress properly:
	// 1. Event inserted with status = NULL
	// 2. Fault-quarantine sets nodeQuarantined = "Quarantined"
	// 3. Node-drainer transitions status from NULL -> "InProgress" when it starts draining
	var evictionStatus *string
	if eventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status == datastore.StatusInProgress {
		// Don't write InProgress on initial insert - let node-drainer set it when it actually starts
		evictionStatus = nil
	} else if eventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status != "" {
		// For any other non-empty status, preserve it
		statusStr := string(eventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status)
		evictionStatus = &statusStr
	}

	_, err := p.db.ExecContext(ctx, query,
		fields.nodeName,
		fields.eventType,
		fields.severity,
		fields.recommendedAction,
		nodeQuarantined,
		evictionStatus,
		eventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Message,
		eventWithStatus.HealthEventStatus.FaultRemediated,
		eventWithStatus.HealthEventStatus.LastRemediationTimestamp,
		documentJSON,
	)
	if err != nil {
		return fmt.Errorf("failed to insert health event: %w", err)
	}

	slog.Debug("Successfully inserted health event", "node", fields.nodeName)

	return nil
}

// UpdateHealthEventStatus updates the status of a health event by ID
func (p *PostgreSQLHealthEventStore) UpdateHealthEventStatus(
	ctx context.Context, id string, status datastore.HealthEventStatus,
) error {
	// PostgreSQL stores health event status in BOTH table columns AND the JSONB document.
	// We must update BOTH to keep them in sync for aggregation pipelines to work correctly.
	// Aggregation rules query the JSONB document, not the table columns.
	//
	// IMPORTANT: When nodeQuarantined is NULL, we need to pass it differently to avoid
	// "inconsistent types deduced for parameter" errors.
	var query string

	var params []interface{}

	if status.NodeQuarantined != nil {
		// NodeQuarantined has a value - update it in both column and JSONB
		statusStr := string(*status.NodeQuarantined)
		//nolint:dupword // SQL query uses nested jsonb_set calls
		query = `
			UPDATE health_events
			SET node_quarantined = $1::text,
			    user_pods_eviction_status = $2::text,
			    user_pods_eviction_message = $3::text,
			    fault_remediated = $4::boolean,
			    last_remediation_timestamp = $5::timestamp,
			    document = jsonb_set(
			        jsonb_set(
			            jsonb_set(
			                jsonb_set(
			                    document,
			                    '{healtheventstatus,nodequarantined}',
			                    to_jsonb($1::text)
			                ),
			                '{healtheventstatus,userpodsevictionstatus,status}',
			                to_jsonb($2::text)
			            ),
			            '{healtheventstatus,userpodsevictionstatus,message}',
			            to_jsonb($3::text)
			        ),
			        '{healtheventstatus,faultremediated}',
			        to_jsonb($4::boolean)
			    ),
			    updated_at = NOW()
			WHERE id = $6::uuid
		`
		params = []interface{}{
			statusStr,
			string(status.UserPodsEvictionStatus.Status),
			status.UserPodsEvictionStatus.Message,
			status.FaultRemediated,
			status.LastRemediationTimestamp,
			id,
		}
	} else {
		// NodeQuarantined is NULL - only update other fields, skip nodequarantined in JSONB
		//nolint:dupword // SQL query uses nested jsonb_set calls
		query = `
			UPDATE health_events
			SET user_pods_eviction_status = $1::text,
			    user_pods_eviction_message = $2::text,
			    fault_remediated = $3::boolean,
			    last_remediation_timestamp = $4::timestamp,
			    document = jsonb_set(
			        jsonb_set(
			            jsonb_set(
			                document,
			                '{healtheventstatus,userpodsevictionstatus,status}',
			                to_jsonb($1::text)
			            ),
			            '{healtheventstatus,userpodsevictionstatus,message}',
			            to_jsonb($2::text)
			        ),
			        '{healtheventstatus,faultremediated}',
			        to_jsonb($3::boolean)
			    ),
			    updated_at = NOW()
			WHERE id = $5::uuid
		`
		params = []interface{}{
			string(status.UserPodsEvictionStatus.Status),
			status.UserPodsEvictionStatus.Message,
			status.FaultRemediated,
			status.LastRemediationTimestamp,
			id,
		}
	}

	result, err := p.db.ExecContext(ctx, query, params...)
	if err != nil {
		slog.Error("UPDATE query failed", "id", id, "error", err)

		return fmt.Errorf("failed to update health event status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	slog.Debug("UPDATE query succeeded", "id", id, "rows_affected", rowsAffected)

	if rowsAffected == 0 {
		return fmt.Errorf("health event not found: %s", id)
	}

	slog.Debug("Successfully updated health event status", "id", id)

	return nil
}

// UpdateHealthEventStatusByNode updates the status of health events by node name
func (p *PostgreSQLHealthEventStore) UpdateHealthEventStatusByNode(
	ctx context.Context, nodeName string, status datastore.HealthEventStatus,
) error {
	var nodeQuarantined *string

	if status.NodeQuarantined != nil {
		statusStr := string(*status.NodeQuarantined)
		nodeQuarantined = &statusStr
	}

	query := `
		UPDATE health_events
		SET node_quarantined = $1,
		    user_pods_eviction_status = $2,
		    user_pods_eviction_message = $3,
		    fault_remediated = $4,
		    last_remediation_timestamp = $5,
		    updated_at = NOW()
		WHERE node_name = $6
	`

	result, err := p.db.ExecContext(ctx, query,
		nodeQuarantined,
		string(status.UserPodsEvictionStatus.Status),
		status.UserPodsEvictionStatus.Message,
		status.FaultRemediated,
		status.LastRemediationTimestamp,
		nodeName,
	)
	if err != nil {
		return fmt.Errorf("failed to update health event status by node: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	slog.Debug("Successfully updated health event statuses", "node", nodeName, "rows", rowsAffected)

	return nil
}

// FindHealthEventsByNode finds all health events for a specific node
func (p *PostgreSQLHealthEventStore) FindHealthEventsByNode(
	ctx context.Context, nodeName string,
) ([]datastore.HealthEventWithStatus, error) {
	query := `
		SELECT document FROM health_events
		WHERE node_name = $1
		ORDER BY created_at DESC
	`

	rows, err := p.db.QueryContext(ctx, query, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to query health events by node: %w", err)
	}
	defer rows.Close()

	var events []datastore.HealthEventWithStatus

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan health event: %w", err)
		}

		var event datastore.HealthEventWithStatus
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal health event: %w", err)
		}

		// Populate RawEvent for cold-start support
		var rawEvent map[string]interface{}
		if err := json.Unmarshal(documentJSON, &rawEvent); err != nil {
			return nil, fmt.Errorf("failed to unmarshal raw event: %w", err)
		}

		event.RawEvent = rawEvent

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating health event rows: %w", err)
	}

	return events, nil
}

// FindHealthEventsByFilter finds health events based on filter criteria
func (p *PostgreSQLHealthEventStore) FindHealthEventsByFilter(
	ctx context.Context, filter map[string]interface{},
) ([]datastore.HealthEventWithStatus, error) {
	conditions, params := p.buildFilterConditions(filter)
	query := p.buildFilterQuery(conditions)

	return p.executeFilterQuery(ctx, query, params)
}

// buildFilterConditions builds WHERE conditions and parameters from filter map
func (p *PostgreSQLHealthEventStore) buildFilterConditions(
	filter map[string]interface{},
) ([]string, []interface{}) {
	var (
		conditions []string
		params     []interface{}
		paramIndex = 1
	)

	for key, value := range filter {
		condition, param := p.buildSingleCondition(key, value, paramIndex)
		conditions = append(conditions, condition)
		params = append(params, param)
		paramIndex++
	}

	return conditions, params
}

// buildSingleCondition builds a single WHERE condition for a filter key-value pair
func (p *PostgreSQLHealthEventStore) buildSingleCondition(
	key string,
	value interface{},
	paramIndex int,
) (string, interface{}) {
	switch key {
	case "node_name":
		return fmt.Sprintf("node_name = $%d", paramIndex), value
	case "event_type":
		return fmt.Sprintf("event_type = $%d", paramIndex), value
	case "node_quarantined":
		return fmt.Sprintf("node_quarantined = $%d", paramIndex), value
	case "user_pods_eviction_status":
		return fmt.Sprintf("user_pods_eviction_status = $%d", paramIndex), value
	default:
		// For complex JSON queries, use JSONB operators
		return fmt.Sprintf("document->>'%s' = $%d", key, paramIndex), value
	}
}

// buildFilterQuery builds the complete SQL query with WHERE clause
func (p *PostgreSQLHealthEventStore) buildFilterQuery(conditions []string) string {
	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	return fmt.Sprintf(`
		SELECT document FROM health_events
		%s
		ORDER BY created_at DESC
	`, whereClause)
}

// executeFilterQuery executes the filter query and returns results
func (p *PostgreSQLHealthEventStore) executeFilterQuery(
	ctx context.Context,
	query string,
	params []interface{},
) ([]datastore.HealthEventWithStatus, error) {
	rows, err := p.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("failed to query health events by filter: %w", err)
	}
	defer rows.Close()

	var events []datastore.HealthEventWithStatus

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan health event: %w", err)
		}

		var event datastore.HealthEventWithStatus
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal health event: %w", err)
		}

		// Populate RawEvent for cold-start support
		var rawEvent map[string]interface{}
		if err := json.Unmarshal(documentJSON, &rawEvent); err != nil {
			return nil, fmt.Errorf("failed to unmarshal raw event: %w", err)
		}

		event.RawEvent = rawEvent

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating health event rows: %w", err)
	}

	return events, nil
}

// FindHealthEventsByStatus finds health events matching a specific status
func (p *PostgreSQLHealthEventStore) FindHealthEventsByStatus(
	ctx context.Context, status datastore.Status,
) ([]datastore.HealthEventWithStatus, error) {
	query := `
		SELECT document FROM health_events
		WHERE user_pods_eviction_status = $1
		ORDER BY created_at DESC
	`

	rows, err := p.db.QueryContext(ctx, query, string(status))
	if err != nil {
		return nil, fmt.Errorf("failed to query health events by status: %w", err)
	}
	defer rows.Close()

	var events []datastore.HealthEventWithStatus

	for rows.Next() {
		var documentJSON []byte
		if err := rows.Scan(&documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan health event: %w", err)
		}

		var event datastore.HealthEventWithStatus
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal health event: %w", err)
		}

		// Populate RawEvent for cold-start support
		var rawEvent map[string]interface{}
		if err := json.Unmarshal(documentJSON, &rawEvent); err != nil {
			return nil, fmt.Errorf("failed to unmarshal raw event: %w", err)
		}

		event.RawEvent = rawEvent

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating health event rows: %w", err)
	}

	return events, nil
}

// UpdateNodeQuarantineStatus updates node quarantine status for a specific event
func (p *PostgreSQLHealthEventStore) UpdateNodeQuarantineStatus(
	ctx context.Context, eventID string, status datastore.Status,
) error {
	// PostgreSQL stores health event status in BOTH table columns AND the JSONB document.
	// We must update BOTH to keep them in sync for aggregation pipelines to work correctly.
	query := `
		UPDATE health_events
		SET node_quarantined = $1,
		    document = jsonb_set(
		        document,
		        '{healtheventstatus,nodequarantined}',
		        to_jsonb($1::text)
		    ),
		    updated_at = NOW()
		WHERE id = $2
	`

	result, err := p.db.ExecContext(ctx, query, string(status), eventID)
	if err != nil {
		return fmt.Errorf("failed to update node quarantine status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("health event not found: %s", eventID)
	}

	return nil
}

// UpdatePodEvictionStatus updates pod eviction status for a specific event
func (p *PostgreSQLHealthEventStore) UpdatePodEvictionStatus(
	ctx context.Context, eventID string, status datastore.OperationStatus,
) error {
	// PostgreSQL stores health event status in BOTH table columns AND the JSONB document.
	// We must update BOTH to keep them in sync for aggregation pipelines to work correctly.
	query := `
		UPDATE health_events
		SET user_pods_eviction_status = $1,
		    user_pods_eviction_message = $2,
		    document = 
		        jsonb_set(jsonb_set(
		                document,
		            '{healtheventstatus,userpodsevictionstatus,status}',
		            to_jsonb($1::text)
		        ),
		        '{healtheventstatus,userpodsevictionstatus,message}',
		        to_jsonb($2::text)
		    ),
		    updated_at = NOW()
		WHERE id = $3
	`

	result, err := p.db.ExecContext(ctx, query, string(status.Status), status.Message, eventID)
	if err != nil {
		return fmt.Errorf("failed to update pod eviction status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("health event not found: %s", eventID)
	}

	return nil
}

// UpdateRemediationStatus updates remediation status for a specific event
func (p *PostgreSQLHealthEventStore) UpdateRemediationStatus(
	ctx context.Context, eventID string, status interface{},
) error {
	// Convert status to boolean if needed
	var faultRemediated *bool

	switch v := status.(type) {
	case bool:
		faultRemediated = &v

	case *bool:
		faultRemediated = v
	default:
		return fmt.Errorf("invalid remediation status type: %T", status)
	}

	// PostgreSQL stores health event status in BOTH table columns AND the JSONB document.
	// We must update BOTH to keep them in sync for aggregation pipelines to work correctly.
	query := `
		UPDATE health_events
		SET fault_remediated = $1,
		    last_remediation_timestamp = NOW(),
		    document = jsonb_set(
		        document,
		        '{healtheventstatus,faultremediated}',
		        to_jsonb($1::boolean)
		    ),
		    updated_at = NOW()
		WHERE id = $2
	`

	result, err := p.db.ExecContext(ctx, query, faultRemediated, eventID)
	if err != nil {
		return fmt.Errorf("failed to update remediation status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("health event not found: %s", eventID)
	}

	return nil
}

// CheckIfNodeAlreadyDrained checks if a node is already drained
func (p *PostgreSQLHealthEventStore) CheckIfNodeAlreadyDrained(
	ctx context.Context, nodeName string,
) (bool, error) {
	query := `
		SELECT COUNT(*) FROM health_events
		WHERE node_name = $1
		AND user_pods_eviction_status = $2
	`

	var count int64

	err := p.db.QueryRowContext(ctx, query, nodeName, string(datastore.StatusSucceeded)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check if node is already drained: %w", err)
	}

	return count > 0, nil
}

// FindLatestEventForNode finds the latest event for a node
func (p *PostgreSQLHealthEventStore) FindLatestEventForNode(
	ctx context.Context, nodeName string,
) (*datastore.HealthEventWithStatus, error) {
	query := `
		SELECT document FROM health_events
		WHERE node_name = $1
		ORDER BY created_at DESC
		LIMIT 1
	`

	var documentJSON []byte

	err := p.db.QueryRowContext(ctx, query, nodeName).Scan(&documentJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to find latest event for node: %w", err)
	}

	var event datastore.HealthEventWithStatus
	if err := json.Unmarshal(documentJSON, &event); err != nil {
		return nil, fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	// Populate RawEvent for cold-start support
	var rawEvent map[string]interface{}
	if err := json.Unmarshal(documentJSON, &rawEvent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal raw event: %w", err)
	}

	event.RawEvent = rawEvent

	return &event, nil
}

// FindHealthEventsByQuery finds health events using query builder
// PostgreSQL: converts builder to SQL and uses native query
func (p *PostgreSQLHealthEventStore) FindHealthEventsByQuery(ctx context.Context,
	builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	// Convert query builder to SQL
	whereClause, args := builder.ToSQL()

	// Build the full query - include id column for PostgreSQL document identification
	//nolint:gosec // G202 false positive - using parameterized query with placeholders
	query := `
		SELECT id, document
		FROM health_events
		WHERE ` + whereClause + `
	`

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query health events: %w", err)
	}
	defer rows.Close()

	var events []datastore.HealthEventWithStatus

	for rows.Next() {
		var (
			documentID   string
			documentJSON []byte
		)

		if err := rows.Scan(&documentID, &documentJSON); err != nil {
			return nil, fmt.Errorf("failed to scan health event row: %w", err)
		}

		var event datastore.HealthEventWithStatus
		if err := json.Unmarshal(documentJSON, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal health event: %w", err)
		}

		// Populate RawEvent for cold-start support
		var rawEvent map[string]interface{}
		if err := json.Unmarshal(documentJSON, &rawEvent); err != nil {
			return nil, fmt.Errorf("failed to unmarshal raw event: %w", err)
		}

		// Add PostgreSQL id to RawEvent for document identification
		rawEvent["id"] = documentID

		event.RawEvent = rawEvent
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating health event rows: %w", err)
	}

	return events, nil
}

// UpdateHealthEventsByQuery updates health events using query builder
// PostgreSQL: converts builders to SQL and uses native UPDATE
func (p *PostgreSQLHealthEventStore) UpdateHealthEventsByQuery(ctx context.Context,
	queryBuilder datastore.QueryBuilder, updateBuilder datastore.UpdateBuilder) error {
	// Convert query builder to SQL WHERE clause
	whereClause, whereArgs := queryBuilder.ToSQL()

	// Convert update builder to SQL SET clause
	setClause, setArgs := updateBuilder.ToSQL()

	// Combine arguments (SET args come first, then WHERE args)
	//nolint:gocritic // allArgs is a new combined slice, not reassigning
	allArgs := append(setArgs, whereArgs...)

	// Adjust WHERE clause parameter numbers

	// SET clause uses $1, $2, etc., so WHERE clause needs to start after those
	adjustedWhereClause := whereClause

	for i := len(setArgs); i >= 1; i-- {
		oldParam := fmt.Sprintf("$%d", i)
		newParam := fmt.Sprintf("$%d", i+len(setArgs))
		adjustedWhereClause = strings.ReplaceAll(adjustedWhereClause, oldParam, newParam)
	}

	// Build the full UPDATE query
	//nolint:gosec // G202 false positive - using parameterized query with placeholders
	query := `
		UPDATE health_events
		SET ` + setClause + `, updatedAt = NOW()
		WHERE ` + adjustedWhereClause

	result, err := p.db.ExecContext(ctx, query, allArgs...)
	if err != nil {
		return fmt.Errorf("failed to update health events: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	slog.Debug("Updated health events",
		"rows_affected", rowsAffected,
		"query", query)

	return nil
}

// Verify that PostgreSQLHealthEventStore implements the HealthEventStore interface
var _ datastore.HealthEventStore = (*PostgreSQLHealthEventStore)(nil)
