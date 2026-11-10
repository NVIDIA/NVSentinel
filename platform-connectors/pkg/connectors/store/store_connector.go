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

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

// Outcomes of a successful batch insert, the label of batchesWritten.
const (
	// outcomeStored means every document of the batch was inserted.
	outcomeStored = "stored"
	// outcomeDuplicate means the only failures were duplicate-key violations of
	// the idempotency index, i.e. a replayed batch whose events already exist.
	outcomeDuplicate = "duplicate"
)

// batchesWritten counts the batches written to the datastore, by outcome:
// inside the request for the deployment platform connector, from the ring
// buffer for the node-local DaemonSet.
var batchesWritten = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "platform_connector_store_batches_total",
	Help: "Batches written to the datastore, by outcome: stored, or duplicate (a resend whose events already existed)",
}, []string{"outcome"})

// EventIdempotencyKey is the idempotency key of the event at index in a batch
// keyed batchKey. The deployment platform connector keys a batch as
// callerPodUID#clientKey before the pipeline runs; the ring buffer loop keys
// its own as instanceID#sequence when it writes (see batchKey).
func EventIdempotencyKey(batchKey string, index int) string {
	return batchKey + "#" + strconv.Itoa(index)
}

type DatabaseStoreConnector struct {
	// databaseClient is the database-agnostic client
	databaseClient client.DatabaseClient
	// ringBuffer is the queue the node-local loop drains; nil in the deployment role
	ringBuffer *ringbuffer.RingBuffer
	maxRetries int
	// instanceID and batchSeq form the idempotency keys of the batches the
	// ring buffer loop writes; see batchKey.
	instanceID string
	batchSeq   atomic.Uint64
}

func InitializeDatabaseStoreConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	clientCertMountPath string, maxRetries int) (*DatabaseStoreConnector, error) {
	connector := &DatabaseStoreConnector{
		ringBuffer: ringbuffer,
		maxRetries: maxRetries,
		instanceID: uuid.NewString(),
	}

	// Create database client factory using store-client
	clientFactory, err := createClientFactory(clientCertMountPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client factory: %w", err)
	}

	// Create database client
	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	connector.databaseClient = databaseClient

	slog.InfoContext(ctx, "Successfully initialized database store connector",
		"maxRetries", maxRetries)

	return connector, nil
}

// VerifyIdempotencyIndex reports whether the idempotency index exists with the
// expected full definition and a completed build. The deployment platform
// connector gates its readiness on this, so no client can write before the
// datastore setup has created the index.
func (r *DatabaseStoreConnector) VerifyIdempotencyIndex(ctx context.Context) error {
	return r.databaseClient.VerifyHealthEventIdempotencyIndex(ctx)
}

func createClientFactory(databaseClientCertMountPath string) (*factory.ClientFactory, error) {
	// Always pass the cert path through explicitly. NewClientFactoryFromEnv()
	// falls back to DefaultCertMountPath even when TLS is disabled, causing
	// infinite cert polling. Using the explicit path variant ensures an empty
	// string (TLS disabled) propagates correctly.
	return factory.NewClientFactoryFromEnvWithCertPath(databaseClientCertMountPath)
}

func (r *DatabaseStoreConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Shutdown stops a loop that has not emptied its queue in time;
			// the batches left behind were acknowledged when they were queued.
			if queued := r.ringBuffer.CurrentLength(); queued > 0 {
				slog.ErrorContext(ctx, "Context canceled with batches still queued, dropping them permanently",
					"batchCount", queued)
			}

			slog.InfoContext(ctx, "Context canceled, exiting health metric processing loop")

			return
		default:
			queuedHealthEvents, quit := r.ringBuffer.Dequeue()
			if quit {
				slog.InfoContext(ctx, "Queue signaled shutdown, exiting processing loop")
				return
			}

			healthEvents := queuedHealthEvents.Events
			if healthEvents == nil || len(healthEvents.GetEvents()) == 0 {
				r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
				continue
			}

			batchCtx, span := tracing.StartSpanWithLinkFromSpanContext(
				ctx, queuedHealthEvents.ParentSpanContext, "platform_connector.store.fetch_and_process_health_metric")

			eventCount := len(healthEvents.GetEvents())

			// The batch is retried here, before the next one is taken, so a
			// monitor's batches are stored in the order it sent them even
			// when a write fails.
			outcome, retryCount, err := r.insertWithRetry(batchCtx, healthEvents, r.batchKey(queuedHealthEvents))
			if err != nil {
				tracing.RecordError(span, err)
				span.SetAttributes(
					attribute.String("platform_connector.store.error", err.Error()),
					attribute.Int("platform_connector.store.retry_count", retryCount),
					attribute.Int("platform_connector.store.max_retries", r.maxRetries),
					attribute.String("platform_connector.store.status", "failed"),
				)

				slog.ErrorContext(batchCtx, dropMessage(ctx, err),
					"error", err,
					"retryCount", retryCount,
					"maxRetries", r.maxRetries,
					"eventCount", eventCount,
					"firstEventNodeName", healthEvents.GetEvents()[0].GetNodeName(),
					"firstEventCheckName", healthEvents.GetEvents()[0].GetCheckName())
			} else {
				batchesWritten.WithLabelValues(outcome).Inc()
				span.SetAttributes(attribute.String("platform_connector.store.status", "inserted"))
			}

			r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)

			span.End()
		}
	}
}

