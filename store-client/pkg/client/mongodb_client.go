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

package client

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	mongoWatcher "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb/watcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoEvent implements the Event interface for MongoDB
type mongoEvent struct {
	rawEvent mongoWatcher.Event
}

func (e *mongoEvent) GetDocumentID() (string, error) {
	fullDocument, ok := e.rawEvent["fullDocument"]
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting fullDocument from event",
			nil,
		).WithMetadata("eventKeys", getMapKeys(e.rawEvent))
	}

	// Convert to bson.M for internal processing - handle both primitive.M and map[string]interface{}
	var document bson.M

	switch v := fullDocument.(type) {
	case primitive.M:
		document = bson.M(v)
	case map[string]interface{}:
		document = bson.M(v)
	default:
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"fullDocument has unsupported type",
			nil,
		).WithMetadata("type", fmt.Sprintf("%T", fullDocument))
	}

	objectID, ok := document["_id"].(primitive.ObjectID)
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting _id from document",
			nil,
		).WithMetadata("documentKeys", getMapKeys(document))
	}

	return objectID.Hex(), nil
}

// GetRecordUUID returns the record UUID.
// For MongoDB, this is the same as GetDocumentID since MongoDB's _id is the primary identifier.
func (e *mongoEvent) GetRecordUUID() (string, error) {
	return e.GetDocumentID()
}

func (e *mongoEvent) GetNodeName() (string, error) {
	fullDocument, ok := e.rawEvent["fullDocument"]
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting fullDocument from event",
			nil,
		).WithMetadata("eventKeys", getMapKeys(e.rawEvent))
	}

	// Convert to bson.M for internal processing - handle both primitive.M and map[string]interface{}
	var document bson.M

	switch v := fullDocument.(type) {
	case primitive.M:
		document = bson.M(v)
	case map[string]interface{}:
		document = bson.M(v)
	default:
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"fullDocument has unsupported type",
			nil,
		).WithMetadata("type", fmt.Sprintf("%T", fullDocument))
	}

	healthEvent, ok := document["healthevent"].(bson.M)
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting healthevent from document",
			nil,
		).WithMetadata("documentKeys", getMapKeys(document))
	}

	nodeName, ok := healthEvent["nodename"].(string)
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting nodename from healthevent",
			nil,
		).WithMetadata("healthEventKeys", getMapKeys(healthEvent))
	}

	return nodeName, nil
}

func (e *mongoEvent) GetResumeToken() []byte {
	// Extract the resume token that was captured when the event was received
	// This token is added by the ChangeStreamWatcher in processChangeStreamEvent
	token, ok := e.rawEvent["_resumeToken"]
	if !ok {
		// If no token was found, return empty (caller will fall back to cursor position)
		return []byte{}
	}

	// MongoDB resume tokens can be various types (Binary, string, etc.)
	// Try to extract as byte array
	switch v := token.(type) {
	case []byte:
		return v
	case primitive.Binary:
		return v.Data
	case bson.RawValue:
		data, err := bson.Marshal(v)
		if err == nil {
			return data
		}
	default:
		// Try to marshal whatever type it is
		data, err := bson.Marshal(v)
		if err == nil {
			return data
		}
	}

	return []byte{}
}

func (e *mongoEvent) UnmarshalDocument(v interface{}) error {
	// Extract fullDocument from the MongoDB change event
	fullDocument, ok := e.rawEvent["fullDocument"]
	if !ok {
		return datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting fullDocument from event",
			nil,
		).WithMetadata("eventKeys", getMapKeys(e.rawEvent))
	}

	// Convert to bson.M for internal processing - handle both primitive.M and map[string]interface{}
	var document bson.M

	switch v := fullDocument.(type) {
	case primitive.M:
		document = bson.M(v)
	case map[string]interface{}:
		document = bson.M(v)
	default:
		return datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"fullDocument has unsupported type",
			nil,
		).WithMetadata("type", fmt.Sprintf("%T", fullDocument))
	}

	// Marshal and unmarshal through BSON to convert to the target type
	bsonBytes, err := bson.Marshal(document)
	if err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderMongoDB,
			"error marshaling BSON for event",
			err,
		)
	}

	if err := bson.Unmarshal(bsonBytes, v); err != nil {
		return datastore.NewSerializationError(
			datastore.ProviderMongoDB,
			fmt.Sprintf("error unmarshaling BSON into type %T", v),
			err,
		)
	}

	// Normalize MongoDB-specific types to standard Go types AFTER unmarshaling
	// This is necessary because bson.Unmarshal into map[string]interface{} preserves MongoDB types
	if targetMap, ok := v.(*map[string]interface{}); ok && targetMap != nil {
		normalizeMongoTypes(*targetMap)
	}

	return nil
}

