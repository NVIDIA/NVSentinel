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

package factory

import (
	"context"
	"fmt"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ClientFactory provides a simple interface for creating database clients
type ClientFactory struct {
	dbConfig config.DatabaseConfig
}

// NewClientFactory creates a new client factory with database configuration
func NewClientFactory(dbConfig config.DatabaseConfig) *ClientFactory {
	return &ClientFactory{
		dbConfig: dbConfig,
	}
}

// NewClientFactoryFromEnv creates a client factory using environment variables
// This is the most common way modules will create clients
func NewClientFactoryFromEnv() (*ClientFactory, error) {
	dbConfig, err := config.NewDatabaseConfigFromEnv()
	if err != nil {
		return nil, datastore.NewConfigurationError(
			"", // Provider unknown at factory level
			"failed to load database configuration from environment",
			err,
		)
	}

	return NewClientFactory(dbConfig), nil
}

// NewClientFactoryFromEnvWithCertPath creates a client factory with custom certificate path
func NewClientFactoryFromEnvWithCertPath(certMountPath string) (*ClientFactory, error) {
	dbConfig, err := config.NewDatabaseConfigFromEnvWithDefaults(certMountPath)
	if err != nil {
		return nil, datastore.NewConfigurationError(
			"", // Provider unknown at factory level
			"failed to load database configuration from environment",
			err,
		).WithMetadata("certMountPath", certMountPath)
	}

	return NewClientFactory(dbConfig), nil
}

// CreateDatabaseClient creates a new database client
func (f *ClientFactory) CreateDatabaseClient(ctx context.Context) (client.DatabaseClient, error) {
	return client.NewMongoDBClient(ctx, f.dbConfig)
}

// CreateCollectionClient creates a new collection-specific client
func (f *ClientFactory) CreateCollectionClient(ctx context.Context) (client.CollectionClient, error) {
	return client.NewMongoDBCollectionClient(ctx, f.dbConfig)
}

// CreateChangeStreamWatcher creates a change stream watcher with the given configuration
// It requires an existing database client to avoid creating duplicate clients
func (f *ClientFactory) CreateChangeStreamWatcher(
	ctx context.Context,
	dbClient client.DatabaseClient,
	clientName string,
	pipeline interface{},
) (client.ChangeStreamWatcher, error) {
	tokenConfig, err := config.TokenConfigFromEnv(clientName)
	if err != nil {
		return nil, datastore.NewConfigurationError(
			datastore.ProviderMongoDB, // Factory is currently MongoDB-specific
			"failed to create token configuration",
			err,
		).WithMetadata("clientName", clientName)
	}

	// Convert database-agnostic pipeline to MongoDB-specific if needed
	mongoPipeline, err := convertToMongoPipeline(pipeline)
	if err != nil {
		return nil, datastore.NewValidationError(
			datastore.ProviderMongoDB,
			"failed to convert pipeline to MongoDB format",
			err,
		).WithMetadata("pipeline", pipeline)
	}

	watcher, err := dbClient.NewChangeStreamWatcher(ctx, client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}, mongoPipeline)
	if err != nil {
		return nil, datastore.NewChangeStreamError(
			datastore.ProviderMongoDB,
			"failed to create change stream watcher",
			err,
		).WithMetadata("clientName", clientName).WithMetadata("tokenConfig", tokenConfig)
	}

	return watcher, nil
}

// GetDatabaseConfig returns the database configuration used by this factory
func (f *ClientFactory) GetDatabaseConfig() config.DatabaseConfig {
	return f.dbConfig
}

// convertToMongoPipeline converts various pipeline types to MongoDB-compatible pipeline
func convertToMongoPipeline(pipeline interface{}) (interface{}, error) {
	switch p := pipeline.(type) {
	case datastore.Pipeline:
		// Use the client package conversion function to avoid circular imports
		return client.ConvertAgnosticPipelineToMongo(p)
	case []interface{}:
		// Convert []interface{} to mongo.Pipeline for change streams
		// This handles pipelines created by client.NewPipelineBuilder()
		mongoPipeline := make([]map[string]interface{}, len(p))

		for i, stage := range p {
			if stageMap, ok := stage.(map[string]interface{}); ok {
				mongoPipeline[i] = stageMap
			} else {
				return nil, fmt.Errorf("invalid pipeline stage type: %T", stage)
			}
		}

		return mongoPipeline, nil
	default:
		// Assume it's already a MongoDB pipeline (backward compatibility)
		return pipeline, nil
	}
}
