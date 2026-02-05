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

package cache

import (
	"sync"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

// Event types for watch notifications.
const (
	EventTypeAdded    = "ADDED"
	EventTypeModified = "MODIFIED"
	EventTypeDeleted  = "DELETED"
	EventTypeError    = "ERROR"
)

// WatchEvent represents a change event for a GPU resource.
type WatchEvent struct {
	// Type indicates the nature of the change.
	Type string

	// Object is the GPU resource.
	Object *v1alpha1.Gpu
}

// subscriber represents a watch subscriber.
type subscriber struct {
	id      string
	channel chan WatchEvent
}

// Broadcaster manages watch event broadcasting to multiple subscribers.
//
// Events are sent to all active subscribers. If a subscriber's buffer is full,
// the event is dropped for that subscriber (non-blocking send).
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[string]*subscriber
	bufferSize  int
	logger      klog.Logger
	onEventDrop func() // Optional callback when events are dropped
}

// NewBroadcaster creates a new Broadcaster instance.
func NewBroadcaster(logger klog.Logger, bufferSize int) *Broadcaster {
	if bufferSize <= 0 {
		bufferSize = 100
	}

	return &Broadcaster{
		subscribers: make(map[string]*subscriber),
		bufferSize:  bufferSize,
		logger:      logger,
	}
}

// SetOnEventDrop sets a callback function that will be called when an event
// is dropped due to a full subscriber buffer. This is useful for metrics.
func (b *Broadcaster) SetOnEventDrop(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onEventDrop = fn
}

// Subscribe creates a new subscription and returns a channel to receive events.
//
// The returned channel has a buffer to prevent blocking the broadcaster.
// If the buffer fills up, events will be dropped for this subscriber.
//
// The caller must call Unsubscribe when done to clean up resources.
func (b *Broadcaster) Subscribe(id string) <-chan WatchEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close existing subscription if any
	if existing, ok := b.subscribers[id]; ok {
		close(existing.channel)
		delete(b.subscribers, id)
	}

	ch := make(chan WatchEvent, b.bufferSize)
	b.subscribers[id] = &subscriber{
		id:      id,
		channel: ch,
	}

	b.logger.V(2).Info("Subscriber added", "id", id, "totalSubscribers", len(b.subscribers))

	return ch
}

// Unsubscribe removes a subscription and closes its channel.
//
// After calling Unsubscribe, the channel returned by Subscribe will be closed.
func (b *Broadcaster) Unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if sub, ok := b.subscribers[id]; ok {
		close(sub.channel)
		delete(b.subscribers, id)
		b.logger.V(2).Info("Subscriber removed", "id", id, "totalSubscribers", len(b.subscribers))
	}
}

// Notify sends an event to all subscribers.
//
// This is a non-blocking operation. If a subscriber's buffer is full,
// the event is dropped for that subscriber and the onEventDrop callback
// is invoked (if set).
func (b *Broadcaster) Notify(event WatchEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		select {
		case sub.channel <- event:
			// Event sent successfully
		default:
			// Buffer full, drop event for this subscriber
			b.logger.V(1).Info("Event dropped due to full buffer",
				"subscriberID", sub.id,
				"eventType", event.Type,
				"gpuName", event.Object.GetMetadata().GetName(),
			)
			if b.onEventDrop != nil {
				b.onEventDrop()
			}
		}
	}
}

// SubscriberCount returns the number of active subscribers.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.subscribers)
}

// Close closes all subscriber channels and removes all subscriptions.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, sub := range b.subscribers {
		close(sub.channel)
		delete(b.subscribers, id)
	}

	b.logger.V(1).Info("Broadcaster closed")
}