// Legacy pipeline builders for backward compatibility
// Deprecated: Use BuildInsertOnlyPipelineAgnostic() instead for database-agnostic support

// BuildInsertOnlyPipeline creates a MongoDB-specific pipeline for insert operations
//
// Deprecated: Use BuildInsertOnlyPipelineAgnostic() instead
func BuildInsertOnlyPipeline() interface{} {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert"}}}},
		}}},
	}
}

// BuildQuarantineUpdatePipeline creates a MongoDB-specific pipeline for quarantine updates
//
// Deprecated: Use BuildQuarantineUpdatePipelineAgnostic() instead
func BuildQuarantineUpdatePipeline() interface{} {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: "update"},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "updateDescription.updatedFields",
					Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: model.Quarantined}}}},
				bson.D{{Key: "updateDescription.updatedFields",
					Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: model.AlreadyQuarantined}}}},
				bson.D{{Key: "updateDescription.updatedFields",
					Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: model.UnQuarantined}}}},
			}},
		}}},
	}
}

// BuildNonFatalInsertPipeline creates a MongoDB-specific pipeline for non-fatal insert operations
//
// Deprecated: Use BuildNonFatalInsertPipelineAgnostic() instead
func BuildNonFatalInsertPipeline() interface{} {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: "insert"},
			{Key: "fullDocument.healthevent.isfatal", Value: false},
			{Key: "fullDocument.healthevent.ishealthy", Value: false},
		}}},
	}
}

// New database-agnostic pipeline builders using the new types

// BuildAllHealthEventInsertsPipeline creates a pipeline that watches for all health event inserts
// This is used by fault-quarantine to detect all new health events without filtering
//
// Deprecated: Use GetPipelineBuilder().BuildAllHealthEventInsertsPipeline() instead.
// This function is maintained for backward compatibility and will be removed in a future version.
func BuildAllHealthEventInsertsPipeline() datastore.Pipeline {
	builder := GetPipelineBuilder()
	return builder.BuildAllHealthEventInsertsPipeline()
}

// BuildNodeQuarantineStatusUpdatesPipeline creates a pipeline that watches for node quarantine status changes
// This is used by node-drainer to detect when nodes are quarantined/unquarantined and need draining
//
// Deprecated: Use GetPipelineBuilder().BuildNodeQuarantineStatusPipeline() instead.
// This function is maintained for backward compatibility and will be removed in a future version.
// The new PipelineBuilder interface provides database-specific optimizations for MongoDB and PostgreSQL.
func BuildNodeQuarantineStatusUpdatesPipeline() datastore.Pipeline {
	// For backward compatibility, use the PipelineBuilder interface
	// This ensures the correct pipeline is selected based on the database provider
	builder := GetPipelineBuilder()
	return builder.BuildNodeQuarantineStatusPipeline()
}

// BuildNonFatalUnhealthyInsertsPipeline creates a pipeline that watches for non-fatal, unhealthy event inserts
// This is used by health-events-analyzer to detect warning-level health events for pattern analysis
//
// Deprecated: Use GetPipelineBuilder().BuildNonFatalUnhealthyInsertsPipeline() instead.
// This function is maintained for backward compatibility and will be removed in a future version.
func BuildNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	builder := GetPipelineBuilder()
	return builder.BuildNonFatalUnhealthyInsertsPipeline()
}

// BuildQuarantinedAndDrainedNodesPipeline creates a pipeline that watches for nodes ready for remediation
// This watches for updates where both quarantine and eviction status indicate the node is ready for reboot,
// or where the node has been unquarantined and needs cleanup, or where quarantine was cancelled
//
// Deprecated: Use GetPipelineBuilder().BuildQuarantinedAndDrainedNodesPipeline() instead.
// This function is maintained for backward compatibility and will be removed in a future version.
func BuildQuarantinedAndDrainedNodesPipeline() datastore.Pipeline {
	builder := GetPipelineBuilder()
	return builder.BuildQuarantinedAndDrainedNodesPipeline()
}

