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

package datastore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

// Removed unused constants

// Store defines the interface for datastore operations related to maintenance events.
type Store interface {
	UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error
	FindEventsToTriggerQuarantine(ctx context.Context, triggerTimeLimit time.Duration) ([]model.MaintenanceEvent, error)
	FindEventsToTriggerHealthy(ctx context.Context, healthyDelay time.Duration) ([]model.MaintenanceEvent, error)
	UpdateEventStatus(ctx context.Context, eventID string, newStatus model.InternalStatus) error
	GetLastProcessedEventTimestampByCSP(
		ctx context.Context,
		clusterName string,
		cspType model.CSP,
		cspNameForLog string,
	) (timestamp time.Time, found bool, err error)
	FindLatestActiveEventByNodeAndType(
		ctx context.Context,
		nodeName string,
		maintenanceType model.MaintenanceType,
		statuses []model.InternalStatus,
	) (*model.MaintenanceEvent, bool, error)
	FindLatestOngoingEventByNode(ctx context.Context, nodeName string) (*model.MaintenanceEvent, bool, error)
	FindActiveEventsByStatuses(ctx context.Context, csp model.CSP, statuses []string) ([]model.MaintenanceEvent, error)
}

// DatabaseStore implements the Store interface using store-client.
type DatabaseStore struct {
	databaseClient client.DatabaseClient
	collectionName string
}

var _ Store = (*DatabaseStore)(nil)

// NewStore creates a new database store client using store-client.
func NewStore(ctx context.Context, databaseClientCertMountPath *string) (*DatabaseStore, error) {
	// Use centralized configuration with maintenance event collection support
	var certPath string
	if databaseClientCertMountPath != nil {
		certPath = *databaseClientCertMountPath
	}

	// Use database-agnostic collection type configuration
	// This approach encapsulates MongoDB-specific environment variables within store-client
	databaseConfig, err := config.NewDatabaseConfigForCollectionType(
		certPath,
		config.CollectionTypeMaintenanceEvents,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create database config: %w", err)
	}

	databaseCollection := databaseConfig.GetCollectionName()

	// Create client factory
	clientFactory := factory.NewClientFactory(databaseConfig)

	// Create database client
	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	slog.Info("Database client initialized successfully using store-client",
		"collection", databaseCollection)

	// Note: Index creation is not directly supported by store-client's DatabaseClient interface.
	// Indexes should be created manually or through database administration tools.
	// The following indexes are recommended for optimal performance:
	// 1. unique index on "eventId" field
	// 2. compound index on "status" + "scheduledStartTime" fields
	// 3. compound index on "status" + "actualEndTime" fields
	// 4. compound index on "csp" + "clusterName" + "eventReceivedTimestamp" (desc) fields
	// 5. index on "cspStatus" field

	return &DatabaseStore{
		databaseClient: databaseClient,
		collectionName: databaseCollection,
	}, nil
}

// executeUpsert performs the UpsertDocument with retries for the given merged event using store-client.
func (s *DatabaseStore) executeUpsert(ctx context.Context, filter map[string]interface{},
	event *model.MaintenanceEvent) error {
	inserted, updated, err := client.RetryableDocumentUpsertWithResult(ctx, s.databaseClient, filter, event,
		client.DefaultMaxRetries, client.DefaultRetryDelay)
	if err != nil {
		return fmt.Errorf("upsert failed for event %s: %w", event.EventID, err)
	}

	// Log the semantic result
	switch {
	case inserted > 0:
		slog.Debug("Inserted new maintenance event", "eventID", event.EventID)
	case updated > 0:
		slog.Debug("Updated existing maintenance event", "eventID", event.EventID)
	default:
		slog.Debug("Matched existing maintenance event but no fields changed",
			"eventID", event.EventID)
	}

	return nil
}

// UpsertMaintenanceEvent inserts or updates a maintenance event.
// Metrics are handled by the caller (Processor).
func (s *DatabaseStore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	if event == nil || event.EventID == "" {
		return fmt.Errorf("invalid event passed to UpsertMaintenanceEvent (nil or empty EventID)")
	}

	filter := map[string]interface{}{"eventId": event.EventID}
	event.LastUpdatedTimestamp = time.Now().UTC()

	// Since Processor now prepares the event fully, we directly upsert.
	// The fetchExistingEvent and mergeEvents logic is removed based on the confidence
	// that each EventID is processed once in its final state by the Processor.
	slog.Debug("Upserting event directly as prepared by Processor", "eventID", event.EventID)

	return s.executeUpsert(ctx, filter, event)
}

