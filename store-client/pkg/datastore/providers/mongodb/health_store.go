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

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MongoHealthEventStore implements HealthEventStore for MongoDB
type MongoHealthEventStore struct {
	databaseClient   client.DatabaseClient
	collectionClient client.CollectionClient
}

// NewMongoHealthEventStore creates a new MongoDB health event store
func NewMongoHealthEventStore(databaseClient client.DatabaseClient,
	collectionClient client.CollectionClient) datastore.HealthEventStore {
	return &MongoHealthEventStore{
		databaseClient:   databaseClient,
		collectionClient: collectionClient,
	}
}

// UpdateHealthEventStatus updates health event status
func (h *MongoHealthEventStore) UpdateHealthEventStatus(ctx context.Context, id string,
	status datastore.HealthEventStatus) error {
	// Update the health event status
	err := h.databaseClient.UpdateDocumentStatus(ctx, id, "healtheventstatus", status)
	if err != nil {
		return fmt.Errorf("failed to update health event status: %w", err)
	}

	return nil
}

// UpdateHealthEventStatusByNode updates health event status by node
func (h *MongoHealthEventStore) UpdateHealthEventStatusByNode(ctx context.Context, nodeName string,
	status datastore.HealthEventStatus) error {
	// Create filter for node name
	filter := map[string]interface{}{
		"healthevent.nodename": nodeName,
	}

	// Create update document
	update := map[string]interface{}{
		"$set": map[string]interface{}{
			"healtheventstatus": status,
		},
	}

	_, err := h.databaseClient.UpdateDocument(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("failed to update health event status by node: %w", err)
	}

	return nil
}

// FindHealthEventsByNode finds health events by node
func (h *MongoHealthEventStore) FindHealthEventsByNode(ctx context.Context,
	nodeName string) ([]datastore.HealthEventWithStatus, error) {
	filter := map[string]interface{}{
		"healthevent.nodename": nodeName,
	}

	cursor, err := h.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to find health events by node",
			err,
		).WithMetadata("nodeName", nodeName)
	}
	defer cursor.Close(ctx)

	var events []datastore.HealthEventWithStatus

	err = cursor.All(ctx, &events)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode health events by node",
			err,
		).WithMetadata("nodeName", nodeName)
	}

	return events, nil
}

// FindHealthEventsByFilter finds health events by filter
func (h *MongoHealthEventStore) FindHealthEventsByFilter(ctx context.Context,
	filter map[string]interface{}) ([]datastore.HealthEventWithStatus, error) {
	cursor, err := h.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to find health events by filter",
			err,
		).WithMetadata("filter", filter)
	}
	defer cursor.Close(ctx)

	var events []datastore.HealthEventWithStatus

	err = cursor.All(ctx, &events)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode health events by filter",
			err,
		).WithMetadata("filter", filter)
	}

	return events, nil
}

// FindHealthEventsByStatus finds health events by status
func (h *MongoHealthEventStore) FindHealthEventsByStatus(ctx context.Context,
	status datastore.Status) ([]datastore.HealthEventWithStatus, error) {
	// Query for any status field that matches the given status
	filter := map[string]interface{}{
		"$or": []map[string]interface{}{
			{"healtheventstatus.nodequarantined": status},
			{"healtheventstatus.userpodsevictionstatus.status": status},
		},
	}

	cursor, err := h.databaseClient.Find(ctx, filter, nil)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to find health events by status",
			err,
		).WithMetadata("status", string(status))
	}
	defer cursor.Close(ctx)

	var events []datastore.HealthEventWithStatus

	err = cursor.All(ctx, &events)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode health events by status",
			err,
		).WithMetadata("status", string(status))
	}

	return events, nil
}

// UpdateNodeQuarantineStatus updates node quarantine status
func (h *MongoHealthEventStore) UpdateNodeQuarantineStatus(ctx context.Context, eventID string,
	status datastore.Status) error {
	// Use the convenience function from our existing implementation
	return client.UpdateHealthEventNodeQuarantineStatus(ctx, h.databaseClient, eventID,
		string(status))
}

// UpdatePodEvictionStatus updates pod eviction status
func (h *MongoHealthEventStore) UpdatePodEvictionStatus(ctx context.Context, eventID string,
	status datastore.OperationStatus) error {
	// Use the convenience function from our existing implementation
	return client.UpdateHealthEventPodEvictionStatus(ctx, h.databaseClient, eventID, status)
}

// UpdateRemediationStatus updates remediation status
func (h *MongoHealthEventStore) UpdateRemediationStatus(ctx context.Context, eventID string, status interface{}) error {
	// Use the convenience function from our existing implementation
	return client.UpdateHealthEventRemediationStatus(ctx, h.databaseClient, eventID, status)
}

// CheckIfNodeAlreadyDrained checks if a node is already drained
func (h *MongoHealthEventStore) CheckIfNodeAlreadyDrained(ctx context.Context,
	nodeName string) (bool, error) {
	// Look for events where the node is successfully drained
	filter := map[string]interface{}{
		"healthevent.nodename":                            nodeName,
		"healtheventstatus.userpodsevictionstatus.status": datastore.StatusSucceeded,
	}

	count, err := h.databaseClient.CountDocuments(ctx, filter, nil)
	if err != nil {
		return false, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to check if node is already drained",
			err,
		).WithMetadata("nodeName", nodeName)
	}

	return count > 0, nil
}

// FindLatestEventForNode finds the latest event for a node
func (h *MongoHealthEventStore) FindLatestEventForNode(
	ctx context.Context,
	nodeName string,
) (*datastore.HealthEventWithStatus, error) {
	filter := map[string]interface{}{
		"healthevent.nodename": nodeName,
	}

	// Sort by creation time descending to get the latest event
	options := &client.FindOneOptions{
		Sort: map[string]interface{}{"createdAt": -1},
	}

	result, err := h.databaseClient.FindOne(ctx, filter, options)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to find latest event for node",
			err,
		).WithMetadata("nodeName", nodeName)
	}

	var event datastore.HealthEventWithStatus

	err = result.Decode(&event)
	if err != nil {
		if client.IsNoDocumentsError(err) {
			return nil, nil // No event found, return nil without error
		}

		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to decode latest event for node",
			err,
		).WithMetadata("nodeName", nodeName)
	}

	return &event, nil
}
