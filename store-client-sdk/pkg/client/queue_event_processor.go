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

package client

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"k8s.io/client-go/util/workqueue"
)

// QueueEventProcessorConfig extends EventProcessorConfig with queue-specific settings
type QueueEventProcessorConfig struct {
	EventProcessorConfig
	// WorkerCount is the number of worker goroutines processing the queue
	WorkerCount int
	// RateLimiter defines the rate limiting strategy for the queue
	RateLimiter workqueue.RateLimiter
}

// QueuedEvent represents an event in the processing queue
type QueuedEvent struct {
	Event   *model.HealthEventWithStatus
	EventID string
	Attempt int
}

// QueueEventProcessor provides a queue-based event processor using Kubernetes workqueue
// This is ideal for modules that need worker pool processing, rate limiting, and backoff
type QueueEventProcessor struct {
	changeStreamWatcher ChangeStreamWatcher
	databaseClient      DatabaseClient
	config              QueueEventProcessorConfig
	eventHandler        EventHandler
	queue               workqueue.RateLimitingInterface
	stopCh              chan struct{}
	workerGroup         sync.WaitGroup
}

// NewQueueEventProcessor creates a new queue-based event processor
func NewQueueEventProcessor(watcher ChangeStreamWatcher, dbClient DatabaseClient, config QueueEventProcessorConfig) EventProcessor {
	if config.WorkerCount == 0 {
		config.WorkerCount = 1 // Default to single worker
	}
	if config.RateLimiter == nil {
		// Default exponential backoff: 10ms to 5min
		config.RateLimiter = workqueue.DefaultControllerRateLimiter()
	}

	return &QueueEventProcessor{
		changeStreamWatcher: watcher,
		databaseClient:      dbClient,
		config:              config,
		queue:               workqueue.NewRateLimitingQueue(config.RateLimiter),
		stopCh:              make(chan struct{}),
	}
}

// SetEventHandler sets the callback function for processing events
func (p *QueueEventProcessor) SetEventHandler(handler EventHandler) {
	p.eventHandler = handler
}

// Start begins processing events from the change stream with worker pool
func (p *QueueEventProcessor) Start(ctx context.Context) error {
	if p.eventHandler == nil {
		return fmt.Errorf("event handler must be set before starting queue processor")
	}

	slog.Info("Starting queue-based event processor", "workers", p.config.WorkerCount)

	// Start worker goroutines
	for i := 0; i < p.config.WorkerCount; i++ {
		p.workerGroup.Add(1)
		go p.worker(ctx, i)
	}

	// Start the change stream watcher
	p.changeStreamWatcher.Start(ctx)

	// Start event ingestion
	go p.ingestEvents(ctx)

	// Wait for shutdown
	<-p.stopCh
	return nil
}

// Stop gracefully shuts down the processor and waits for workers to finish
func (p *QueueEventProcessor) Stop(ctx context.Context) error {
	slog.Info("Stopping queue-based event processor")

	// Signal stop
	close(p.stopCh)

	// Shutdown queue
	p.queue.ShutDown()

	// Wait for workers to finish
	p.workerGroup.Wait()

	// Close change stream
	return p.changeStreamWatcher.Close(ctx)
}

// ingestEvents reads from change stream and adds events to the queue
func (p *QueueEventProcessor) ingestEvents(ctx context.Context) {
	slog.Info("Starting event ingestion from change stream")

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping event ingestion")
			return
		case <-p.stopCh:
			slog.Info("Stop signal received, stopping event ingestion")
			return
		case event, ok := <-p.changeStreamWatcher.Events():
			if !ok {
				slog.Info("Event channel closed, stopping ingestion")
				return
			}

			// Unmarshal the event
			var healthEventWithStatus model.HealthEventWithStatus
			if err := event.UnmarshalDocument(&healthEventWithStatus); err != nil {
				slog.Error("Failed to unmarshal event, skipping", "error", err)
				// Still mark as processed to avoid stuck streams
				if markErr := p.changeStreamWatcher.MarkProcessed(ctx); markErr != nil {
					slog.Error("Failed to mark failed event as processed", "error", markErr)
				}
				continue
			}

			eventID, err := event.GetDocumentID()
			if err != nil {
				slog.Error("Failed to get document ID, skipping event", "error", err)
				// Still mark as processed
				if markErr := p.changeStreamWatcher.MarkProcessed(ctx); markErr != nil {
					slog.Error("Failed to mark event as processed", "error", markErr)
				}
				continue
			}

			// Add to queue for processing
			queuedEvent := &QueuedEvent{
				Event:   &healthEventWithStatus,
				EventID: eventID,
				Attempt: 0,
			}

			p.queue.Add(queuedEvent)
			slog.Debug("Event added to processing queue", "eventID", eventID)
		}
	}
}

