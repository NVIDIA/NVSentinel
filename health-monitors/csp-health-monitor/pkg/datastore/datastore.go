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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	DefaultMongoDBCollection = "MaintenanceEvents"
	maxRetries               = 3
	retryDelay               = 2 * time.Second
	defaultUnknown           = "UNKNOWN"
)

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

// MongoStore implements the Store interface using MongoDB.
type MongoStore struct {
	client         *mongo.Collection
	collectionName string
}

var _ Store = (*MongoStore)(nil)

// NewStore creates a new MongoDB store client.
func NewStore(ctx context.Context, mongoClientCertMountPath *string) (*MongoStore, error) {
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		return nil, fmt.Errorf("MONGODB_URI environment variable is not set")
	}

	mongoDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		return nil, fmt.Errorf("MONGODB_DATABASE_NAME environment variable is not set")
	}

	mongoCollection := os.Getenv("MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME")
	if mongoCollection == "" {
		slog.Warn("MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME not set, using default",
			"defaultCollection", DefaultMongoDBCollection)

		mongoCollection = DefaultMongoDBCollection
	}

	totalTimeoutSeconds, _ := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	intervalSeconds, _ := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	totalCACertTimeoutSeconds, _ := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	intervalCACertSeconds, _ := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)

	if mongoClientCertMountPath == nil || *mongoClientCertMountPath == "" {
		return nil, fmt.Errorf("mongo client certificate mount path is required")
	}

	mongoConfig := storewatcher.MongoDBConfig{
		URI:        mongoURI,
		Database:   mongoDatabase,
		Collection: mongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(*mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(*mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(*mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    totalTimeoutSeconds,
		TotalPingIntervalSeconds:   intervalSeconds,
		TotalCACertTimeoutSeconds:  totalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: intervalCACertSeconds,
	}

	slog.Info("Initializing MongoDB connection",
		"mongoURI", mongoURI,
		"database", mongoDatabase,
		"collection", mongoCollection)

	collection, err := storewatcher.GetCollectionClient(ctx, mongoConfig)
	if err != nil {
		// Consider adding a datastore connection metric error here
		return nil, fmt.Errorf("error initializing MongoDB collection client: %w", err)
	}

	slog.Info("MongoDB collection client initialized successfully.")

	// Ensure Indexes Exist
	indexModels := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "eventId", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("unique_eventid"),
		},
		{
			Keys:    bson.D{bson.E{Key: "status", Value: 1}, bson.E{Key: "scheduledStartTime", Value: 1}},
			Options: options.Index().SetName("status_scheduledstart"),
		},
		{
			Keys:    bson.D{bson.E{Key: "status", Value: 1}, bson.E{Key: "actualEndTime", Value: 1}},
			Options: options.Index().SetName("status_actualend"),
		},
		{Keys: bson.D{
			bson.E{Key: "csp", Value: 1},
			bson.E{Key: "clusterName", Value: 1},
			bson.E{Key: "eventReceivedTimestamp", Value: -1},
		}, Options: options.Index().SetName("csp_cluster_received_desc")},
		{
			Keys:    bson.D{bson.E{Key: "cspStatus", Value: 1}},
			Options: options.Index().SetName("csp_status"),
		},
	}

	indexView := collection.Indexes()

	_, indexErr := indexView.CreateMany(ctx, indexModels)
	if indexErr != nil {
		// Consider adding a datastore index creation metric error here (but maybe only warning level)
		slog.Warn("Failed to create indexes (they might already exist)", "error", indexErr)
	} else {
		slog.Info("Successfully created or ensured MongoDB indexes exist.")
	}

	return &MongoStore{
		client:         collection,
		collectionName: mongoCollection,
	}, nil
}

// getEnvAsInt parses an integer environment variable.
func getEnvAsInt(name string, defaultVal int) (int, error) {
	valueStr := os.Getenv(name)
	if valueStr == "" {
		return defaultVal, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		slog.Warn("Invalid integer value for environment variable; using default",
			"name", name,
			"value", valueStr,
			"default", defaultVal,
			"error", err)

		return defaultVal, fmt.Errorf("invalid value for %s: %s", name, valueStr)
	}

	return value, nil
}

// executeUpsert performs the UpdateOne with retries for the given merged event.
func (s *MongoStore) executeUpsert(ctx context.Context, filter bson.D, event *model.MaintenanceEvent) error {
	update := bson.M{"$set": event}
	opts := options.Update().SetUpsert(true)

	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		slog.Debug("Attempt to upsert maintenance event",
			"attempt", i,
			"eventID", event.EventID)

		result, err := s.client.UpdateOne(ctx, filter, update, opts)
		if err == nil {
			switch {
			case result.UpsertedCount > 0:
				slog.Debug("Inserted new maintenance event", "eventID", event.EventID)
			case result.ModifiedCount > 0:
				slog.Debug("Updated existing maintenance event", "eventID", event.EventID)
			default:
				slog.Debug("Matched existing maintenance event but no fields changed",
					"eventID", event.EventID)
			}

			return nil
		}

		lastErr = err
		slog.Warn("Attempt failed to upsert event; retrying",
			"attempt", i,
			"eventID", event.EventID,
			"error", err)
		time.Sleep(retryDelay)
	}

	return fmt.Errorf("upsert failed for event %s after %d retries: %w", event.EventID, maxRetries, lastErr)
}