// insertWithRetry writes one batch and, after a failed write, retries it in
// place with exponential backoff, up to maxRetries times. The loop holds the
// batch meanwhile, so no later batch is stored ahead of it. The backoff is the
// one the ring buffer was configured with, and a canceled context ends the
// retries. An event the datastore can never store as sent is dropped on its
// own, at Error, and the events after it are still stored. It returns the
// outcome and the number of retries made.
func (r *DatabaseStoreConnector) insertWithRetry(
	ctx context.Context, healthEvents *protos.HealthEvents, batchKey string,
) (string, int, error) {
	delay, maxDelay := r.ringBuffer.RetryDelays()
	batch := &batchWrite{events: healthEvents.GetEvents()}

	for retries := 0; ; {
		outcome, err := r.insertHealthEventsFrom(ctx, healthEvents, batchKey, batch.from)
		if err == nil {
			return batch.outcome(outcome), retries, nil
		}

		// An event the datastore can never store as sent gets the same answer
		// on every attempt, so retrying it would only hold the queue. The insert
		// stopped there, so the events after it are stored without it.
		if failure := unstorable(err); failure != nil {
			if !batch.skipRejected(ctx, err, failure) {
				return "", retries, err
			}

			if batch.from == len(batch.events) {
				return batch.outcome(outcomeDuplicate), retries, nil
			}

			continue
		}

		if retries >= r.maxRetries || ctx.Err() != nil {
			return outcome, retries, err
		}

		slog.WarnContext(ctx, "Error inserting health events, will retry in place with exponential backoff",
			"error", err,
			"retryCount", retries+1,
			"maxRetries", r.maxRetries,
			"retryDelay", delay,
			"eventCount", len(healthEvents.GetEvents()))
		r.ringBuffer.CountRetry()

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return "", retries, err
		case <-timer.C:
		}

		delay = min(delay*2, maxDelay)
		retries++
	}
}

// batchWrite follows one batch through insertWithRetry.
type batchWrite struct {
	events []*protos.HealthEvent
	// from is the first event not yet stored or dropped. The insert is ordered
	// and stops at the event it fails on, so the events before it are stored.
	from int
	// stored says an event of the batch is in the datastore, and inserted that
	// one was written by this batch rather than found there already.
	stored, inserted bool
}

// outcome is the batch's outcome once its last write returned last: stored
// when any of its events was written, else last.
func (b *batchWrite) outcome(last string) string {
	if b.inserted {
		return outcomeStored
	}

	return last
}

// skipRejected drops, at Error, the event the datastore can never store as
// sent, so the next write starts after it. It reports false when the batch has
// to be dropped whole instead: the event cannot be placed in the batch, or it
// is the last one and nothing of the batch is stored.
func (b *batchWrite) skipRejected(ctx context.Context, err error, failure *datastore.BulkWriteFailure) bool {
	index := failure.Failed.DocumentIndex
	b.stored = b.stored || failure.InsertedCount+failure.DuplicateCount > 0
	b.inserted = b.inserted || failure.InsertedCount > 0

	if index < b.from || index >= len(b.events) || (index == len(b.events)-1 && !b.stored) {
		return false
	}

	slog.ErrorContext(ctx, "Event cannot be stored as sent, dropping it permanently and storing the rest of its batch",
		"error", err,
		"eventIndex", index,
		"eventCount", len(b.events),
		"nodeName", b.events[index].GetNodeName(),
		"checkName", b.events[index].GetCheckName())

	b.from = index + 1

	return true
}

// dropMessage says why the loop drops a batch: it was stopped first, the
// datastore gave an answer no retry can change, or the retries ran out.
func dropMessage(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "Stopped before the batch was stored, dropping health events permanently"
	case unstorable(err) != nil:
		return "Batch cannot be stored as sent, dropping health events permanently"
	default:
		return "Max retries exceeded, dropping health events permanently"
	}
}

