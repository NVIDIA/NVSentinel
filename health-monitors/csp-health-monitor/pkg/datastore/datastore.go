// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	klog "k8s.io/klog/v2"
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
		klog.Warningf("MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME not set, using default: %s", DefaultMongoDBCollection)
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

	klog.Infof(
		"Initializing MongoDB connection to %s, Database: %s, Collection: %s",
		mongoURI, mongoDatabase, mongoCollection,
	)

	collection, err := storewatcher.GetCollectionClient(ctx, mongoConfig)
	if err != nil {
		// Consider adding a datastore connection metric error here
		return nil, fmt.Errorf("error initializing MongoDB collection client: %w", err)
	}

	klog.Infof("MongoDB collection client initialized successfully.")

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
		klog.Warningf("Failed to create indexes (they might already exist): %v", indexErr)
	} else {
		klog.Info("Successfully created or ensured MongoDB indexes exist.")
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
		klog.Warningf(
			"Invalid integer value for environment variable %s: '%s'. Using default %d. Error: %v",
			name, valueStr, defaultVal, err,
		)

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
		klog.V(3).Infof("Attempt %d to upsert maintenance event (EventID: %s)", i, event.EventID)

		result, err := s.client.UpdateOne(ctx, filter, update, opts)
		if err == nil {
			switch {
			case result.UpsertedCount > 0:
				klog.V(2).Infof("Inserted new maintenance event (EventID: %s)", event.EventID)
			case result.ModifiedCount > 0:
				klog.V(2).Infof("Updated existing maintenance event (EventID: %s)", event.EventID)
			default:
				klog.V(2).Infof(
					"Matched existing maintenance event but no fields changed (EventID: %s)",
					event.EventID,
				)
			}

			return nil
		}

		lastErr = err
		klog.Warningf("Attempt %d failed to upsert event (EventID: %s): %v; retrying...", i, event.EventID, err)
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
	klog.V(3).Infof("Upserting event %s directly as prepared by Processor.", event.EventID)

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

	klog.V(2).Infof(
		"Querying for quarantine triggers: status=%s, scheduledStartTime=(%v, %v]",
		model.StatusDetected, now.Format(time.RFC3339), triggerBefore.Format(time.RFC3339),
	)

	cursor, err := s.client.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for quarantine trigger: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for quarantine trigger: %w", err)
	}

	klog.V(2).Infof("Found %d events potentially ready for quarantine trigger.", len(results))

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

	klog.V(2).Infof(
		"Querying for healthy triggers: status=%s, actualEndTime != nil AND actualEndTime <= %v",
		model.StatusMaintenanceComplete, triggerIfEndedBefore.Format(time.RFC3339),
	)

	cursor, err := s.client.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for healthy trigger: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for healthy trigger: %w", err)
	}

	klog.V(2).Infof("Found %d events potentially ready for healthy trigger.", len(results))

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
		klog.V(3).Infof("Attempt %d to update status to '%s' for event (EventID: %s)",
			i, newStatus, eventID)

		result, err = s.client.UpdateOne(ctx, filter, update)
		if err == nil {
			if result.MatchedCount == 0 {
				klog.Warningf("Attempted to update status for non-existent event (EventID: %s)", eventID)
				return nil // Not an error if event is gone
			}

			klog.V(2).Infof("Successfully updated status to '%s' for event (EventID: %s)",
				newStatus, eventID)

			return nil // Success
		}

		klog.Warningf(
			"Attempt %d failed to update status for event (EventID: %s): %v. Retrying in %v...",
			i, eventID, err, retryDelay)
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

	klog.V(2).Infof("Querying for last processed %s timestamp with filter: %v", cspNameForLog, filter)

	var latestEvent model.MaintenanceEvent
	dbErr := s.client.FindOne(ctx, filter, findOptions).Decode(&latestEvent)

	if dbErr != nil {
		if errors.Is(dbErr, mongo.ErrNoDocuments) {
			klog.V(1).Infof("No previous %s event timestamp found in datastore (Cluster: %s)",
				cspNameForLog, clusterName)
			return time.Time{}, false, nil
		}

		klog.Errorf("Failed to query last processed %s log timestamp for cluster %s: %v",
			cspNameForLog, clusterName, dbErr)

		return time.Time{}, false, fmt.Errorf("failed to query last %s log timestamp: %w", cspNameForLog, dbErr)
	}

	// Use EventReceivedTimestamp as the marker for when we processed it
	klog.V(1).Infof(
		"Found last processed %s log timestamp for cluster %s: %v (EventID: %s)",
		cspNameForLog, clusterName, latestEvent.EventReceivedTimestamp, latestEvent.EventID)

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

	klog.V(3).Infof("Querying for latest active event with filter: %v, sort: {lastUpdatedTimestamp: -1}", filter)

	var latestEvent model.MaintenanceEvent

	err := s.client.FindOne(ctx, filter, findOptions).Decode(&latestEvent)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			klog.V(2).
				Infof("No active event found for Node: %s, Type: %s, Statuses: %v", nodeName, maintenanceType, statuses)
			return nil, false, nil
		}

		klog.Errorf(
			"Failed to query latest active event for Node: %s, Type: %s, Statuses: %v: %v",
			nodeName,
			maintenanceType,
			statuses,
			err,
		)

		return nil, false, fmt.Errorf("failed to query latest active event: %w", err)
	}

	klog.V(2).
		Infof("Found latest active event %s for Node: %s, "+
			"Type: %s, Statuses: %v", latestEvent.EventID, nodeName, maintenanceType, statuses)

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
			klog.V(2).Infof("No ongoing event found for node %s", nodeName)

			return nil, false, nil
		}

		return nil, false, fmt.Errorf("query latest ongoing event for node %s: %w", nodeName, err)
	}

	klog.V(2).Infof("Found ongoing event %s for node %s", event.EventID, nodeName)

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

	klog.V(2).Infof("Querying for active events with filter: %v", filter)

	cursor, err := s.client.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query active events: %w", err)
	}

	defer cursor.Close(ctx)

	var results []model.MaintenanceEvent
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode maintenance events for active events: %w", err)
	}

	klog.V(2).Infof("Found %d active events.", len(results))

	return results, nil
}