// UpsertMaintenanceEvent inserts or updates a maintenance event.
// Metrics are handled by the caller (Processor).
func (s *MongoStore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	if event == nil || event.EventID == "" {
		return fmt.Errorf("invalid event passed to UpsertMaintenanceEvent (nil or empty EventID)")
	}

	filter := bson.D{{Key: "eventId", Value: event.EventID}}
	event.LastUpdatedTimestamp = time.Now().UTC()

	// Since Processor now prepares the event fully, we directly upsert.
	// The fetchExistingEvent and mergeEvents logic is removed based on the confidence
	// that each EventID is processed once in its final state by the Processor.
	slog.Debug("Upserting event directly as prepared by Processor", "eventID", event.EventID)

	return s.executeUpsert(ctx, filter, event)
}

// FindEventsToTriggerQuarantine finds events ready for quarantine trigger.
// Metrics (duration, errors) handled by the caller (Trigger Engine).
func (s *MongoStore) FindEventsToTriggerQuarantine(
	ctx context.Context,
	triggerTimeLimit time.Duration,
) ([]model.MaintenanceEvent, error) {
	now := time.Now().UTC()
	triggerBefore := now.Add(triggerTimeLimit)

	filter := bson.D{
		bson.E{Key: "status", Value: model.StatusDetected},
		bson.E{Key: "scheduledStartTime", Value: bson.D{
			bson.E{Key: "$gt", Value: now},
			bson.E{Key: "$lte", Value: triggerBefore},
		}},
	}

	slog.Debug("Querying for quarantine triggers",
		"status", model.StatusDetected,
		"currentTime", now.Format(time.RFC3339),
		"triggerBefore", triggerBefore.Format(time.RFC3339))

	cursor, err := s.client.Find(ctx, filter)
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
func (s *MongoStore) FindEventsToTriggerHealthy(
	ctx context.Context,
	healthyDelay time.Duration,
) ([]model.MaintenanceEvent, error) {
	now := time.Now().UTC()
	triggerIfEndedBefore := now.Add(-healthyDelay) // Event must have ended *before* or *at* this time

	filter := bson.D{
		bson.E{Key: "status", Value: model.StatusMaintenanceComplete},
		bson.E{Key: "actualEndTime", Value: bson.D{
			bson.E{Key: "$ne", Value: nil},                   // actualEndTime must exist
			bson.E{Key: "$lte", Value: triggerIfEndedBefore}, // and be sufficiently in the past
		}},
	}

	slog.Debug("Querying for healthy triggers",
		"status", model.StatusMaintenanceComplete,
		"actualEndTimeBefore", triggerIfEndedBefore.Format(time.RFC3339))

	cursor, err := s.client.Find(ctx, filter)
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
func (s *MongoStore) UpdateEventStatus(ctx context.Context, eventID string, newStatus model.InternalStatus) error {
	if eventID == "" {
		return fmt.Errorf("cannot update status for empty eventID")
	}

	filter := bson.D{bson.E{Key: "eventId", Value: eventID}}
	update := bson.D{
		bson.E{Key: "$set", Value: bson.D{
			bson.E{Key: "status", Value: newStatus},
			bson.E{Key: "lastUpdatedTimestamp", Value: time.Now().UTC()},
		}},
	}

	var err error

	var result *mongo.UpdateResult

	for i := 1; i <= maxRetries; i++ {
		slog.Debug("Attempt to update status for event",
			"attempt", i,
			"newStatus", newStatus,
			"eventID", eventID)

		result, err = s.client.UpdateOne(ctx, filter, update)
		if err == nil {
			if result.MatchedCount == 0 {
				slog.Warn("Attempted to update status for non-existent event", "eventID", eventID)
				return nil // Not an error if event is gone
			}

			slog.Debug("Successfully updated status for event",
				"newStatus", newStatus,
				"eventID", eventID)

			return nil // Success
		}

		slog.Warn("Attempt failed to update status for event; retrying",
			"attempt", i,
			"eventID", eventID,
			"error", err,
			"retryDelay", retryDelay)
		time.Sleep(retryDelay)
	}

	return fmt.Errorf(
		"failed to update status for event (EventID: %s) after %d retries: %w",
		eventID, maxRetries, err)
}

// GetLastProcessedEventTimestampByCSP is a helper to get the latest event timestamp for a given CSP.
func (s *MongoStore) GetLastProcessedEventTimestampByCSP(
	ctx context.Context,
	clusterName string,
	cspType model.CSP,
	cspNameForLog string,
) (timestamp time.Time, found bool, err error) {
	filter := bson.D{bson.E{Key: "csp", Value: cspType}}
	if clusterName != "" {
		filter = append(filter, bson.E{Key: "clusterName", Value: clusterName})
	}

	findOptions := options.FindOne().
		SetSort(bson.D{bson.E{Key: "eventReceivedTimestamp", Value: -1}})
		// Sort by internal received time

	slog.Debug("Querying for last processed timestamp",
		"csp", cspNameForLog)

	var latestEvent model.MaintenanceEvent
	dbErr := s.client.FindOne(ctx, filter, findOptions).Decode(&latestEvent)

	if dbErr != nil {
		if errors.Is(dbErr, mongo.ErrNoDocuments) {
			slog.Debug("No previous event timestamp found in datastore",
				"csp", cspNameForLog,
				"cluster", clusterName)

			return time.Time{}, false, nil
		}

		slog.Error("Failed to query last processed log timestamp",
			"csp", cspNameForLog,
			"cluster", clusterName,
			"error", dbErr)

		return time.Time{}, false, fmt.Errorf("failed to query last %s log timestamp: %w", cspNameForLog, dbErr)
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
func (s *MongoStore) FindLatestActiveEventByNodeAndType(
	ctx context.Context,
	nodeName string,
	maintenanceType model.MaintenanceType,
	statuses []model.InternalStatus,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" || maintenanceType == "" || len(statuses) == 0 {
		return nil, false, fmt.Errorf("nodeName, maintenanceType, and at least one status are required")
	}

	filter := bson.D{
		bson.E{Key: "nodeName", Value: nodeName},
		bson.E{Key: "maintenanceType", Value: maintenanceType},
		bson.E{Key: "status", Value: bson.D{bson.E{Key: "$in", Value: statuses}}},
	}

	// Sort by LastUpdatedTimestamp descending to get the latest one.
	// If multiple have the exact same LastUpdatedTimestamp, this will pick one arbitrarily among them.
	// Consider adding a secondary sort key if more deterministic behavior is needed in such rare cases.
	findOptions := options.FindOne().SetSort(bson.D{bson.E{Key: "lastUpdatedTimestamp", Value: -1}})

	slog.Debug("Querying for latest active event",
		"node", nodeName,
		"maintenanceType", maintenanceType,
		"sort", "lastUpdatedTimestamp: -1")

	var latestEvent model.MaintenanceEvent

	err := s.client.FindOne(ctx, filter, findOptions).Decode(&latestEvent)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			slog.Info("No active event found", "node", nodeName, "type", maintenanceType, "statuses", statuses)
			return nil, false, nil
		}

		slog.Error("Failed to query latest active event",
			"node", nodeName,
			"type", maintenanceType,
			"statuses", statuses,
			"error", err)

		return nil, false, fmt.Errorf("failed to query latest active event: %w", err)
	}

	slog.Info("Found latest active event",
		"eventID", latestEvent.EventID,
		"node", nodeName,
		"type", maintenanceType,
		"statuses", statuses)

	return &latestEvent, true, nil
}

// FindLatestOngoingEventByNode finds the most recently updated ONGOING event for a given node.
func (s *MongoStore) FindLatestOngoingEventByNode(
	ctx context.Context,
	nodeName string,
) (*model.MaintenanceEvent, bool, error) {
	if nodeName == "" {
		return nil, false, fmt.Errorf("nodeName is required")
	}

	filter := bson.D{{Key: "nodeName", Value: nodeName}, {Key: "status", Value: model.StatusMaintenanceOngoing}}
	opts := options.FindOne().SetSort(bson.D{{Key: "lastUpdatedTimestamp", Value: -1}})

	var event model.MaintenanceEvent

	err := s.client.FindOne(ctx, filter, opts).Decode(&event)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			slog.Debug("No ongoing event found for node", "node", nodeName)

			return nil, false, nil
		}

		return nil, false, fmt.Errorf("query latest ongoing event for node %s: %w", nodeName, err)
	}

	slog.Debug("Found ongoing event", "eventID", event.EventID, "node", nodeName)

	return &event, true, nil
}

// FindActiveEventsByStatuses finds active events by their csp status.
func (s *MongoStore) FindActiveEventsByStatuses(
	ctx context.Context,
	csp model.CSP,
	statuses []string,
) ([]model.MaintenanceEvent, error) {
	if len(statuses) == 0 {
		return nil, fmt.Errorf("at least one status is required")
	}

	filter := bson.D{
		bson.E{Key: "csp", Value: csp},
		bson.E{Key: "cspStatus", Value: bson.D{bson.E{Key: "$in", Value: statuses}}},
	}

	slog.Debug("Querying for active events",
		"csp", csp,
		"statuses", statuses)

	cursor, err := s.client.Find(ctx, filter)
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
