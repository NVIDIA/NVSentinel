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
	"time"

	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/metrics"

	"go.mongodb.org/mongo-driver/bson"
	"k8s.io/client-go/util/workqueue"
)

func NewEventQueueManager() EventQueueManager {
	mgr := &eventQueueManager{
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.NewTypedItemExponentialFailureRateLimiter[NodeEvent](10*time.Second, 5*time.Minute),
		),
		shutdown: make(chan struct{}),
	}

	return mgr
}

func (m *eventQueueManager) SetEventProcessor(processor EventProcessor) {
	m.eventProcessor = processor
}

func (m *eventQueueManager) EnqueueEvent(ctx context.Context,
	nodeName string, event bson.M, collection MongoCollectionAPI) error {
	if ctx.Err() != nil {
		return fmt.Errorf("context cancelled while enqueueing event for node %s: %w", nodeName, ctx.Err())
	}

	select {
	case <-m.shutdown:
		return fmt.Errorf("queue is shutting down")
	default:
	}

	nodeEvent := NodeEvent{
		NodeName:   nodeName,
		Event:      &event,
		Collection: collection,
	}

	m.queue.Add(nodeEvent)
	metrics.NodeQueueDepth.WithLabelValues(nodeName).Set(float64(m.queue.Len()))

	return nil
}

func (m *eventQueueManager) Shutdown() {
	slog.Info("Shutting down workqueue")
	m.queue.ShutDown()
	close(m.shutdown)
	slog.Info("Workqueue shutdown complete")
}
