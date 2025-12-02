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

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

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

	// Normalize HealthEvent fields from bson.M to map[string]interface{}
	normalizeHealthEvents(events)

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

	// Iterate manually to populate both the struct and RawEvent with _id
	for cursor.Next(ctx) {
		// First decode into a raw map to preserve _id
		var rawDoc map[string]interface{}
		if err := cursor.Decode(&rawDoc); err != nil {
			return nil, datastore.NewQueryError(
				datastore.ProviderMongoDB,
				"failed to decode raw document",
				err,
			).WithMetadata("filter", filter)
		}

		// Then use bson to convert the raw map into our struct
		var event datastore.HealthEventWithStatus

		bsonBytes, err := bson.Marshal(rawDoc)
		if err != nil {
			return nil, datastore.NewQueryError(
				datastore.ProviderMongoDB,
				"failed to marshal raw document to BSON",
				err,
			).WithMetadata("filter", filter)
		}

		if err := bson.Unmarshal(bsonBytes, &event); err != nil {
			return nil, datastore.NewQueryError(
				datastore.ProviderMongoDB,
				"failed to unmarshal BSON to health event",
				err,
			).WithMetadata("filter", filter)
		}

		// Store the raw document (with _id) in RawEvent
		event.RawEvent = rawDoc

		events = append(events, event)
	}

	if err := cursor.Err(); err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"cursor error while iterating health events",
			err,
		).WithMetadata("filter", filter)
	}

	// Normalize HealthEvent fields from bson.M to map[string]interface{}
	normalizeHealthEvents(events)

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

	// Normalize HealthEvent fields from bson.M to map[string]interface{}
	normalizeHealthEvents(events)

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

// FindHealthEventsByQuery finds health events using query builder
// MongoDB: converts builder to map and uses existing FindHealthEventsByFilter
func (h *MongoHealthEventStore) FindHealthEventsByQuery(ctx context.Context,
	builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	// Convert query builder to MongoDB filter
	filter := builder.ToMongo()

	// Use existing implementation
	return h.FindHealthEventsByFilter(ctx, filter)
}

// UpdateHealthEventsByQuery updates health events using query builder
// MongoDB: converts builders to maps and uses existing UpdateMany
func (h *MongoHealthEventStore) UpdateHealthEventsByQuery(ctx context.Context,
	queryBuilder datastore.QueryBuilder, updateBuilder datastore.UpdateBuilder) error {
	// Convert query builder to MongoDB filter
	filter := queryBuilder.ToMongo()

	// Convert update builder to MongoDB update document
	update := updateBuilder.ToMongo()

	// Use MongoDB UpdateManyDocuments
	_, err := h.databaseClient.UpdateManyDocuments(ctx, filter, update)
	if err != nil {
		return datastore.NewUpdateError(
			datastore.ProviderMongoDB,
			"failed to update health events by query",
			err,
		)
	}

	return nil
}

// normalizeHealthEvents converts bson.M types to map[string]interface{} in HealthEvent fields
// This ensures consistency across database providers (MongoDB-specific types -> generic maps)
func normalizeHealthEvents(events []datastore.HealthEventWithStatus) {
	for i := range events {
		if events[i].HealthEvent != nil {
			events[i].HealthEvent = normalizeValue(events[i].HealthEvent)
		}
	}
}

// normalizeValue recursively converts MongoDB types (bson.M, primitive.D, primitive.A, etc.) to standard Go types
func normalizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case primitive.D:
		return normalizePrimitiveD(val)
	case primitive.A:
		return normalizeArray(val)
	case map[string]interface{}:
		return normalizeMap(val)
	case []interface{}:
		return normalizeArray(val)
	default:
		// Primitive types, return as-is
		return val
	}
}

// normalizePrimitiveD converts primitive.D to map[string]interface{} and normalizes nested values
func normalizePrimitiveD(val primitive.D) interface{} {
	bsonBytes, err := bson.Marshal(val)
	if err != nil {
		return val // Return as-is if marshal fails
	}

	var m map[string]interface{}
	if err := bson.Unmarshal(bsonBytes, &m); err != nil {
		return val // Return as-is if unmarshal fails
	}

	return normalizeMap(m)
}

// normalizeMap recursively normalizes all values in a map
func normalizeMap(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = normalizeValue(v)
	}

	return result
}

// normalizeArray recursively normalizes all elements in an array
func normalizeArray(arr interface{}) []interface{} {
	var length int

	var getValue func(int) interface{}

	switch v := arr.(type) {
	case primitive.A:
		length = len(v)
		getValue = func(i int) interface{} { return v[i] }
	case []interface{}:
		length = len(v)
		getValue = func(i int) interface{} { return v[i] }
	default:
		return nil
	}

	result := make([]interface{}, length)
	for i := 0; i < length; i++ {
		result[i] = normalizeValue(getValue(i))
	}

	return result
}