// batchKey returns the idempotency key prefix of a queued batch, chosen on
// its first write attempt and kept on the item: a batch retried after a
// failed write carries the same per-event keys, so its next attempt stores
// only what the first one did not, instead of storing the documents before
// the failure a second time.
func (r *DatabaseStoreConnector) batchKey(item *ringbuffer.QueuedHealthEvents) string {
	if item.BatchKey == "" {
		item.BatchKey = r.instanceID + "#" + strconv.FormatUint(r.batchSeq.Add(1), 10)
	}

	return item.BatchKey
}

// Disconnect closes the database client connection
// Safe to call multiple times - will not error if already disconnected
func (r *DatabaseStoreConnector) Disconnect(ctx context.Context) error {
	if r.databaseClient == nil {
		return nil
	}

	err := r.databaseClient.Close(ctx)
	if err != nil {
		// Log but don't return error if already disconnected
		// This can happen in tests where mtest framework also disconnects
		slog.WarnContext(ctx, "Error disconnecting database client (may already be disconnected)", "error", err)

		return nil
	}

	slog.InfoContext(ctx, "Successfully disconnected database client")

	return nil
}

// ProcessBatch writes one batch inside the caller's request, the deployment
// platform connector's path: the events already carry their idempotency keys
// (stamped before the pipeline ran), the reply follows the returned error,
// and a resend whose events already exist is a success, counted as a
// duplicate.
func (r *DatabaseStoreConnector) ProcessBatch(ctx context.Context, healthEvents *protos.HealthEvents) error {
	outcome, err := r.insertHealthEvents(ctx, healthEvents, "")
	if err != nil {
		return terminalIfDocumentError(err)
	}

	if len(healthEvents.GetEvents()) > 0 {
		batchesWritten.WithLabelValues(outcome).Inc()
	}

	return nil
}

// terminalIfDocumentError turns an answer about a document that a resend can
// never change into a status the caller does not retry. Every other failure is
// left as it is and answered with a retryable status: a document error can
// also be the server stepping down in the middle of the batch, which a resend
// cures.
func terminalIfDocumentError(err error) error {
	failure := unstorable(err)
	if failure == nil {
		return err
	}

	if failure.Failed.Duplicate {
		return status.Errorf(codes.InvalidArgument,
			"event %d duplicates a document already stored under index %q, so the batch cannot be stored as sent: %s",
			failure.Failed.DocumentIndex, failure.Failed.IndexName, failure.Failed.Message)
	}

	return status.Errorf(codes.InvalidArgument,
		"the datastore rejects event %d for what it contains, so the batch cannot be stored as sent: %s",
		failure.Failed.DocumentIndex, failure.Failed.Message)
}

// unstorable returns the failure when err is an answer about a document that a
// resend can never change, and nil otherwise: a duplicate on a unique index
// other than the idempotency index, or a document the datastore rejects for
// what it contains (see datastore.BulkDocumentError.Permanent), such as a
// check name longer than its PostgreSQL column. A duplicate on the idempotency
// index never gets here: the insert skips it as an event already stored.
func unstorable(err error) *datastore.BulkWriteFailure {
	var failure *datastore.BulkWriteFailure
	if errors.As(err, &failure) && (failure.Failed.Duplicate || failure.Failed.Permanent) {
		return failure
	}

	return nil
}

// insertHealthEvents writes one batch: in order, past the documents that
// already exist under the idempotency index, so a resent or retried batch
// stores only its missing events and a monitor's events land in the order it
// sent them; such duplicates count as success. With a batchKey the stored
// copies are keyed batchKey#index (the ring buffer loop, whose events carry no
// keys yet); without one the events keep the keys they arrived with. It
// reports the outcome (outcomeStored or outcomeDuplicate), or an error the
// caller may retry.
func (r *DatabaseStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
	batchKey string,
) (string, error) {
	return r.insertHealthEventsFrom(ctx, healthEvents, batchKey, 0)
}

