// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const (
	defaultMaxInFlight     = 1000
	defaultShutdownTimeout = 5 * time.Second
)

type partitionedTask struct {
	seq   uint64
	event Event
}

// PartitionedEventProcessor processes change stream events concurrently across a worker pool,
// partitioned by node name. Events for the same node are guaranteed to process sequentially,
// while events for different nodes are evaluated concurrently.
//
// Checkpoints (resume tokens) are advanced using a low-water mark tracker to guarantee that
// out-of-order completions never advance the checkpoint past an unresolved earlier event.
type PartitionedEventProcessor struct {
	changeStreamWatcher    ChangeStreamWatcher
	databaseClient         DatabaseClient
	config                 EventProcessorConfig
	eventHandler           EventHandler
	workers                int
	workerChs              []chan *partitionedTask
	tracker                *LowWaterMarkTracker
	stopCh                 chan struct{}
	stopOnce               sync.Once
	cancelMu               sync.Mutex
	cancelWorkers          context.CancelFunc
	checkpointMu           sync.Mutex
	pendingCheckpointToken []byte
	wg                     sync.WaitGroup
}

// NewPartitionedEventProcessor creates a new PartitionedEventProcessor.
func NewPartitionedEventProcessor(
	watcher ChangeStreamWatcher, dbClient DatabaseClient, config EventProcessorConfig,
) *PartitionedEventProcessor {
	workers := config.Workers
	if workers <= 1 {
		workers = 1
	}

	workerChs := make([]chan *partitionedTask, workers)
	for i := range workers {
		workerChs[i] = make(chan *partitionedTask, 64)
	}

	return &PartitionedEventProcessor{
		changeStreamWatcher: watcher,
		databaseClient:      dbClient,
		config:              config,
		workers:             workers,
		workerChs:           workerChs,
		tracker:             NewLowWaterMarkTracker(),
		stopCh:              make(chan struct{}),
	}
}

// SetEventHandler sets the callback function for processing events.
func (p *PartitionedEventProcessor) SetEventHandler(handler EventHandler) {
	p.eventHandler = handler
}

// Start begins processing events concurrently from the change stream.
func (p *PartitionedEventProcessor) Start(ctx context.Context) error {
	if p.eventHandler == nil {
		return fmt.Errorf("event handler must be set before starting processor")
	}

	slog.Info("Starting partitioned event processor", "workers", p.workers)

	if p.changeStreamWatcher != nil {
		p.changeStreamWatcher.Start(ctx)
	} else {
		slog.Info("No change stream watcher available")
		<-ctx.Done()

		return ctx.Err()
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	p.cancelMu.Lock()
	p.cancelWorkers = cancelWorkers

	select {
	case <-p.stopCh:
		cancelWorkers()
	default:
	}

	p.cancelMu.Unlock()

	for i := range p.workers {
		p.wg.Add(1)

		go func(workerID int) {
			defer p.wg.Done()

			p.runWorker(workerCtx, workerID, p.workerChs[workerID])
		}(i)
	}

	err := p.processEvents(ctx)

	for _, ch := range p.workerChs {
		close(ch)
	}

	p.wg.Wait()

	p.checkpointMu.Lock()

	tokenToFlush := p.pendingCheckpointToken
	if flushToken := p.tracker.Flush(); len(flushToken) > 0 {
		tokenToFlush = flushToken
	}

	if len(tokenToFlush) > 0 {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancelShutdown()

		if markErr := p.markProcessed(shutdownCtx, tokenToFlush); markErr != nil {
			slog.Error("Failed to mark final checkpoint token", "error", markErr)

			p.pendingCheckpointToken = tokenToFlush
		} else {
			p.pendingCheckpointToken = nil
		}
	}
	p.checkpointMu.Unlock()

	return err
}

// Stop gracefully shuts down the processor.
func (p *PartitionedEventProcessor) Stop(ctx context.Context) error {
	slog.Info("Stopping partitioned event processor")

	p.stopOnce.Do(func() {
		close(p.stopCh)
	})

	p.cancelMu.Lock()
	if p.cancelWorkers != nil {
		p.cancelWorkers()
	}
	p.cancelMu.Unlock()

	if p.changeStreamWatcher != nil {
		return p.changeStreamWatcher.Close(ctx)
	}

	return nil
}

func (p *PartitionedEventProcessor) waitBackpressure(ctx context.Context, maxInFlight int) error {
	if p.tracker.InFlightCount() < maxInFlight {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopCh:
		return nil
	case <-p.tracker.DrainCh():
		return nil
	}
}

func (p *PartitionedEventProcessor) onEventsClosed(ctx context.Context) error {
	slog.Info("Event channel closed, stopping processor")

	if ctx.Err() != nil {
		return ctx.Err()
	}

	return nil
}

func (p *PartitionedEventProcessor) dispatchEvent(ctx context.Context, event Event) error {
	seq := p.tracker.Register(event.GetResumeToken())
	task := &partitionedTask{seq: seq, event: event}
	workerIdx := p.selectWorker(event)

	select {
	case p.workerChs[workerIdx] <- task:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopCh:
		return nil
	}
}

func (p *PartitionedEventProcessor) processEvents(ctx context.Context) error {
	slog.Info("Listening for events on the change stream channel...")

	maxInFlight := p.config.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = defaultMaxInFlight
	}

	eventsCh := p.changeStreamWatcher.Events()

	for {
		if err := p.waitBackpressure(ctx, maxInFlight); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping event processor")

			return ctx.Err()
		case <-p.stopCh:
			slog.Info("Stop signal received, shutting down event processor")

			return nil
		case event, ok := <-eventsCh:
			if !ok {
				return p.onEventsClosed(ctx)
			}

			if err := p.dispatchEvent(ctx, event); err != nil {
				return err
			}
		}
	}
}

