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

	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/config"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore"
	mongoWatcher "github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore/providers/mongodb/watcher"
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

	// Convert generic map to bson.M for internal processing
	document := bson.M(fullDocument.(map[string]interface{}))

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

func (e *mongoEvent) GetNodeName() (string, error) {
	fullDocument, ok := e.rawEvent["fullDocument"]
	if !ok {
		return "", datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"error extracting fullDocument from event",
			nil,
		).WithMetadata("eventKeys", getMapKeys(e.rawEvent))
	}

	// Convert generic map to bson.M for internal processing
	document := bson.M(fullDocument.(map[string]interface{}))

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

	// Convert generic map to bson.M for internal processing
	document := bson.M(fullDocument.(map[string]interface{}))

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

	return nil
}

// Legacy pipeline builders for backward compatibility
// Deprecated: Use BuildInsertOnlyPipelineAgnostic() instead for database-agnostic support

// BuildInsertOnlyPipeline creates a MongoDB-specific pipeline for insert operations
// Deprecated: Use BuildInsertOnlyPipelineAgnostic() instead
func BuildInsertOnlyPipeline() interface{} {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert"}}}},
		}}},
	}
}

// BuildQuarantineUpdatePipeline creates a MongoDB-specific pipeline for quarantine updates
// Deprecated: Use BuildQuarantineUpdatePipelineAgnostic() instead
func BuildQuarantineUpdatePipeline() interface{} {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: "update"},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "updateDescription.updatedFields",
					Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: "Quarantined"}}}},
				bson.D{{Key: "updateDescription.updatedFields",
					Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: "UnQuarantined"}}}},
			}},
		}}},
	}
}

// BuildNonFatalInsertPipeline creates a MongoDB-specific pipeline for non-fatal insert operations
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

// BuildInsertOnlyPipelineAgnostic creates a database-agnostic pipeline for insert operations
func BuildInsertOnlyPipelineAgnostic() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert")),
				)),
			)),
		),
	)
}

// BuildQuarantineUpdatePipelineAgnostic creates a database-agnostic pipeline for quarantine updates
func BuildQuarantineUpdatePipelineAgnostic() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "update"),
				datastore.E("$or", datastore.A(
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", "Quarantined"),
						)),
					),
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", "UnQuarantined"),
						)),
					),
				)),
			)),
		),
	)
}

// BuildNonFatalInsertPipelineAgnostic creates a database-agnostic pipeline for non-fatal insert operations
func BuildNonFatalInsertPipelineAgnostic() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "insert"),
				datastore.E("fullDocument.healthevent.isfatal", false),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	)
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
	update := bson.M{
		"$set": bson.M{
			statusPath: status,
		},
	}

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
	cursor, err := c.mongoCol.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, datastore.NewQueryError(
			datastore.ProviderMongoDB,
			"failed to execute aggregation",
			err,
		).WithMetadata("pipeline", pipeline)
	}

	return &mongoCursor{cursor: cursor}, nil
}

// WithTransaction executes a function within a transaction
func (c *MongoDBClient) WithTransaction(ctx context.Context, fn func(SessionContext) error) error {
	session, err := c.client.StartSession()
	if err != nil {
		return datastore.NewConnectionError(
			datastore.ProviderMongoDB,
			"failed to start session",
			err,
		)
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		sessionCtx := &mongoSessionContext{SessionContext: sc}
		return nil, fn(sessionCtx)
	})

	if err != nil {
		return datastore.NewTransactionError(
			datastore.ProviderMongoDB,
			"transaction failed",
			err,
		)
	}

	return nil
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

type mongoSessionContext struct {
	mongo.SessionContext
}

type mongoChangeStreamWatcher struct {
	watcher *mongoWatcher.ChangeStreamWatcher
}

func (w *mongoChangeStreamWatcher) Start(ctx context.Context) {
	w.watcher.Start(ctx)
}

func (w *mongoChangeStreamWatcher) Events() <-chan Event {
	bsonChan := w.watcher.Events()
	eventChan := make(chan Event)

	go func() {
		defer close(eventChan)

		for rawEvent := range bsonChan {
			eventChan <- &mongoEvent{rawEvent: rawEvent}
		}
	}()

	return eventChan
}

func (w *mongoChangeStreamWatcher) MarkProcessed(ctx context.Context) error {
	return w.watcher.MarkProcessed(ctx)
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