// FindEventsToTriggerQuarantine finds events ready for quarantine trigger.
// Metrics (duration, errors) handled by the caller (Trigger Engine).
func (s *DatabaseStore) FindEventsToTriggerQuarantine(
	ctx context.Context,
	triggerTimeLimit time.Duration,
) ([]model.MaintenanceEvent, error) {
	now := time.Now().UTC()
	triggerBefore := now.Add(triggerTimeLimit)

	// Use database-agnostic filter building
	statusFilter := client.BuildStatusFilter("status", model.StatusDetected)
	timeFilter := client.BuildTimeRangeFilter("scheduledStartTime", &now, &triggerBefore)

	// Combine filters
	filter := client.NewFilterBuilder().
		And(statusFilter, timeFilter).
		Build()

	slog.Debug("Querying for quarantine triggers",
		"status", model.StatusDetected,
		"currentTime", now.Format(time.RFC3339),
		"triggerBefore", triggerBefore.Format(time.RFC3339))

	cursor, err := s.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for quarantine trigger: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for quarantine trigger: %w", err)
	}

	slog.Debug("Found events potentially ready for quarantine trigger", "count", len(results))

	return results, nil
}

// FindEventsToTriggerHealthy finds completed events ready for healthy trigger.
// Metrics (duration, errors) handled by the caller (Trigger Engine).
func (s *DatabaseStore) FindEventsToTriggerHealthy(
	ctx context.Context,
	healthyDelay time.Duration,
) ([]model.MaintenanceEvent, error) {
	now := time.Now().UTC()
	triggerIfEndedBefore := now.Add(-healthyDelay) // Event must have ended *before* or *at* this time

	// Use database-agnostic filter building
	statusFilter := client.BuildStatusFilter("status", model.StatusMaintenanceComplete)
	notNullFilter := client.BuildNotNullFilter("actualEndTime")
	timeFilter := client.NewFilterBuilder().Lte("actualEndTime", triggerIfEndedBefore).Build()

	// Combine filters
	filter := client.NewFilterBuilder().
		And(statusFilter, notNullFilter, timeFilter).
		Build()

	slog.Debug("Querying for healthy triggers",
		"status", model.StatusMaintenanceComplete,
		"actualEndTimeBefore", triggerIfEndedBefore.Format(time.RFC3339))

	cursor, err := s.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for healthy trigger: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for healthy trigger: %w", err)
	}

	slog.Debug("Found events potentially ready for healthy trigger", "count", len(results))

	return results, nil
}

// UpdateEventStatus updates only the status and timestamp.
// Metrics handled by the caller (Trigger Engine).
func (s *DatabaseStore) UpdateEventStatus(ctx context.Context, eventID string, newStatus model.InternalStatus) error {
	if eventID == "" {
		return fmt.Errorf("cannot update status for empty eventID")
	}

	filter := client.BuildStatusFilter("eventId", eventID)
	update := client.BuildSetUpdate(map[string]interface{}{
		"status":               newStatus,
		"lastUpdatedTimestamp": time.Now().UTC(),
	})

	// Use semantic update method from store-client
	matched, _, err := client.RetryableUpdateWithResult(ctx, s.databaseClient, filter, update,
		client.DefaultMaxRetries, client.DefaultRetryDelay)
	if err != nil {
		return fmt.Errorf("failed to update status for event (EventID: %s): %w", eventID, err)
	}

	if matched == 0 {
		slog.Warn("Attempted to update status for non-existent event", "eventID", eventID)
		return nil // Not an error if event is gone
	}

	slog.Debug("Successfully updated status for event",
		"newStatus", newStatus,
		"eventID", eventID)

	return nil
}

