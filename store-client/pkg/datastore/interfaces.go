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
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// DataStore is the main interface that all providers must implement
type DataStore interface {
	// Maintenance Events (CSP Health Monitor)
	MaintenanceEventStore() MaintenanceEventStore

	// Health Events (Platform Connectors, Fault Quarantine, etc.)
	HealthEventStore() HealthEventStore

	// Connection management
	Ping(ctx context.Context) error
	Close(ctx context.Context) error

	// Provider identification
	Provider() DataStoreProvider
}

// MaintenanceEventStore handles CSP maintenance events
type MaintenanceEventStore interface {
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

// HealthEventStore handles platform health events
type HealthEventStore interface {
	// Core event operations
	UpdateHealthEventStatus(ctx context.Context, id string, status HealthEventStatus) error
	UpdateHealthEventStatusByNode(ctx context.Context, nodeName string, status HealthEventStatus) error

	// Query operations
	FindHealthEventsByNode(ctx context.Context, nodeName string) ([]HealthEventWithStatus, error)
	FindHealthEventsByFilter(ctx context.Context, filter map[string]interface{}) ([]HealthEventWithStatus, error)
	FindHealthEventsByStatus(ctx context.Context, status Status) ([]HealthEventWithStatus, error)

	// Query builder operations (database-agnostic)
	// MongoDB: converts builder to map and uses existing code
	// PostgreSQL: converts builder to SQL and uses native queries
	FindHealthEventsByQuery(ctx context.Context, builder QueryBuilder) ([]HealthEventWithStatus, error)
	UpdateHealthEventsByQuery(ctx context.Context, queryBuilder QueryBuilder, updateBuilder UpdateBuilder) error

	// Convenience methods for common operations
	UpdateNodeQuarantineStatus(ctx context.Context, eventID string, status Status) error
	UpdatePodEvictionStatus(ctx context.Context, eventID string, status OperationStatus) error
	UpdateRemediationStatus(ctx context.Context, eventID string, status interface{}) error

	// Node drain specific operations
	CheckIfNodeAlreadyDrained(ctx context.Context, nodeName string) (bool, error)
	FindLatestEventForNode(ctx context.Context, nodeName string) (*HealthEventWithStatus, error)
}

// QueryBuilder interface for database-agnostic queries
type QueryBuilder interface {
	ToMongo() map[string]interface{}
	ToSQL() (string, []interface{})
}

// UpdateBuilder interface for database-agnostic updates
type UpdateBuilder interface {
	ToMongo() map[string]interface{}
	ToSQL() (string, []interface{})
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
	SupportedProviders() []DataStoreProvider
}
