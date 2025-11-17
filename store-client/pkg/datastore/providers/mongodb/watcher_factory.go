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
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

// MongoWatcherFactory implements WatcherFactory for MongoDB
type MongoWatcherFactory struct{}

// NewMongoWatcherFactory creates a new MongoDB watcher factory
func NewMongoWatcherFactory() watcher.WatcherFactory {
	return &MongoWatcherFactory{}
}

// CreateChangeStreamWatcher creates a MongoDB change stream watcher
func (f *MongoWatcherFactory) CreateChangeStreamWatcher(
	ctx context.Context,
	ds datastore.DataStore,
	config watcher.WatcherConfig,
) (datastore.ChangeStreamWatcher, error) {
	// Type assert to MongoDB store
	mongoStore, ok := ds.(*AdaptedMongoStore)
	if !ok {
		return nil, fmt.Errorf("expected MongoDB datastore, got %T", ds)
	}

	// Convert database-agnostic pipeline to MongoDB-specific pipeline
	mongoPipeline, err := client.ConvertAgnosticPipelineToMongo(config.Pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to convert pipeline: %w", err)
	}

	// Create the change stream watcher using the existing factory
	watcher, err := mongoStore.CreateChangeStreamWatcher(ctx, "watcher-factory", mongoPipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to create MongoDB change stream watcher: %w", err)
	}

	return watcher, nil
}

// SupportedProvider returns the provider this factory supports
func (f *MongoWatcherFactory) SupportedProvider() datastore.DataStoreProvider {
	return datastore.ProviderMongoDB
}

// init registers the MongoDB watcher factory
func init() {
	watcher.RegisterWatcherFactory(datastore.ProviderMongoDB, NewMongoWatcherFactory())
}
