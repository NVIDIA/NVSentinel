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

package helper

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/nvidia/nvsentinel/store-client/pkg/adapter"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

// DatastoreClientConfig holds configuration for creating datastore clients
type DatastoreClientConfig struct {
	// ModuleName is used for logging and client identification
	ModuleName string

	// DataStoreConfig contains the new-style datastore configuration
	// If nil, configuration will be loaded from environment variables
	DataStoreConfig *datastore.DataStoreConfig

	// DatabaseClientCertMountPath specifies custom certificate mount path
	// If empty, will be read from MONGODB_CLIENT_CERT_MOUNT_PATH environment variable
	DatabaseClientCertMountPath string

	// Pipeline for change stream watcher (if needed)
	Pipeline interface{}
}

// DatastoreClientBundle contains all the clients created for a module
type DatastoreClientBundle struct {
	DatabaseClient      client.DatabaseClient
	ChangeStreamWatcher client.ChangeStreamWatcher
	Factory             *factory.ClientFactory
}

// Close gracefully closes all clients in the bundle
func (bundle *DatastoreClientBundle) Close(ctx context.Context) error {
	var errors []error

	if bundle.ChangeStreamWatcher != nil {
		if err := bundle.ChangeStreamWatcher.Close(ctx); err != nil {
			errors = append(errors, fmt.Errorf("failed to close change stream watcher: %w", err))
		}
	}

	if bundle.DatabaseClient != nil {
		if err := bundle.DatabaseClient.Close(ctx); err != nil {
			errors = append(errors, fmt.Errorf("failed to close database client: %w", err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors closing datastore bundle: %v", errors)
	}

	return nil
}

// NewDatastoreClient creates a standardized datastore client bundle
// This consolidates all the different initialization patterns used across modules
func NewDatastoreClient(ctx context.Context, config DatastoreClientConfig) (*DatastoreClientBundle, error) {
	if config.ModuleName == "" {
		return nil, fmt.Errorf("module name is required")
	}

	slog.Info("Initializing datastore client", "module", config.ModuleName)

	// Create the client factory using the appropriate method based on configuration
	clientFactory, err := createStandardizedFactory(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client factory for %s: %w", config.ModuleName, err)
	}

	// Create database client
	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client for %s: %w", config.ModuleName, err)
	}

	bundle := &DatastoreClientBundle{
		DatabaseClient: databaseClient,
		Factory:        clientFactory,
	}

	// Create change stream watcher if pipeline is provided
	// CRITICAL: Pass the SAME database client to avoid creating duplicate clients with different channels!
	if config.Pipeline != nil {
		changeStreamWatcher, err := clientFactory.CreateChangeStreamWatcher(
			ctx, databaseClient, config.ModuleName, config.Pipeline)
		if err != nil {
			// Clean up database client on failure
			databaseClient.Close(ctx)
			return nil, fmt.Errorf("failed to create change stream watcher for %s: %w", config.ModuleName, err)
		}

		bundle.ChangeStreamWatcher = changeStreamWatcher
	}

	slog.Info("Successfully initialized datastore client",
		"module", config.ModuleName,
		"hasWatcher", bundle.ChangeStreamWatcher != nil,
	)

	return bundle, nil
}

// createStandardizedFactory creates a client factory using the best available method
func createStandardizedFactory(config DatastoreClientConfig) (*factory.ClientFactory, error) {
	// Method 1: If DataStoreConfig is provided, use it directly with adapter conversion
	if config.DataStoreConfig != nil {
		slog.Debug("Using provided DataStoreConfig", "module", config.ModuleName)

		if config.DatabaseClientCertMountPath != "" {
			// Use cert path version of adapter
			legacyConfig := adapter.ConvertDataStoreConfigToLegacyWithCertPath(
				config.DataStoreConfig, config.DatabaseClientCertMountPath,
			)

			return factory.NewClientFactory(legacyConfig), nil
		} else {
			// Use standard adapter
			legacyConfig := adapter.ConvertDataStoreConfigToLegacy(config.DataStoreConfig)
			return factory.NewClientFactory(legacyConfig), nil
		}
	}

	// Method 2: Load from environment with custom cert path
	if config.DatabaseClientCertMountPath != "" {
		slog.Debug("Using environment config with custom cert path",
			"module", config.ModuleName,
			"certPath", config.DatabaseClientCertMountPath)

		return factory.NewClientFactoryFromEnvWithCertPath(config.DatabaseClientCertMountPath)
	}

	// Method 3: Check environment variable for cert path
	if envCertPath := os.Getenv("MONGODB_CLIENT_CERT_MOUNT_PATH"); envCertPath != "" {
		slog.Debug("Using environment config with cert path from env var",
			"module", config.ModuleName,
			"certPath", envCertPath)

		return factory.NewClientFactoryFromEnvWithCertPath(envCertPath)
	}

	// Method 4: Default - load from environment without cert path
	slog.Debug("Using standard environment config", "module", config.ModuleName)

	return factory.NewClientFactoryFromEnv()
}

// NewDatabaseClientOnly creates just a database client (no change stream watcher)
// This is a convenience function for modules that only need database operations
func NewDatabaseClientOnly(ctx context.Context, moduleName string) (client.DatabaseClient, error) {
	config := DatastoreClientConfig{
		ModuleName: moduleName,
		Pipeline:   nil, // No change stream watcher
	}

	bundle, err := NewDatastoreClient(ctx, config)
	if err != nil {
		return nil, err
	}

	return bundle.DatabaseClient, nil
}

// NewChangeStreamWatcherOnly creates just a change stream watcher (no database client)
// This is a convenience function for modules that only need event watching
func NewChangeStreamWatcherOnly(
	ctx context.Context, moduleName string, pipeline interface{},
) (client.ChangeStreamWatcher, error) {
	config := DatastoreClientConfig{
		ModuleName: moduleName,
		Pipeline:   pipeline,
	}

	bundle, err := NewDatastoreClient(ctx, config)
	if err != nil {
		return nil, err
	}

	// Close the database client since we only want the watcher
	if bundle.DatabaseClient != nil {
		bundle.DatabaseClient.Close(ctx)
	}

	return bundle.ChangeStreamWatcher, nil
}

// NewDatastoreClientFromConfig creates a bundle from an explicit DataStoreConfig
// This is useful when the config is loaded from a custom source (not environment)
func NewDatastoreClientFromConfig(
	ctx context.Context, moduleName string, dsConfig datastore.DataStoreConfig, pipeline interface{},
) (*DatastoreClientBundle, error) {
	config := DatastoreClientConfig{
		ModuleName:      moduleName,
		DataStoreConfig: &dsConfig,
		Pipeline:        pipeline,
	}

	return NewDatastoreClient(ctx, config)
}

// NewDatastoreClientWithCertPath creates a bundle with a custom certificate path
// This is useful for modules that have specific certificate mount requirements
func NewDatastoreClientWithCertPath(
	ctx context.Context, moduleName string, certMountPath string, pipeline interface{},
) (*DatastoreClientBundle, error) {
	config := DatastoreClientConfig{
		ModuleName:                  moduleName,
		DatabaseClientCertMountPath: certMountPath,
		Pipeline:                    pipeline,
	}

	return NewDatastoreClient(ctx, config)
}
