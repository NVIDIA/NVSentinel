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

package mongodb

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MongoMaintenanceEventStore implements MaintenanceEventStore for MongoDB
type MongoMaintenanceEventStore struct {
	databaseClient   client.DatabaseClient
	collectionClient client.CollectionClient
}

// NewMongoMaintenanceEventStore creates a new MongoDB maintenance event store
func NewMongoMaintenanceEventStore(
	databaseClient client.DatabaseClient,
	collectionClient client.CollectionClient,
) datastore.MaintenanceEventStore {
	return &MongoMaintenanceEventStore{
		databaseClient:   databaseClient,
		collectionClient: collectionClient,
	}
}

// UpsertMaintenanceEvent upserts a maintenance event
func (m *MongoMaintenanceEventStore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	// Create filter based on event ID
	filter := map[string]interface{}{
		"eventId": event.EventID,
	}

	// Upsert the document
	_, err := m.databaseClient.UpsertDocument(ctx, filter, event)
	if err != nil {
		return fmt.Errorf("failed to upsert maintenance event: %w", err)
	}

	return nil
}

// FindEventsToTriggerQuarantine finds events that should trigger quarantine
func (m *MongoMaintenanceEventStore) FindEventsToTriggerQuarantine(
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

	cursor, err := m.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to query events for quarantine trigger",
			err,
		).WithMetadata("triggerTimeLimit", triggerTimeLimit.String())
	}
	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode maintenance events for quarantine trigger",
			err,
		).WithMetadata("triggerTimeLimit", triggerTimeLimit.String())
	}

	return results, nil
}

// FindEventsToTriggerHealthy finds events that should trigger healthy status
func (m *MongoMaintenanceEventStore) FindEventsToTriggerHealthy(
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

	cursor, err := m.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to query events for healthy trigger",
			err,
		).WithMetadata("healthyDelay", healthyDelay.String())
	}
	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode maintenance events for healthy trigger",
			err,
		).WithMetadata("healthyDelay", healthyDelay.String())
	}

	return results, nil
}

// UpdateEventStatus updates the status of an event
func (m *MongoMaintenanceEventStore) UpdateEventStatus(
	ctx context.Context,
	eventID string,
	newStatus model.InternalStatus,
) error {
	// Update the event status
	err := m.databaseClient.UpdateDocumentStatus(ctx, eventID, "status", newStatus)
	if err != nil {
		return fmt.Errorf("failed to update event status: %w", err)
	}

	return nil
}

// GetLastProcessedEventTimestampByCSP gets the last processed event timestamp for a CSP
func (m *MongoMaintenanceEventStore) GetLastProcessedEventTimestampByCSP(
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

	var latestEvent model.MaintenanceEvent

	found, err = client.FindOneWithExists(ctx, m.databaseClient, filter, findOptions, &latestEvent)
	if err != nil {
		return time.Time{}, false, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to query last processed log timestamp",
			err,
		).WithMetadata("csp", string(cspType)).WithMetadata("cluster", clusterName)
	}

	if !found {
		return time.Time{}, false, nil
	}

	// Use EventReceivedTimestamp as the marker for when we processed it
	return latestEvent.EventReceivedTimestamp, true, nil
}

// FindLatestActiveEventByNodeAndType finds the latest active event by node and type
func (m *MongoMaintenanceEventStore) FindLatestActiveEventByNodeAndType(
	ctx context.Context,
	nodeName string,
	maintenanceType model.MaintenanceType,
	statuses []model.InternalStatus,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" || maintenanceType == "" || len(statuses) == 0 {
		return nil, false, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"nodeName, maintenanceType, and at least one status are required",
			nil,
		)
	}

	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("nodeName", nodeName).
		Eq("maintenanceType", maintenanceType).
		In("status", statuses).
		Build()

	// Sort by LastUpdatedTimestamp descending to get the latest one
	findOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"lastUpdatedTimestamp": -1},
	}

	var latestEvent model.MaintenanceEvent

	found, err := client.FindOneWithExists(ctx, m.databaseClient, filter, findOptions, &latestEvent)
	if err != nil {
		return nil, false, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to query latest active event",
			err,
		).WithMetadata("nodeName", nodeName).WithMetadata("maintenanceType", string(maintenanceType))
	}

	if !found {
		return nil, false, nil
	}

	return &latestEvent, true, nil
}

// FindLatestOngoingEventByNode finds the latest ongoing event by node
func (m *MongoMaintenanceEventStore) FindLatestOngoingEventByNode(
	ctx context.Context,
	nodeName string,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" {
		return nil, false, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"nodeName is required",
			nil,
		)
	}

	// Query for ongoing events (not completed or cancelled)
	filter := client.NewFilterBuilder().
		Eq("nodeName", nodeName).
		In("status", []model.InternalStatus{
			model.StatusDetected,
			model.StatusQuarantineTriggered,
			model.StatusMaintenanceOngoing,
		}).
		Build()

	opts := &client.FindOneOptions{
		Sort: map[string]interface{}{"lastUpdatedTimestamp": -1},
	}

	var event model.MaintenanceEvent

	found, err := client.FindOneWithExists(ctx, m.databaseClient, filter, opts, &event)
	if err != nil {
		return nil, false, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to query latest ongoing event for node",
			err,
		).WithMetadata("nodeName", nodeName)
	}

	if !found {
		return nil, false, nil
	}

	return &event, true, nil
}

// FindActiveEventsByStatuses finds active events by statuses
func (m *MongoMaintenanceEventStore) FindActiveEventsByStatuses(
	ctx context.Context,
	csp model.CSP,
	statuses []string,
) ([]model.MaintenanceEvent, error) {
	if len(statuses) == 0 {
		return nil, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"at least one status is required",
			nil,
		)
	}

	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("csp", csp).
		In("cspStatus", statuses).
		Build()

	cursor, err := m.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to find active events by statuses",
			err,
		).WithMetadata("csp", string(csp)).WithMetadata("statuses", statuses)
	}
	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode active events by statuses",
			err,
		).WithMetadata("csp", string(csp)).WithMetadata("statuses", statuses)
	}

	return results, nil
}
