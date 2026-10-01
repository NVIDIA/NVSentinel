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

			outcome, err := r.insertHealthEvents(batchCtx, healthEvents, r.batchKey(queuedHealthEvents))
			if err != nil {
				retryCount := r.ringBuffer.NumRequeues(queuedHealthEvents)

				tracing.RecordError(span, err)
				span.SetAttributes(
					attribute.String("platform_connector.store.error", err.Error()),
					attribute.Int("platform_connector.store.retry_count", retryCount),
					attribute.Int("platform_connector.store.max_retries", r.maxRetries),
				)

				if retryCount < r.maxRetries {
					slog.WarnContext(batchCtx, "Error inserting health events, will retry with exponential backoff",
						"error", err,
						"retryCount", retryCount,
						"maxRetries", r.maxRetries,
						"eventCount", eventCount)

					r.ringBuffer.AddRateLimited(queuedHealthEvents)
				} else {
					span.SetAttributes(attribute.String("platform_connector.store.status", "failed"))
					slog.ErrorContext(batchCtx, "Max retries exceeded, dropping health events permanently",
						"error", err,
						"retryCount", retryCount,
						"maxRetries", r.maxRetries,
						"eventCount", eventCount,
						"firstEventNodeName", healthEvents.GetEvents()[0].GetNodeName(),
						"firstEventCheckName", healthEvents.GetEvents()[0].GetCheckName())
					r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
				}
			} else {
				batchesWritten.WithLabelValues(outcome).Inc()
				span.SetAttributes(attribute.String("platform_connector.store.status", "inserted"))
				r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
			}

			span.End()
		}
	}
}

// batchKey returns the idempotency key prefix of a queued batch, chosen on
// its first write attempt and kept on the item: a batch requeued after a
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

// terminalIfDocumentError turns the one answer about a document that a resend
// can never change, a duplicate on a unique index other than the idempotency
// index, into a status the caller does not retry. Every other failure is left
// as it is and answered with a retryable status: a document error can also be
// the server stepping down in the middle of the batch, which a resend cures.
func terminalIfDocumentError(err error) error {
	var failure *datastore.BulkWriteFailure
	if !errors.As(err, &failure) || !failure.Failed.Duplicate {
		return err
	}

	return status.Errorf(codes.InvalidArgument,
		"event %d duplicates a document already stored under index %q, so the batch cannot be stored as sent: %s",
		failure.Failed.DocumentIndex, failure.Failed.IndexName, failure.Failed.Message)
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
	// An empty batch is a success before any datastore call: on MongoDB the
	// driver rejects an empty insert with ErrEmptySlice, which classifies as
	// retryable and would burn the whole retry budget.
	if len(healthEvents.GetEvents()) == 0 {
		return outcomeStored, nil
	}

	// Prepare all documents for batch insertion
	ctx, span := tracing.StartSpan(ctx, "platform_connector.store.insert_health_events")
	defer span.End()

	healthEventWithStatusList := make([]any, 0, len(healthEvents.GetEvents()))
	traceID := span.SpanContext().TraceID().String()

	for i, healthEvent := range healthEvents.GetEvents() {
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

func GenerateRandomObjectID() string {
	return uuid.New().String()
}
