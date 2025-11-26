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

package postgresql

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

// PostgreSQLWatcherFactory implements WatcherFactory for PostgreSQL
type PostgreSQLWatcherFactory struct{}

// NewPostgreSQLWatcherFactory creates a new PostgreSQL watcher factory
func NewPostgreSQLWatcherFactory() watcher.WatcherFactory {
	return &PostgreSQLWatcherFactory{}
}

// CreateChangeStreamWatcher creates a PostgreSQL change stream watcher.
//
//nolint:cyclop // Function has clear linear flow, splitting would reduce readability
func (f *PostgreSQLWatcherFactory) CreateChangeStreamWatcher(
	ctx context.Context,
	ds datastore.DataStore,
	config watcher.WatcherConfig,
) (datastore.ChangeStreamWatcher, error) {
	pgStore, ok := ds.(*PostgreSQLDataStore)
	if !ok {
		return nil, fmt.Errorf("expected PostgreSQL datastore, got %T", ds)
	}

	clientName := f.extractClientName(config)
	tableName := config.CollectionName

	if tableName == "" {
		return nil, fmt.Errorf("CollectionName (table name) is required for PostgreSQL watcher")
	}

	snakeCaseTableName := toSnakeCase(tableName)

	slog.Info("Creating PostgreSQL change stream watcher",
		"clientName", clientName,
		"originalTableName", tableName,
		"postgresTableName", snakeCaseTableName)

	changeStreamWatcher := NewPostgreSQLChangeStreamWatcher(
		pgStore.db,
		clientName,
		snakeCaseTableName,
		pgStore.connString,
		ModeHybrid,
	)

	f.applyPipelineFilter(changeStreamWatcher, config.Pipeline, tableName)

	return NewPostgreSQLChangeStreamWatcherWithUnwrap(changeStreamWatcher), nil
}

// extractClientName extracts client name from config options.
func (f *PostgreSQLWatcherFactory) extractClientName(config watcher.WatcherConfig) string {
	if config.Options != nil {
		if name, ok := config.Options["ClientName"].(string); ok && name != "" {
			return name
		}
	}

	return "watcher-factory"
}

// applyPipelineFilter applies pipeline filter to the watcher if provided.
func (f *PostgreSQLWatcherFactory) applyPipelineFilter(
	w *PostgreSQLChangeStreamWatcher,
	pipeline datastore.Pipeline,
	tableName string,
) {
	if len(pipeline) == 0 {
		return
	}

	slog.Info("PostgreSQL: Setting up two-layer filtering (server-side SQL + application-side fallback)",
		"tableName", tableName,
		"pipelineStages", len(pipeline))

	w.pipeline = pipeline

	pipelineFilter, err := NewPipelineFilter(pipeline)
	if err != nil {
		slog.Warn("Failed to create application-side pipeline filter",
			"error", err,
			"tableName", tableName)

		return
	}

	if pipelineFilter != nil {
		w.pipelineFilter = pipelineFilter
	}
}

// SupportedProvider returns the provider this factory supports
func (f *PostgreSQLWatcherFactory) SupportedProvider() datastore.DataStoreProvider {
	return datastore.ProviderPostgreSQL
}

// init registers the PostgreSQL watcher factory
func init() {
	watcher.RegisterWatcherFactory(datastore.ProviderPostgreSQL, NewPostgreSQLWatcherFactory())
}