// MongoDBClient implements DatabaseClient for MongoDB
type MongoDBClient struct {
	client     *mongo.Client
	database   string
	collection string
	mongoCol   *mongo.Collection
	config     config.DatabaseConfig
}

// MongoDBCollectionClient implements CollectionClient for MongoDB
type MongoDBCollectionClient struct {
	*MongoDBClient
}

// NewMongoDBClient creates a new MongoDB client from database configuration
func NewMongoDBClient(ctx context.Context, dbConfig config.DatabaseConfig) (*MongoDBClient, error) {
	// Convert to the existing MongoDBConfig format for backward compatibility
	mongoConfig := mongoWatcher.MongoDBConfig{
		URI:        dbConfig.GetConnectionURI(),
		Database:   dbConfig.GetDatabaseName(),
		Collection: dbConfig.GetCollectionName(),
		ClientTLSCertConfig: mongoWatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: dbConfig.GetCertConfig().GetCertPath(),
			TlsKeyPath:  dbConfig.GetCertConfig().GetKeyPath(),
			CaCertPath:  dbConfig.GetCertConfig().GetCACertPath(),
		},
		TotalPingTimeoutSeconds:          dbConfig.GetTimeoutConfig().GetPingTimeoutSeconds(),
		TotalPingIntervalSeconds:         dbConfig.GetTimeoutConfig().GetPingIntervalSeconds(),
		TotalCACertTimeoutSeconds:        dbConfig.GetTimeoutConfig().GetCACertTimeoutSeconds(),
		TotalCACertIntervalSeconds:       dbConfig.GetTimeoutConfig().GetCACertIntervalSeconds(),
		ChangeStreamRetryDeadlineSeconds: dbConfig.GetTimeoutConfig().GetChangeStreamRetryDeadlineSeconds(),
		ChangeStreamRetryIntervalSeconds: dbConfig.GetTimeoutConfig().GetChangeStreamRetryIntervalSeconds(),
	}

	// Use the existing GetCollectionClient function for consistency
	mongoCol, err := mongoWatcher.GetCollectionClient(ctx, mongoConfig)
	if err != nil {
		return nil, datastore.NewConnectionError(
			datastore.ProviderMongoDB,
			"failed to create MongoDB collection client",
			err,
		)
	}

	// Extract the client from the collection
	client := mongoCol.Database().Client()

	return &MongoDBClient{
		client:     client,
		database:   dbConfig.GetDatabaseName(),
		collection: dbConfig.GetCollectionName(),
		mongoCol:   mongoCol,
		config:     dbConfig,
	}, nil
}

// NewMongoDBCollectionClient creates a collection-specific client
func NewMongoDBCollectionClient(ctx context.Context, dbConfig config.DatabaseConfig) (*MongoDBCollectionClient, error) {
	mongoClient, err := NewMongoDBClient(ctx, dbConfig)
	if err != nil {
		return nil, err
	}

	return &MongoDBCollectionClient{
		MongoDBClient: mongoClient,
	}, nil
}

// UpdateDocumentStatus updates a specific status field in a document
// This consolidates the common pattern used across all modules
func (c *MongoDBClient) UpdateDocumentStatus(
	ctx context.Context, documentID string, statusPath string, status interface{},
) error {
	objectID, err := primitive.ObjectIDFromHex(documentID)
	if err != nil {
		return datastore.NewValidationError(
			datastore.ProviderMongoDB,
			fmt.Sprintf("invalid document ID %s", documentID),
			err,
		).WithMetadata("documentID", documentID)
	}

	filter := bson.M{"_id": objectID}

	// Determine if this is a nested field path (e.g., "healtheventstatus.nodequarantined")
	// or a base path with a struct (e.g., "healtheventstatus" with HealthEventStatus)
	var updateFields bson.M
	if c.isNestedFieldPath(statusPath) {
		// Direct field update for convenience functions (e.g., UpdateHealthEventNodeQuarantineStatus)
		updateFields = c.buildDirectFieldUpdate(statusPath, status)
	} else {
		// Granular struct update for store methods (e.g., UpdateHealthEventStatus)
		updateFields = c.buildStructFieldUpdates(statusPath, status)
	}

	update := bson.M{"$set": updateFields}

	_, err = c.mongoCol.UpdateOne(ctx, filter, update)
	if err != nil {
		return datastore.NewUpdateError(
			datastore.ProviderMongoDB,
			fmt.Sprintf("failed to update document %s status at path %s", documentID, statusPath),
			err,
		).WithMetadata("documentID", documentID).
			WithMetadata("statusPath", statusPath)
	}

	return nil
}

