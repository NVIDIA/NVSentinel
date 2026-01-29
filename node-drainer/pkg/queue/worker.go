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
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Interfaces are defined in types.go

func (m *eventQueueManager) Start(ctx context.Context) {
	slog.Info("Starting workqueue processor")

	go m.runWorker(ctx)
}

func (m *eventQueueManager) runWorker(ctx context.Context) {
	for m.processNextWorkItem(ctx) {
	}

	slog.Info("Worker stopped")
}

func (m *eventQueueManager) processNextWorkItem(ctx context.Context) bool {
	nodeEvent, shutdown := m.queue.Get()
	if shutdown {
		return false
	}

	defer m.queue.Done(nodeEvent)

	var err error
	// Use database-agnostic interface
	if nodeEvent.Event != nil && nodeEvent.Database != nil && nodeEvent.HealthEventStore != nil {
		err = m.processEventGeneric(ctx, *nodeEvent.Event, nodeEvent.Database, nodeEvent.HealthEventStore, nodeEvent.NodeName)
	} else {
		err = fmt.Errorf("event data, database interface, or health event store not available")
	}

	if err != nil {
		slog.Warn("Error processing event for node (will retry)",
			"node", nodeEvent.NodeName,
			"attempt", m.queue.NumRequeues(nodeEvent)+1,
			"error", err)
		m.queue.AddRateLimited(nodeEvent)
	} else {
		m.queue.Forget(nodeEvent)
	}

	metrics.QueueDepth.Set(float64(m.queue.Len()))

	return true
}

func (m *eventQueueManager) processEventGeneric(ctx context.Context,
	event datastore.Event, database DataStore, healthEventStore datastore.HealthEventStore, nodeName string) error {
	if m.dataStoreEventProcessor == nil {
		return fmt.Errorf("no datastore event processor configured")
	}

	return m.dataStoreEventProcessor.ProcessEventGeneric(ctx, event, database, healthEventStore, nodeName)
}

// processEvent method has been removed - only processEventGeneric is used now
