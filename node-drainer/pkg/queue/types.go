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

package queue

import (
	"context"

	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type NodeEvent struct {
	NodeName         string
	EventID          string                     // Unique event ID for deduplication (from _id field)
	Event            *datastore.Event           // Database-agnostic event data
	HealthEventStore datastore.HealthEventStore // New database-agnostic interface
	Database         DataStore                  // New database-agnostic interface

	// Deprecated fields for backward compatibility
	EventBSON *map[string]interface{} // DEPRECATED: Use Event instead
	// Collection field has been removed - use Database instead
}

// DataStore provides database-agnostic operations using store-client
type DataStore interface {
	UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error)
	FindDocument(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error)
	FindDocuments(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error)
}

// DataStoreEventProcessor provides database-agnostic event processing
type DataStoreEventProcessor interface {
	ProcessEventGeneric(ctx context.Context, event datastore.Event, database DataStore,
		healthEventStore datastore.HealthEventStore, nodeName string) error
}

// EventProcessor interface has been removed - use DataStoreEventProcessor instead

type EventQueueManager interface {
	// New database-agnostic method
	EnqueueEventGeneric(ctx context.Context, nodeName string, event datastore.Event, database DataStore,
		healthEventStore datastore.HealthEventStore) error

	// Deprecated EnqueueEvent method has been removed - use EnqueueEventGeneric instead

	Start(ctx context.Context)
	Shutdown()

	// New database-agnostic method
	SetDataStoreEventProcessor(processor DataStoreEventProcessor)

	// Deprecated SetEventProcessor method has been removed - use SetDataStoreEventProcessor instead
}

type eventQueueManager struct {
	queue                   workqueue.TypedRateLimitingInterface[NodeEvent]
	dataStoreEventProcessor DataStoreEventProcessor // New database-agnostic processor
	shutdown                chan struct{}
}