// isNestedFieldPath determines if the status path is a nested field path (contains ".")
// Used to distinguish between direct field updates and struct-based updates
func (c *MongoDBClient) isNestedFieldPath(statusPath string) bool {
	return strings.Contains(statusPath, ".")
}

// buildDirectFieldUpdate creates a direct field update for a specific nested path
// Used by convenience functions like UpdateHealthEventNodeQuarantineStatus
// Example: statusPath="healtheventstatus.nodequarantined", status="Quarantined"
//
//	→ {"healtheventstatus.nodequarantined": "Quarantined"}
func (c *MongoDBClient) buildDirectFieldUpdate(statusPath string, status interface{}) bson.M {
	return bson.M{statusPath: status}
}

// buildStructFieldUpdates extracts individual fields from a status struct for granular updates
// This prevents overwriting other fields when doing partial status updates
// Used by store methods like UpdateHealthEventStatus
// Example: statusPath="healtheventstatus", status=HealthEventStatus{FaultRemediated: true}
//
//	→ {"healtheventstatus.faultremediated": true}
func (c *MongoDBClient) buildStructFieldUpdates(basePath string, status interface{}) bson.M {
	updateFields := bson.M{}

	// Try to extract fields from HealthEventStatus struct
	healthStatus, ok := status.(datastore.HealthEventStatus)
	if !ok {
		// Type assertion failed - this could be a different status type (e.g., maintenance events)
		// Fall back to updating the whole object for backward compatibility
		updateFields[basePath] = status

		return updateFields
	}

	// Only update fields that are explicitly set (non-nil for pointers, non-zero for values)
	if healthStatus.NodeQuarantined != nil {
		updateFields[basePath+".nodequarantined"] = string(*healthStatus.NodeQuarantined)
	}

	// For UserPodsEvictionStatus, we need to update nested fields
	if healthStatus.UserPodsEvictionStatus.Status != "" {
		updateFields[basePath+".userpodsevictionstatus.status"] = string(healthStatus.UserPodsEvictionStatus.Status)

		if healthStatus.UserPodsEvictionStatus.Message != "" {
			updateFields[basePath+".userpodsevictionstatus.message"] = healthStatus.UserPodsEvictionStatus.Message
		}

		if len(healthStatus.UserPodsEvictionStatus.Metadata) > 0 {
			updateFields[basePath+".userpodsevictionstatus.metadata"] = healthStatus.UserPodsEvictionStatus.Metadata
		}
	}

	if healthStatus.FaultRemediated != nil {
		updateFields[basePath+".faultremediated"] = *healthStatus.FaultRemediated
	}

	if healthStatus.LastRemediationTimestamp != nil {
		updateFields[basePath+".lastremediationtimestamp"] = *healthStatus.LastRemediationTimestamp
	}

	return updateFields
}

// UpdateDocument performs a general update operation
func (c *MongoDBClient) UpdateDocument(
	ctx context.Context, filter interface{}, update interface{},
) (*UpdateResult, error) {
	result, err := c.mongoCol.UpdateOne(ctx, filter, update)
	if err != nil {
		return nil, datastore.NewUpdateError(
			datastore.ProviderMongoDB,
			"failed to update document",
			err,
		).WithMetadata("filter", filter).WithMetadata("update", update)
	}

	return &UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: result.UpsertedCount,
		UpsertedID:    result.UpsertedID,
	}, nil
}

// InsertMany inserts multiple documents
func (c *MongoDBClient) InsertMany(ctx context.Context, documents []interface{}) (*InsertManyResult, error) {
	result, err := c.mongoCol.InsertMany(ctx, documents)
	if err != nil {
		return nil, datastore.NewInsertError(
			datastore.ProviderMongoDB,
			"failed to insert documents",
			err,
		).WithMetadata("count", len(documents))
	}

	return &InsertManyResult{
		InsertedIDs: result.InsertedIDs,
	}, nil
}

// UpdateManyDocuments performs a general update operation on multiple documents
func (c *MongoDBClient) UpdateManyDocuments(
	ctx context.Context, filter interface{}, update interface{},
) (*UpdateResult, error) {
	result, err := c.mongoCol.UpdateMany(ctx, filter, update)
	if err != nil {
		return nil, datastore.NewUpdateError(
			datastore.ProviderMongoDB,
			"failed to update documents",
			err,
		).WithMetadata("filter", filter).WithMetadata("update", update)
	}

	return &UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: result.UpsertedCount,
		UpsertedID:    result.UpsertedID,
	}, nil
}