// insertHealthEventsFrom writes the events of a batch from index from on, as
// insertHealthEvents does: each keeps the key of its position in the whole
// batch, and a failed document is reported at that position.
func (r *DatabaseStoreConnector) insertHealthEventsFrom(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
	batchKey string,
	from int,
) (string, error) {
	// An empty batch is a success before any datastore call: on MongoDB the
	// driver rejects an empty insert with ErrEmptySlice, which classifies as
	// retryable and would burn the whole retry budget.
	if len(healthEvents.GetEvents()) <= from {
		return outcomeStored, nil
	}

	// Prepare all documents for batch insertion
	ctx, span := tracing.StartSpan(ctx, "platform_connector.store.insert_health_events")
	defer span.End()

	healthEventWithStatusList := make([]any, 0, len(healthEvents.GetEvents())-from)
	traceID := span.SpanContext().TraceID().String()

	for offset, healthEvent := range healthEvents.GetEvents()[from:] {
		i := from + offset

		_, eventSpan := tracing.StartSpan(ctx, "platform_connector.process_event")

		// CRITICAL FIX: Clone the HealthEvent to avoid pointer reuse issues with gRPC buffers
		// Without this clone, the healthEvent pointer may point to reused gRPC buffer memory
		// that gets overwritten by subsequent requests, causing data corruption in MongoDB.
		// This manifests as events having wrong isfatal/ishealthy/message values.
		clonedHealthEvent := proto.Clone(healthEvent).(*protos.HealthEvent)

		if clonedHealthEvent.Metadata == nil {
			clonedHealthEvent.Metadata = make(map[string]string)
		}

		clonedHealthEvent.Metadata[tracing.MetadataKeyTraceID] = traceID

		if batchKey != "" {
			// Set on the stored copy only, so the events shared with the other
			// connectors are left as they are. An inbound key is overwritten:
			// it was copied from a stored document (a derived event) and would
			// collide with that document under the unique index.
			clonedHealthEvent.Metadata[datastore.HealthEventIdempotencyKeyMetadataField] = EventIdempotencyKey(batchKey, i)
		}

		slog.DebugContext(ctx, "Processing health event for insertion", "index", i, "nodeName", clonedHealthEvent.NodeName)

		tracing.AddHealthEventAttributes(eventSpan, clonedHealthEvent)

		healthEventWithStatusObj := model.HealthEventWithStatus{
			CreatedAt:   time.Now().UTC(),
			HealthEvent: clonedHealthEvent,
			HealthEventStatus: &protos.HealthEventStatus{
				UserPodsEvictionStatus: &protos.OperationStatus{},
				SpanIds: map[string]string{
					tracing.ServicePlatformConnector: tracing.SpanIDFromSpan(eventSpan),
				},
			},
		}
		healthEventWithStatusList = append(healthEventWithStatusList, healthEventWithStatusObj)

		eventSpan.End()
	}

	slog.DebugContext(ctx, "Inserting health events batch", "documentCount", len(healthEventWithStatusList))

	dbCtx, dbSpan := tracing.StartSpan(ctx, "platform_connector.db.insert")
	defer dbSpan.End()

	// In order, and past a document that already exists under the idempotency
	// index. Inserts, never updates, so MongoDB's change streams see INSERT
	// operations. The insert is not atomic: it stops at the first failure and
	// the documents before it stay stored, which the keys make harmless.
	result, err := r.databaseClient.InsertManyIdempotent(dbCtx, healthEventWithStatusList)
	if err != nil {
		var failure *datastore.BulkWriteFailure
		if errors.As(err, &failure) {
			// The client counts from the first document it was given.
			failure.Failed.DocumentIndex += from
		}

		slog.ErrorContext(ctx, "Insert failed", "error", err)
		tracing.RecordError(dbSpan, err)
		dbSpan.SetAttributes(
			attribute.String("platform_connector.error.type", "insert_many_failed"),
			attribute.String("platform_connector.error.message", err.Error()),
		)

		return "", fmt.Errorf("insertMany failed: %w", err)
	}

	if result != nil && result.DuplicateCount > 0 {
		// Documents already stored under the idempotency index are a resent
		// batch: exactly the success the key is for.
		if len(result.InsertedIDs) > 0 {
			// A resend after a partial write: the missing events are now
			// stored, so this is a store, not a pure replay.
			slog.InfoContext(ctx, "Resent batch stored its missing events",
				"insertedCount", len(result.InsertedIDs),
				"duplicateCount", result.DuplicateCount)
			dbSpan.SetAttributes(attribute.String("platform_connector.store.status", "partial_resend"))

			return outcomeStored, nil
		}

		slog.InfoContext(ctx, "Resent batch detected, events already stored",
			"duplicateCount", result.DuplicateCount)
		dbSpan.SetAttributes(attribute.String("platform_connector.store.status", "duplicate"))

		return outcomeDuplicate, nil
	}

	slog.DebugContext(ctx, "InsertMany completed successfully")

	return outcomeStored, nil
}
