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
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// EventProcessorConfig holds configuration for the unified event processor
type EventProcessorConfig struct {
	// MaxRetries for event processing (used by QueueEventProcessor for retry limit)
	// DefaultEventProcessor ignores this and does not retry
	// Set to 0 for no retries, -1 for unlimited retries
	MaxRetries int
	// EnableMetrics controls whether to track processing metrics
	EnableMetrics bool
	// MetricsLabels for module-specific metrics categorization
	MetricsLabels map[string]string
	// MarkProcessedOnError determines whether to mark events as processed even if handler returns error
	// Set to false (default) to preserve event for retry on next restart
	// Set to true to skip failed events and continue (use with caution - may lose events)
	MarkProcessedOnError bool
}

// EventProcessor provides a unified interface for processing change stream events
type EventProcessor interface {
	// Start begins processing events from the change stream
	Start(ctx context.Context) error
	// Stop gracefully shuts down the processor
	Stop(ctx context.Context) error
	// SetEventHandler sets the callback function for processing events
	SetEventHandler(handler EventHandler)
}

// EventHandler defines the callback interface for event processing
type EventHandler interface {
	// ProcessEvent handles a single health event and returns success status
	ProcessEvent(ctx context.Context, event *model.HealthEventWithStatus) error
}

// EventHandlerFunc is a function adapter for EventHandler interface
type EventHandlerFunc func(ctx context.Context, event *model.HealthEventWithStatus) error

func (f EventHandlerFunc) ProcessEvent(ctx context.Context, event *model.HealthEventWithStatus) error {
	return f(ctx, event)
}

// DefaultEventProcessor provides a standard implementation of event processing
type DefaultEventProcessor struct {
	changeStreamWatcher ChangeStreamWatcher
	databaseClient      DatabaseClient
	config              EventProcessorConfig
	eventHandler        EventHandler
	stopCh              chan struct{}
}

// NewEventProcessor creates a new unified event processor
func NewEventProcessor(
	watcher ChangeStreamWatcher, dbClient DatabaseClient, config EventProcessorConfig,
) EventProcessor {
	return &DefaultEventProcessor{
		changeStreamWatcher: watcher,
		databaseClient:      dbClient,
		config:              config,
		stopCh:              make(chan struct{}),
	}
}

// SetEventHandler sets the callback function for processing events
func (p *DefaultEventProcessor) SetEventHandler(handler EventHandler) {
	p.eventHandler = handler
}

// Start begins processing events from the change stream
func (p *DefaultEventProcessor) Start(ctx context.Context) error {
	if p.eventHandler == nil {
		return fmt.Errorf("event handler must be set before starting processor")
	}

	slog.Info("Starting unified event processor")

	if p.changeStreamWatcher != nil {
		p.changeStreamWatcher.Start(ctx)
	} else {
		slog.Info("No change stream watcher available")
		<-ctx.Done()

		return ctx.Err()
	}

	// Process events in main loop
	return p.processEvents(ctx)
}

// Stop gracefully shuts down the processor
func (p *DefaultEventProcessor) Stop(ctx context.Context) error {
	slog.Info("Stopping unified event processor")
	close(p.stopCh)

	if p.changeStreamWatcher != nil {
		return p.changeStreamWatcher.Close(ctx)
	}

	return nil
}

// processEvents handles the main event processing loop
func (p *DefaultEventProcessor) processEvents(ctx context.Context) error {
	slog.Info("Listening for events on the channel...")

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping event processor")
			return ctx.Err()
		case <-p.stopCh:
			slog.Info("Stop signal received, shutting down event processor")
			return nil
		case event, ok := <-p.changeStreamWatcher.Events():
			if !ok {
				slog.Info("Event channel closed, stopping processor")
				return nil
			}

			eventID, _ := event.GetDocumentID()
			slog.Debug("Processing event", "eventID", eventID)

			if err := p.handleSingleEvent(ctx, event); err != nil {
				slog.Error("Failed to handle event", "eventID", eventID, "error", err)
			}
		}
	}
}

// handleSingleEvent processes a single event.
// IMPORTANT: Does NOT retry internally - handler is responsible for its own retries if needed.
// This prevents retry-induced blocking of the event stream.
func (p *DefaultEventProcessor) handleSingleEvent(ctx context.Context, event Event) error {
	startTime := time.Now()
	token := event.GetResumeToken()

	var healthEventWithStatus model.HealthEventWithStatus
	if err := event.UnmarshalDocument(&healthEventWithStatus); err != nil {
		p.updateMetrics("unmarshal_error", "", time.Since(startTime), false)

		if markErr := p.markProcessed(ctx, token); markErr != nil {
			slog.Error("Failed to mark processed after unmarshal error", "error", markErr)
		}

		return fmt.Errorf("failed to unmarshal event: %w", err)
	}

	eventID, err := event.GetDocumentID()
	if err != nil {
		p.updateMetrics("document_id_error", "", time.Since(startTime), false)

		if markErr := p.markProcessed(ctx, token); markErr != nil {
			slog.Error("Failed to mark processed after document ID error", "error", markErr)
		}

		return fmt.Errorf("failed to get document ID: %w", err)
	}

	slog.Debug("Processing event", "eventID", eventID, "event", healthEventWithStatus)

	processErr := p.eventHandler.ProcessEvent(ctx, &healthEventWithStatus)
	if processErr != nil {
		p.updateMetrics("processing_failed", eventID, time.Since(startTime), false)
		slog.Error("Event processing failed", "eventID", eventID, "error", processErr)

		return p.handleProcessingError(ctx, eventID, processErr, token)
	}

	p.updateMetrics("processing_success", eventID, time.Since(startTime), true)

	if markErr := p.markProcessed(ctx, token); markErr != nil {
		p.updateMetrics("mark_processed_error", eventID, time.Since(startTime), false)

		return fmt.Errorf("failed to mark event as processed: %w", markErr)
	}

	return nil
}

func (p *DefaultEventProcessor) handleProcessingError(
	ctx context.Context, eventID string, processErr error, token []byte,
) error {
	if !p.config.MarkProcessedOnError {
		slog.Error("Event processing failed, NOT marking as processed - will retry on restart",
			"eventID", eventID, "error", processErr)

		return processErr
	}

	slog.Warn("Marking failed event as processed due to MarkProcessedOnError=true",
		"eventID", eventID, "error", processErr)

	if markErr := p.markProcessed(ctx, token); markErr != nil {
		slog.Error("Failed to mark processed after error", "error", markErr)

		return fmt.Errorf("failed to mark event as processed: %w", markErr)
	}

	return processErr
}

// markProcessed marks the current event as processed in the change stream watcher.
// Returns nil if no watcher is configured.
func (p *DefaultEventProcessor) markProcessed(ctx context.Context, token []byte) error {
	if p.changeStreamWatcher == nil {
		return nil
	}

	return p.changeStreamWatcher.MarkProcessed(ctx, token)
}

// updateMetrics updates processing metrics if enabled
func (p *DefaultEventProcessor) updateMetrics(eventType, eventID string, duration time.Duration, success bool) {
	if !p.config.EnableMetrics {
		return
	}

	// This is a placeholder for metrics integration
	// In a real implementation, this would integrate with prometheus or similar
	labels := make(map[string]string)
	for k, v := range p.config.MetricsLabels {
		labels[k] = v
	}

	labels["event_type"] = eventType
	labels["success"] = fmt.Sprintf("%t", success)

	slog.Debug("Event processing metrics",
		"labels", labels,
		"eventID", eventID,
		"duration_ms", duration.Milliseconds(),
	)
}