// UpsertDocument performs an upsert operation
func (c *MongoDBClient) UpsertDocument(
	ctx context.Context, filter interface{}, document interface{},
) (*UpdateResult, error) {
	opts := options.Update().SetUpsert(true)
	update := bson.M{"$set": document}

	result, err := c.mongoCol.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return nil, datastore.NewInsertError(
			datastore.ProviderMongoDB,
			"failed to upsert document",
			err,
		).WithMetadata("filter", filter).WithMetadata("document", document)
	}

	return &UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: result.UpsertedCount,
		UpsertedID:    result.UpsertedID,
	}, nil
}

// FindOne finds a single document
func (c *MongoDBClient) FindOne(ctx context.Context, filter interface{}, opts *FindOneOptions) (SingleResult, error) {
	mongoOpts := options.FindOne()

	if opts != nil {
		if opts.Sort != nil {
			mongoOpts.SetSort(opts.Sort)
		}

		if opts.Skip != nil {
			mongoOpts.SetSkip(*opts.Skip)
		}
	}

	result := c.mongoCol.FindOne(ctx, filter, mongoOpts)

	return &mongoSingleResult{result: result}, nil
}

// Find finds multiple documents
func (c *MongoDBClient) Find(ctx context.Context, filter interface{}, opts *FindOptions) (Cursor, error) {
	mongoOpts := options.Find()

	if opts != nil {
		if opts.Sort != nil {
			mongoOpts.SetSort(opts.Sort)
		}

		if opts.Limit != nil {
			mongoOpts.SetLimit(*opts.Limit)
		}

		if opts.Skip != nil {
			mongoOpts.SetSkip(*opts.Skip)
		}
	}

	cursor, err := c.mongoCol.Find(ctx, filter, mongoOpts)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to execute find query",
			err,
		).WithMetadata("filter", filter)
	}

	return &mongoCursor{cursor: cursor}, nil
}

// CountDocuments counts documents matching the filter
func (c *MongoDBClient) CountDocuments(ctx context.Context, filter interface{}, opts *CountOptions) (int64, error) {
	mongoOpts := options.Count()

	if opts != nil {
		if opts.Limit != nil {
			mongoOpts.SetLimit(*opts.Limit)
		}

		if opts.Skip != nil {
			mongoOpts.SetSkip(*opts.Skip)
		}
	}

	count, err := c.mongoCol.CountDocuments(ctx, filter, mongoOpts)
	if err != nil {
		return 0, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to count documents",
			err,
		).WithMetadata("filter", filter)
	}

	return count, nil
}

// Aggregate performs an aggregation query
func (c *MongoDBClient) Aggregate(ctx context.Context, pipeline interface{}) (Cursor, error) {
	// Convert datastore.Pipeline to mongo.Pipeline if needed
	var mongoPipeline interface{}

	switch p := pipeline.(type) {
	case datastore.Pipeline:
		convertedPipeline, err := ConvertAgnosticPipelineToMongo(p)
		if err != nil {
			return nil, datastore.NewQueryError(
				datastore.ProviderMongoDB,
				"failed to convert pipeline",
				err,
			).WithMetadata("pipeline", pipeline)
		}

		mongoPipeline = convertedPipeline
	default:
		// Use as-is for backward compatibility (mongo.Pipeline, etc.)
		mongoPipeline = pipeline
	}

	cursor, err := c.mongoCol.Aggregate(ctx, mongoPipeline)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to execute aggregation",
			err,
		).WithMetadata("pipeline", pipeline)
	}

	return &mongoCursor{cursor: cursor}, nil
}

// Ping checks database connectivity
func (c *MongoDBClient) Ping(ctx context.Context) error {
	var result bson.M

	err := c.client.Database(c.database).RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Decode(&result)
	if err != nil {
		return datastore.NewConnectionError(
			datastore.ProviderMongoDB,
			"failed to ping database",
			err,
		)
	}

	return nil
}