func (p *PartitionedEventProcessor) runWorker(ctx context.Context, id int, ch <-chan *partitionedTask) {
	slog.Debug("Starting worker goroutine", "workerID", id)

	for task := range ch {
		select {
		case <-ctx.Done():
			slog.Debug("Discarding uncheckpointed task during shutdown",
				"workerID", id, "seq", task.seq)

			continue
		default:
		}

		if err := p.handleTask(ctx, task); err != nil {
			slog.Error("Worker failed to handle task", "workerID", id, "seq", task.seq, "error", err)

			var uncheckpointedErr *uncheckpointedEventError
			if errors.As(err, &uncheckpointedErr) {
				if stopErr := p.Stop(ctx); stopErr != nil {
					slog.Error("Failed to stop processor on uncheckpointed error", "error", stopErr)
				}

				return
			}
		}
	}
}

func (p *PartitionedEventProcessor) parseEvent(event Event) (*model.HealthEventWithStatus, string, error) {
	var healthEventWithStatus model.HealthEventWithStatus
	if err := event.UnmarshalDocument(&healthEventWithStatus); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal event: %w", err)
	}

	eventID, err := event.GetDocumentID()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get document ID: %w", err)
	}

	return &healthEventWithStatus, eventID, nil
}

func (p *PartitionedEventProcessor) shouldSkip(event Event) bool {
	return p.config.SkipEvent != nil && p.config.SkipEvent(event)
}

func (p *PartitionedEventProcessor) handleProcessError(
	ctx context.Context, seq uint64, eventID string, startTime time.Time, processErr error,
) error {
	p.updateMetrics("processing_failed", eventID, time.Since(startTime), false)
	slog.Error("Event processing failed", "eventID", eventID, "seq", seq, "error", processErr)

	// Context cancellations and timeouts are transient conditions and must not be marked
	// as processed, allowing the event to be retried on restart rather than permanently skipped.
	if errors.Is(processErr, context.Canceled) || errors.Is(processErr, context.DeadlineExceeded) || ctx.Err() != nil {
		return newUncheckpointedEventError(processErr)
	}

	if p.config.MarkProcessedOnError {
		slog.Warn("Marking failed event as processed due to MarkProcessedOnError=true", "eventID", eventID)

		p.onTaskCompleted(ctx, seq)

		return processErr
	}

	return newUncheckpointedEventError(processErr)
}

func (p *PartitionedEventProcessor) handleTask(ctx context.Context, task *partitionedTask) error {
	startTime := time.Now()
	event := task.event
	seq := task.seq

	if p.shouldSkip(event) {
		p.updateMetrics("processing_skipped", "", time.Since(startTime), true)
		p.onTaskCompleted(ctx, seq)

		return nil
	}

	healthEventWithStatus, eventID, err := p.parseEvent(event)
	if err != nil {
		p.updateMetrics("unmarshal_error", "", time.Since(startTime), false)

		if p.config.MarkProcessedOnError {
			p.onTaskCompleted(ctx, seq)

			return nil
		}

		return newUncheckpointedEventError(err)
	}

	eventCtx := ctx

	if p.config.EventTimeout > 0 {
		var cancel context.CancelFunc

		eventCtx, cancel = context.WithTimeout(ctx, p.config.EventTimeout)
		defer cancel()
	}

	slog.Debug("Processing event", "eventID", eventID, "seq", seq)

	processErr := p.eventHandler.ProcessEvent(eventCtx, healthEventWithStatus)
	if processErr != nil {
		return p.handleProcessError(ctx, seq, eventID, startTime, processErr)
	}

	p.updateMetrics("processing_success", eventID, time.Since(startTime), true)
	p.onTaskCompleted(ctx, seq)

	return nil
}

func (p *PartitionedEventProcessor) onTaskCompleted(ctx context.Context, seq uint64) {
	p.checkpointMu.Lock()
	defer p.checkpointMu.Unlock()

	advancedToken := p.tracker.MarkDone(seq)
	if len(advancedToken) > 0 {
		p.pendingCheckpointToken = advancedToken
	}

	if len(p.pendingCheckpointToken) == 0 {
		return
	}

	if markErr := p.markProcessed(ctx, p.pendingCheckpointToken); markErr != nil {
		slog.Error("Failed to checkpoint low-water mark resume token", "error", markErr)
	} else {
		p.pendingCheckpointToken = nil
	}
}

func (p *PartitionedEventProcessor) selectWorker(event Event) int {
	if p.workers <= 1 {
		return 0
	}

	key := ""
	if p.config.PartitionKeyFunc != nil {
		key = p.config.PartitionKeyFunc(event)
	} else if nodeName, err := event.GetNodeName(); err == nil {
		key = nodeName
	}

	if key == "" {
		return 0
	}

	return int(hashNode(key)&0x7fffffff) % p.workers
}

func (p *PartitionedEventProcessor) markProcessed(ctx context.Context, token []byte) error {
	if p.changeStreamWatcher == nil {
		return nil
	}

	return p.changeStreamWatcher.MarkProcessed(ctx, token)
}

func (p *PartitionedEventProcessor) updateMetrics(
	eventType, eventID string, duration time.Duration, success bool,
) {
	if !p.config.EnableMetrics {
		return
	}

	labels := make(map[string]string)
	maps.Copy(labels, p.config.MetricsLabels)

	labels["event_type"] = eventType
	labels["success"] = fmt.Sprintf("%t", success)

	slog.Debug("Event processing metrics",
		"labels", labels,
		"eventID", eventID,
		"duration_ms", duration.Milliseconds(),
	)
}

func hashNode(node string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(node))

	return h.Sum32()
}
