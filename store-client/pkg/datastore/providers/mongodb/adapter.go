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
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

// AdaptedMongoStore adapts our existing MongoDB client to implement the new DataStore interface
type AdaptedMongoStore struct {
	databaseClient   client.DatabaseClient
	collectionClient client.CollectionClient
	factory          *factory.ClientFactory
	config           datastore.DataStoreConfig

	// Store implementations
	maintenanceStore datastore.MaintenanceEventStore
	healthStore      datastore.HealthEventStore
}

// NewAdaptedMongoStore creates a new adapted MongoDB store
func NewAdaptedMongoStore(ctx context.Context, certMountPath *string,
	dsConfig datastore.DataStoreConfig) (datastore.DataStore, error) {
	// Set up environment variables for backward compatibility
	oldCollectionName := os.Getenv("MONGODB_COLLECTION_NAME")

	if collectionName := dsConfig.Options["collectionName"]; collectionName != "" {
		os.Setenv("MONGODB_COLLECTION_NAME", collectionName)

		defer func() {
			if oldCollectionName == "" {
				os.Unsetenv("MONGODB_COLLECTION_NAME")
			} else {
				os.Setenv("MONGODB_COLLECTION_NAME", oldCollectionName)
			}
		}()
	}

	// Prefer TLSConfig from DataStoreConfig if set (from LoadDatastoreConfig)
	// This ensures our backward-compatible TLS configuration is used
	if certMountPath == nil && dsConfig.Connection.TLSConfig != nil && dsConfig.Connection.TLSConfig.CAPath != "" {
		// Extract directory from CA cert path
		certDir := filepath.Dir(dsConfig.Connection.TLSConfig.CAPath)
		certMountPath = &certDir
		slog.Info("Using certificate path from DataStoreConfig", "path", certDir)
	}

	// Create database configuration using our existing config system
	var databaseConfig config.DatabaseConfig

	var err error

	if certMountPath != nil {
		databaseConfig, err = config.NewDatabaseConfigFromEnvWithDefaults(*certMountPath)
	} else {
		databaseConfig, err = config.NewDatabaseConfigFromEnvWithDefaults("")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create database config: %w", err)
	}

	// Create client factory
	clientFactory := factory.NewClientFactory(databaseConfig)

	// Create database client
	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	// Create collection client
	collectionClient, err := clientFactory.CreateCollectionClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create collection client: %w", err)
	}

	store := &AdaptedMongoStore{
		databaseClient:   databaseClient,
		collectionClient: collectionClient,
		factory:          clientFactory,
		config:           dsConfig,
	}

	// Initialize store implementations
	store.maintenanceStore = NewMongoMaintenanceEventStore(databaseClient, collectionClient)
	store.healthStore = NewMongoHealthEventStore(databaseClient, collectionClient)

	slog.Info("Successfully created adapted MongoDB store")

	return store, nil
}

// MaintenanceEventStore returns the maintenance event store
func (a *AdaptedMongoStore) MaintenanceEventStore() datastore.MaintenanceEventStore {
	return a.maintenanceStore
}

// HealthEventStore returns the health event store
func (a *AdaptedMongoStore) HealthEventStore() datastore.HealthEventStore {
	return a.healthStore
}

// Ping tests the connection
func (a *AdaptedMongoStore) Ping(ctx context.Context) error {
	return a.databaseClient.Ping(ctx)
}

// Close closes the connection
func (a *AdaptedMongoStore) Close(ctx context.Context) error {
	return a.databaseClient.Close(ctx)
}

// Provider returns the datastore provider type
func (a *AdaptedMongoStore) Provider() datastore.DataStoreProvider {
	return datastore.ProviderMongoDB
}

// GetDatabaseClient returns the underlying database client for MongoDB-specific operations
// This is used by services that need MongoDB-specific functionality like aggregation pipelines
func (a *AdaptedMongoStore) GetDatabaseClient() client.DatabaseClient {
	return a.databaseClient
}

// GetCollectionClient returns the underlying collection client for MongoDB-specific operations
func (a *AdaptedMongoStore) GetCollectionClient() client.CollectionClient {
	return a.collectionClient
}