// GetLastProcessedEventTimestampByCSP is a helper to get the latest event timestamp for a given CSP.
func (s *DatabaseStore) GetLastProcessedEventTimestampByCSP(
	ctx context.Context,
	clusterName string,
	cspType model.CSP,
	cspNameForLog string,
) (timestamp time.Time, found bool, err error) {
	// Use database-agnostic filter building
	builder := client.NewFilterBuilder().Eq("csp", cspType)
	if clusterName != "" {
		builder = builder.Eq("clusterName", clusterName)
	}

	filter := builder.Build()
	findOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"eventReceivedTimestamp": -1},
	}

	slog.Debug("Querying for last processed timestamp",
		"csp", cspNameForLog)

	var latestEvent model.MaintenanceEvent

	found, err = client.FindOneWithExists(ctx, s.databaseClient, filter, findOptions, &latestEvent)
	if err != nil {
		slog.Error("Failed to query last processed log timestamp",
			"csp", cspNameForLog,
			"cluster", clusterName,
			"error", err)

		return time.Time{}, false, fmt.Errorf("failed to query last %s log timestamp: %w", cspNameForLog, err)
	}

	if !found {
		slog.Debug("No previous event timestamp found in datastore",
			"csp", cspNameForLog,
			"cluster", clusterName)

		return time.Time{}, false, nil
	}

	// Use EventReceivedTimestamp as the marker for when we processed it
	slog.Debug("Found last processed log timestamp",
		"csp", cspNameForLog,
		"cluster", clusterName,
		"timestamp", latestEvent.EventReceivedTimestamp,
		"eventID", latestEvent.EventID)

	return latestEvent.EventReceivedTimestamp, true, nil
}

// FindLatestActiveEventByNodeAndType finds the most recently updated event for a
// given node, type, and one of several statuses.
func (s *DatabaseStore) FindLatestActiveEventByNodeAndType(
	ctx context.Context,
	nodeName string,
	maintenanceType model.MaintenanceType,
	statuses []model.InternalStatus,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" || maintenanceType == "" || len(statuses) == 0 {
		return nil, false, fmt.Errorf("nodeName, maintenanceType, and at least one status are required")
	}

	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("nodeName", nodeName).
		Eq("maintenanceType", maintenanceType).
		In("status", statuses).
		Build()

	// Sort by LastUpdatedTimestamp descending to get the latest one.
	// If multiple have the exact same LastUpdatedTimestamp, this will pick one arbitrarily among them.
	// Consider adding a secondary sort key if more deterministic behavior is needed in such rare cases.
	findOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"lastUpdatedTimestamp": -1},
	}

	slog.Debug("Querying for latest active event",
		"node", nodeName,
		"maintenanceType", maintenanceType,
		"sort", "lastUpdatedTimestamp: -1")

	var latestEvent model.MaintenanceEvent

	found, err := client.FindOneWithExists(ctx, s.databaseClient, filter, findOptions, &latestEvent)
	if err != nil {
		slog.Error("Failed to query latest active event",
			"node", nodeName,
			"type", maintenanceType,
			"statuses", statuses,
			"error", err)

		return nil, false, fmt.Errorf("failed to query latest active event: %w", err)
	}

	if !found {
		slog.Info("No active event found", "node", nodeName, "type", maintenanceType, "statuses", statuses)
		return nil, false, nil
	}

	slog.Info("Found latest active event",
		"eventID", latestEvent.EventID,
		"node", nodeName,
		"type", maintenanceType,
		"statuses", statuses)

	return &latestEvent, true, nil
}

// FindLatestOngoingEventByNode finds the most recently updated ONGOING event for a given node.
func (s *DatabaseStore) FindLatestOngoingEventByNode(
	ctx context.Context,
	nodeName string,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" {
		return nil, false, fmt.Errorf("nodeName is required")
	}

	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("nodeName", nodeName).
		Eq("status", model.StatusMaintenanceOngoing).
		Build()

	opts := &client.FindOneOptions{
		Sort: map[string]interface{}{"lastUpdatedTimestamp": -1},
	}

	var event model.MaintenanceEvent

	found, err := client.FindOneWithExists(ctx, s.databaseClient, filter, opts, &event)
	if err != nil {
		return nil, false, fmt.Errorf("query latest ongoing event for node %s: %w", nodeName, err)
	}

	if !found {
		slog.Debug("No ongoing event found for node", "node", nodeName)
		return nil, false, nil
	}

	slog.Debug("Found ongoing event", "eventID", event.EventID, "node", nodeName)

	return &event, true, nil
}

// FindActiveEventsByStatuses finds active events by their csp status.
func (s *DatabaseStore) FindActiveEventsByStatuses(
	ctx context.Context,
	csp model.CSP,
	statuses []string,
) ([]model.MaintenanceEvent, error) {
	if len(statuses) == 0 {
		return nil, fmt.Errorf("at least one status is required")
	}

	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("csp", csp).
		In("cspStatus", statuses).
		Build()

	slog.Debug("Querying for active events",
		"csp", csp,
		"statuses", statuses)

	cursor, err := s.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query active events: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for active events: %w", err)
	}

	slog.Debug("Found active events", "count", len(results))

	return results, nil
}