// worker processes events from the queue
func (p *QueueEventProcessor) worker(ctx context.Context, workerID int) {
	defer p.workerGroup.Done()

	slog.Info("Starting queue worker", "workerID", workerID)

	for {
		// Get next item from queue
		item, shutdown := p.queue.Get()
		if shutdown {
			slog.Info("Queue shutdown, stopping worker", "workerID", workerID)
			return
		}

		// Process the event
		queuedEvent, ok := item.(*QueuedEvent)
		if !ok {
			slog.Error("Invalid item type in queue", "workerID", workerID)
			p.queue.Done(item)
			continue
		}

		p.processQueuedEvent(ctx, queuedEvent, workerID)
		p.queue.Done(item)
	}
}

// processQueuedEvent handles a single queued event
func (p *QueueEventProcessor) processQueuedEvent(ctx context.Context, queuedEvent *QueuedEvent, workerID int) {
	startTime := time.Now()
	queuedEvent.Attempt++

	slog.Debug("Processing queued event",
		"workerID", workerID,
		"eventID", queuedEvent.EventID,
		"attempt", queuedEvent.Attempt)

	// Process the event
	err := p.eventHandler.ProcessEvent(ctx, queuedEvent.Event)

	if err != nil {
		slog.Error("Event processing failed",
			"workerID", workerID,
			"eventID", queuedEvent.EventID,
			"attempt", queuedEvent.Attempt,
			"error", err)

		// Check if we should retry
		maxRetries := p.config.MaxRetries
		if maxRetries < 0 || queuedEvent.Attempt < maxRetries {
			// Re-queue for retry with exponential backoff
			p.queue.AddRateLimited(queuedEvent)
			p.updateMetrics("processing_retry", queuedEvent.EventID, time.Since(startTime), false)
			return
		} else {
			// Max retries reached
			slog.Error("Max retries reached for event",
				"eventID", queuedEvent.EventID,
				"attempts", queuedEvent.Attempt)
			p.updateMetrics("processing_failed", queuedEvent.EventID, time.Since(startTime), false)
		}
	} else {
		// Success
		slog.Debug("Event processed successfully",
			"workerID", workerID,
			"eventID", queuedEvent.EventID)
		p.updateMetrics("processing_success", queuedEvent.EventID, time.Since(startTime), true)
	}

	// Always mark as processed in the change stream to advance resume token
	// This prevents the change stream from getting stuck on failed events
	if markErr := p.changeStreamWatcher.MarkProcessed(ctx); markErr != nil {
		slog.Error("Failed to mark event as processed in change stream",
			"eventID", queuedEvent.EventID,
			"error", markErr)
		p.updateMetrics("mark_processed_error", queuedEvent.EventID, time.Since(startTime), false)
	}
}

// updateMetrics updates processing metrics if enabled
func (p *QueueEventProcessor) updateMetrics(eventType, eventID string, duration time.Duration, success bool) {
	if !p.config.EnableMetrics {
		return
	}

	labels := make(map[string]string)
	for k, v := range p.config.MetricsLabels {
		labels[k] = v
	}
	labels["event_type"] = eventType
	labels["success"] = fmt.Sprintf("%t", success)
	labels["processor_type"] = "queue"

	slog.Debug("Queue event processing metrics",
		"labels", labels,
		"eventID", eventID,
		"duration_ms", duration.Milliseconds(),
		"queue_depth", p.queue.Len(),
	)
}

// GetQueueDepth returns the current number of items in the processing queue
func (p *QueueEventProcessor) GetQueueDepth() int {
	return p.queue.Len()
}