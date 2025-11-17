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

package watcher

import (
	"context"
	"fmt"
	"sync"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// WatcherConfig holds configuration for creating change stream watchers
type WatcherConfig struct {
	// Pipeline operations to filter events
	Pipeline datastore.Pipeline `json:"pipeline,omitempty"`

	// Collection or table name to watch
	CollectionName string `json:"collectionName"`

	// Resume token for continuing from a specific point
	ResumeToken []byte `json:"resumeToken,omitempty"`

	// Provider-specific options
	Options map[string]interface{} `json:"options,omitempty"`
}

// WatcherFactory creates change stream watchers for specific datastore providers
type WatcherFactory interface {
	CreateChangeStreamWatcher(
		ctx context.Context,
		ds datastore.DataStore,
		config WatcherConfig,
	) (datastore.ChangeStreamWatcher, error)
	SupportedProvider() datastore.DataStoreProvider
}

// Global registry for watcher factories
var (
	watcherFactories = make(map[datastore.DataStoreProvider]WatcherFactory)
	watcherMutex     sync.RWMutex
)

// RegisterWatcherFactory registers a watcher factory for a specific provider
func RegisterWatcherFactory(provider datastore.DataStoreProvider, factory WatcherFactory) {
	watcherMutex.Lock()
	defer watcherMutex.Unlock()

	watcherFactories[provider] = factory
}

// CreateChangeStreamWatcher creates a change stream watcher for the given datastore
func CreateChangeStreamWatcher(
	ctx context.Context,
	ds datastore.DataStore,
	config WatcherConfig,
) (datastore.ChangeStreamWatcher, error) {
	// Determine the provider from the datastore config
	provider, err := getProviderFromDataStore(ds)
	if err != nil {
		return nil, fmt.Errorf("failed to determine datastore provider: %w", err)
	}

	watcherMutex.RLock()

	factory, exists := watcherFactories[provider]

	watcherMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no watcher factory registered for provider: %s", provider)
	}

	return factory.CreateChangeStreamWatcher(ctx, ds, config)
}

// getProviderFromDataStore extracts the provider type from a datastore instance
func getProviderFromDataStore(ds datastore.DataStore) (datastore.DataStoreProvider, error) {
	return ds.Provider(), nil
}