// NewChangeStreamWatcher creates a new change stream watcher using the existing implementation
func (c *MongoDBClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig TokenConfig,
	pipeline interface{}) (ChangeStreamWatcher, error) {
	// Convert to the existing configuration format
	mongoConfig := mongoWatcher.MongoDBConfig{
		URI:        c.config.GetConnectionURI(),
		Database:   c.database,
		Collection: c.collection,
		// Add TLS certificate configuration from the client's config
		ClientTLSCertConfig: mongoWatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: c.config.GetCertConfig().GetCertPath(),
			TlsKeyPath:  c.config.GetCertConfig().GetKeyPath(),
			CaCertPath:  c.config.GetCertConfig().GetCACertPath(),
		},
		// Copy timeout settings from client (these should be available from config)
		TotalPingTimeoutSeconds:          c.config.GetTimeoutConfig().GetPingTimeoutSeconds(),
		TotalPingIntervalSeconds:         c.config.GetTimeoutConfig().GetPingIntervalSeconds(),
		TotalCACertTimeoutSeconds:        c.config.GetTimeoutConfig().GetCACertTimeoutSeconds(),
		TotalCACertIntervalSeconds:       c.config.GetTimeoutConfig().GetCACertIntervalSeconds(),
		ChangeStreamRetryDeadlineSeconds: c.config.GetTimeoutConfig().GetChangeStreamRetryDeadlineSeconds(),
		ChangeStreamRetryIntervalSeconds: c.config.GetTimeoutConfig().GetChangeStreamRetryIntervalSeconds(),
	}

	storeTokenConfig := mongoWatcher.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}

	// Convert pipeline to mongo.Pipeline
	// Handle both direct mongo.Pipeline and converted []map[string]interface{}
	var mongoPipeline mongo.Pipeline

	switch p := pipeline.(type) {
	case mongo.Pipeline:
		mongoPipeline = p
	case []map[string]interface{}:
		slog.Info("Converting []map[string]interface{} to mongo.Pipeline", "length", len(p))
		// Convert from factory conversion format
		mongoPipeline = make(mongo.Pipeline, len(p))

		for i, stage := range p {
			// Convert map[string]interface{} to bson.D
			var bsonStage bson.D

			for key, value := range stage {
				bsonStage = append(bsonStage, bson.E{Key: key, Value: value})
			}

			mongoPipeline[i] = bsonStage
		}
	default:
		return nil, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"pipeline must be of type mongo.Pipeline or []map[string]interface{}",
			nil,
		).WithMetadata("actualType", fmt.Sprintf("%T", pipeline))
	}

	watcherInstance, err := mongoWatcher.NewChangeStreamWatcher(ctx, mongoConfig, storeTokenConfig, mongoPipeline)
	if err != nil {
		return nil, datastore.NewChangeStreamError(
			datastore.ProviderMongoDB,
			"failed to create change stream watcher",
			err,
		).WithMetadata("tokenConfig", tokenConfig)
	}

	return &mongoChangeStreamWatcher{watcher: watcherInstance}, nil
}

// Close closes the MongoDB client
func (c *MongoDBClient) Close(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

// Collection-specific methods for MongoDBCollectionClient

func (c *MongoDBCollectionClient) GetCollectionName() string {
	return c.collection
}

func (c *MongoDBCollectionClient) GetDatabaseName() string {
	return c.database
}

// Wrapper types for MongoDB-specific implementations

type mongoSingleResult struct {
	result *mongo.SingleResult
}

func (r *mongoSingleResult) Decode(v interface{}) error {
	return r.result.Decode(v)
}

func (r *mongoSingleResult) Err() error {
	return r.result.Err()
}

type mongoCursor struct {
	cursor *mongo.Cursor
}

func (c *mongoCursor) Next(ctx context.Context) bool {
	return c.cursor.Next(ctx)
}

func (c *mongoCursor) Decode(v interface{}) error {
	return c.cursor.Decode(v)
}

func (c *mongoCursor) Close(ctx context.Context) error {
	return c.cursor.Close(ctx)
}

func (c *mongoCursor) All(ctx context.Context, results interface{}) error {
	return c.cursor.All(ctx, results)
}

func (c *mongoCursor) Err() error {
	return c.cursor.Err()
}

type mongoChangeStreamWatcher struct {
	watcher   *mongoWatcher.ChangeStreamWatcher
	eventChan chan Event
	initOnce  sync.Once
}

func (w *mongoChangeStreamWatcher) Start(ctx context.Context) {
	w.watcher.Start(ctx)
}

// Events returns the event channel for this watcher.
// The returned channel is initialized once and cached, so multiple calls
// return the same channel. Safe for concurrent use.
func (w *mongoChangeStreamWatcher) Events() <-chan Event {
	w.initOnce.Do(func() {
		w.eventChan = make(chan Event)
		bsonChan := w.watcher.Events()

		go func() {
			defer close(w.eventChan)

			for rawEvent := range bsonChan {
				w.eventChan <- &mongoEvent{rawEvent: rawEvent}
			}
		}()
	})

	return w.eventChan
}

func (w *mongoChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	return w.watcher.MarkProcessed(ctx, token)
}

func (w *mongoChangeStreamWatcher) GetUnprocessedEventCount(ctx context.Context,
	lastProcessedID string) (int64, error) {
	objectID, err := primitive.ObjectIDFromHex(lastProcessedID)
	if err != nil {
		return 0, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			fmt.Sprintf("invalid object ID %s", lastProcessedID),
			err,
		).WithMetadata("lastProcessedID", lastProcessedID)
	}

	return w.watcher.GetUnprocessedEventCount(ctx, objectID)
}

