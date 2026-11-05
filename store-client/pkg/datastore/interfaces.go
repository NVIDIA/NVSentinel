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
)

// DataStore is the main interface that all providers must implement
type DataStore interface {
	// Health Events (Platform Connectors, Fault Quarantine, etc.)
	HealthEventStore() HealthEventStore

	// Connection management
	Ping(ctx context.Context) error
	Close(ctx context.Context) error

	// Provider identification
	Provider() DataStoreProvider
}

// HealthEventStore handles platform health events
type HealthEventStore interface {
	// Core event operations
	UpdateHealthEventStatus(ctx context.Context, id string, status HealthEventStatus) error

	// Query builder operations (database-agnostic)
	// MongoDB: converts builder to map and uses existing code
	// PostgreSQL: converts builder to SQL and uses native queries
	FindHealthEventsByQuery(ctx context.Context, builder QueryBuilder) ([]HealthEventWithStatus, error)

	// Writes a service's span ID into the span_ids map for trace context propagation.
	UpdateSpanID(ctx context.Context, id string, serviceName string, spanID string) error

	// FindLatestHealthEventByQuery returns the single most recent (by createdAt) event
	// matching the query, or nil if none match. It pushes the sort+limit to the
	// datastore so callers (e.g. cold start's session-end lookup) do not load every
	// matching event into memory.
	FindLatestHealthEventByQuery(ctx context.Context, builder QueryBuilder) (*HealthEventWithStatus, error)

	// Cold-start support: iterates matching events in bounded batches, ordered
	// from oldest to newest by creation time and document ID.
	// fn is called once per batch; return a non-nil error from fn to stop iteration.
	// Implementations must remain correct when fn updates records so they no
	// longer match the query.
	FindHealthEventsByQueryBatched(
		ctx context.Context, builder QueryBuilder, batchSize int,
		fn func([]HealthEventWithStatus) error,
	) error
}

// QueryBuilder interface for database-agnostic queries
type QueryBuilder interface {
	ToMongo() map[string]any
	ToSQL() (string, []any)
}

// UpdateBuilder interface for database-agnostic updates
type UpdateBuilder interface {
	ToMongo() map[string]any
	ToSQL() (string, []any)
}

// ChangeStreamWatcher provides change stream functionality
type ChangeStreamWatcher interface {
	Events() <-chan EventWithToken
	Start(ctx context.Context)
	MarkProcessed(ctx context.Context, token []byte) error
	Close(ctx context.Context) error
}

// DataStoreFactory creates datastore instances
type DataStoreFactory interface {
	NewDataStore(ctx context.Context, config DataStoreConfig) (DataStore, error)
}