// CreateChangeStreamWatcher creates a change stream watcher
func (a *AdaptedMongoStore) CreateChangeStreamWatcher(ctx context.Context, clientName string,
	pipeline interface{}) (datastore.ChangeStreamWatcher, error) {
	// Use our existing factory to create a change stream watcher
	// Note: Token configuration is loaded from environment variables by the factory
	// via config.TokenConfigFromEnv(clientName). To customize token collection,
	// set the MONGODB_TOKEN_COLLECTION_NAME environment variable.

	// CRITICAL: Pass the existing databaseClient to avoid creating duplicate clients
	watcher, err := a.factory.CreateChangeStreamWatcher(ctx, a.databaseClient, clientName, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	// Adapt the existing watcher to the new interface
	return NewAdaptedChangeStreamWatcher(watcher), nil
}

// NewChangeStreamWatcher creates a new change stream watcher for the MongoDB datastore
// This method makes MongoDB compatible with the datastore abstraction layer using a config map
func (a *AdaptedMongoStore) NewChangeStreamWatcher(
	ctx context.Context, config interface{},
) (datastore.ChangeStreamWatcher, error) {
	// Convert the generic config to MongoDB-specific parameters
	var clientName string

	var pipeline interface{}

	// Handle different config types that might be passed

	switch c := config.(type) {
	case map[string]interface{}:
		// Support generic config format from factory
		if clientNameVal, ok := c["ClientName"].(string); ok {
			clientName = clientNameVal
		}

		// Pipeline is optional for MongoDB
		if pipelineVal, ok := c["Pipeline"]; ok {
			pipeline = pipelineVal
		}
	default:
		return nil, fmt.Errorf("unsupported config type: %T", config)
	}

	// Validate required parameters
	if clientName == "" {
		return nil, fmt.Errorf("ClientName is required")
	}

	// Use the existing CreateChangeStreamWatcher method
	return a.CreateChangeStreamWatcher(ctx, clientName, pipeline)
}

// AdaptedChangeStreamWatcher adapts our existing change stream watcher to the new interface
type AdaptedChangeStreamWatcher struct {
	watcher   client.ChangeStreamWatcher
	eventChan chan datastore.EventWithToken
	initOnce  sync.Once
}

// NewAdaptedChangeStreamWatcher creates a new adapted change stream watcher
func NewAdaptedChangeStreamWatcher(watcher client.ChangeStreamWatcher) datastore.ChangeStreamWatcher {
	return &AdaptedChangeStreamWatcher{watcher: watcher}
}

// Events returns the events channel
// CRITICAL FIX: Only create the channel and goroutine ONCE to prevent event loss
func (a *AdaptedChangeStreamWatcher) Events() <-chan datastore.EventWithToken {
	a.initOnce.Do(func() {
		a.eventChan = make(chan datastore.EventWithToken)

		go func() {
			defer close(a.eventChan)

			for event := range a.watcher.Events() {
				// Convert from our existing Event interface to the new EventWithToken
				eventMap := make(map[string]interface{})

				// Extract the event data
				if err := event.UnmarshalDocument(&eventMap); err != nil {
					slog.Error("Failed to unmarshal event", "error", err)
					continue
				}

				// Create EventWithToken
				eventWithToken := datastore.EventWithToken{
					Event:       eventMap,
					ResumeToken: []byte(""), // We'll need to extract the resume token properly
				}

				a.eventChan <- eventWithToken
			}
		}()
	})

	return a.eventChan
}

// Start starts the watcher
func (a *AdaptedChangeStreamWatcher) Start(ctx context.Context) {
	a.watcher.Start(ctx)
}

// MarkProcessed marks events as processed
func (a *AdaptedChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	return a.watcher.MarkProcessed(ctx, token)
}

// Close closes the watcher
func (a *AdaptedChangeStreamWatcher) Close(ctx context.Context) error {
	return a.watcher.Close(ctx)
}

// Unwrap returns the underlying client.ChangeStreamWatcher
// This is needed for services that still use the old EventWatcher/EventProcessor
func (a *AdaptedChangeStreamWatcher) Unwrap() client.ChangeStreamWatcher {
	return a.watcher
}