func (w *mongoChangeStreamWatcher) Close(ctx context.Context) error {
	return w.watcher.Close(ctx)
}

// Helper function to extract map keys for error metadata
func getMapKeys(m interface{}) []string {
	switch v := m.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}

		return keys
	case bson.M:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}

		return keys
	default:
		return []string{"<unable to extract keys>"}
	}
}

// ConvertAgnosticPipelineToMongo converts a database-agnostic pipeline to MongoDB pipeline
func ConvertAgnosticPipelineToMongo(pipeline datastore.Pipeline) (interface{}, error) {
	if len(pipeline) == 0 {
		// Return an insert-only pipeline as default
		return mongo.Pipeline{
			bson.D{{Key: "$match", Value: bson.D{
				{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert"}}}},
			}}},
		}, nil
	}

	mongoPipeline := make(mongo.Pipeline, len(pipeline))

	for i, doc := range pipeline {
		mongoDoc, err := convertDocumentToBsonD(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to convert document at index %d: %w", i, err)
		}

		mongoPipeline[i] = mongoDoc
	}

	return mongoPipeline, nil
}

// convertDocumentToBsonD recursively converts a datastore.Document to bson.D
func convertDocumentToBsonD(doc datastore.Document) (bson.D, error) {
	result := bson.D{}

	for _, elem := range doc {
		convertedValue, err := convertValueToBson(elem.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to convert value for key %s: %w", elem.Key, err)
		}

		result = append(result, bson.E{Key: elem.Key, Value: convertedValue})
	}

	return result, nil
}

// convertValueToBson recursively converts values to BSON-compatible types
func convertValueToBson(value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case datastore.Document:
		return convertDocumentToBsonD(v)
	case datastore.Array:
		result := bson.A{}

		for i, item := range v {
			convertedItem, err := convertValueToBson(item)
			if err != nil {
				return nil, fmt.Errorf("failed to convert array item at index %d: %w", i, err)
			}

			result = append(result, convertedItem)
		}

		return result, nil
	default:
		// For primitive types (string, int, bool, etc.), return as-is
		return v, nil
	}
}

// normalizeMongoTypes recursively converts MongoDB-specific types to standard Go types
// This ensures that modules don't have to deal with MongoDB-specific types like primitive.DateTime
func normalizeMongoTypes(doc map[string]interface{}) {
	for key, value := range doc {
		doc[key] = normalizeValue(value)
	}
}

// normalizeValue converts a single value, handling MongoDB-specific types and nested structures
func normalizeValue(value interface{}) interface{} {
	switch v := value.(type) {
	case primitive.DateTime:
		// Convert MongoDB DateTime to standard time.Time
		return v.Time()
	case primitive.ObjectID:
		// Keep ObjectID as-is for _id fields, but could convert to string if needed
		return v
	case map[string]interface{}:
		// Recursively normalize nested documents
		normalizeMongoTypes(v)
		return v
	case bson.M:
		// Handle bson.M (which is map[string]interface{})
		normalizeMongoTypes(v)
		return v
	case []interface{}:
		// Recursively normalize arrays
		for i, item := range v {
			v[i] = normalizeValue(item)
		}

		return v
	case primitive.A:
		// Handle primitive.A arrays
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = normalizeValue(item)
		}

		return result
	default:
		// Return primitive types as-is
		return v
	}
}
