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
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
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

// Outcomes of a successful batch insert.
const (
	// OutcomeStored means every document of the batch was inserted.
	OutcomeStored = "stored"
	// OutcomeDuplicate means the only failures were duplicate-key violations of
	// the idempotency index, i.e. a replayed batch whose events already exist.
	OutcomeDuplicate = "duplicate"
)

type DatabaseStoreConnector struct {
	// databaseClient is the database-agnostic client
	databaseClient client.DatabaseClient
	// resourceSinkClients are client for pushing data to the resource count sink
	ringBuffer *ringbuffer.RingBuffer
	maxRetries int
}

func InitializeDatabaseStoreConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	clientCertMountPath string, maxRetries int) (*DatabaseStoreConnector, error) {
	connector := &DatabaseStoreConnector{
		ringBuffer: ringbuffer,
		maxRetries: maxRetries,
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

// EnsureIdempotencyIndex builds a short-lived database client and idempotently
// creates the unique partial idempotency index on the health events
// collection. It backs the PC_MODE=ensure-idempotency-index Job; the caller
// owns the bound of one attempt, and the Job retries a failed attempt.
func EnsureIdempotencyIndex(ctx context.Context, clientCertMountPath string) error {
	clientFactory, err := createClientFactory(clientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to create database client factory: %w", err)
	}

	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create database client: %w", err)
	}

	defer func() {
		if closeErr := databaseClient.Close(ctx); closeErr != nil {
			slog.WarnContext(ctx, "Error closing database client after index ensure", "error", closeErr)
		}
	}()

	if err := databaseClient.EnsureHealthEventIdempotencyIndex(ctx); err != nil {
		return fmt.Errorf("failed to ensure idempotency index: %w", err)
	}

	return nil
}

// VerifyIdempotencyIndex reports whether the idempotency index exists with the
// expected full definition and a completed build. The deployment platform
// connector gates its readiness on this, so no client can write before the
// index Job has completed.
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
	// Build an in-memory cache of entity states from existing documents in the database
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

			_, err := r.insertHealthEvents(batchCtx, healthEvents, false)
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
				span.SetAttributes(attribute.String("platform_connector.store.status", "inserted"))
				r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
			}

			span.End()
		}
	}
}

func (r *DatabaseStoreConnector) ShutdownRingBuffer(ctx context.Context) {
	if r.ringBuffer != nil {
		slog.InfoContext(ctx, "Shutting down database store connector ring buffer with drain")
		r.ringBuffer.ShutDownHealthMetricQueue()
		slog.InfoContext(ctx, "Database store connector ring buffer drained successfully")
	}
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

// InsertBatch inserts one batch and reports its outcome (OutcomeStored or
// OutcomeDuplicate), or an error the caller may retry. The deployment platform
// connector calls it from inside the request instead of through the queue.
func (r *DatabaseStoreConnector) InsertBatch(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) (string, error) {
	return r.insertHealthEvents(ctx, healthEvents, true)
}

// insertHealthEvents writes one batch. With idempotent set (the deployment
// platform connector) the insert is ordered, resumes past duplicates on the
// idempotency index, and such a duplicate counts as success; otherwise it is
// today's single ordered InsertMany, unchanged for the node-local DaemonSet.
func (r *DatabaseStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
	idempotent bool,
) (string, error) {
	// An empty batch is a success before any datastore call: on MongoDB the
	// driver rejects an empty insert with ErrEmptySlice, which classifies as
	// retryable and would burn the whole retry budget.
	if len(healthEvents.GetEvents()) == 0 {
		return OutcomeStored, nil
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

		if !idempotent {
			// Only the deployment platform connector stamps this key. One that
			// arrives on the socket path was copied from a stored document (a
			// derived event) and would collide with it under the unique index.
			delete(clonedHealthEvent.Metadata, datastore.HealthEventIdempotencyKeyMetadataField)
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

	var (
		result *client.InsertManyResult
		err    error
	)

	if idempotent {
		// In order, and past a document that already exists under the
		// idempotency index, so a resent batch inserts only its missing events
		// and a monitor's events land in the order it sent them.
		result, err = r.databaseClient.InsertManyIdempotent(dbCtx, healthEventWithStatusList)
	} else {
		// Insert all documents in a single batch operation. This ensures
		// MongoDB generates INSERT operations (not UPDATE) for change streams.
		// The insert is ordered, not atomic: it stops at the first failure and
		// the documents before it stay stored, and without an idempotency key
		// a resend of the batch stores those again.
		result, err = r.databaseClient.InsertMany(dbCtx, healthEventWithStatusList)
	}

	if err != nil {
		slog.ErrorContext(ctx, "Insert failed", "error", err, "idempotent", idempotent)
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

			return OutcomeStored, nil
		}

		slog.InfoContext(ctx, "Resent batch detected, events already stored",
			"duplicateCount", result.DuplicateCount)
		dbSpan.SetAttributes(attribute.String("platform_connector.store.status", "duplicate"))

		return OutcomeDuplicate, nil
	}

	slog.DebugContext(ctx, "InsertMany completed successfully")

	return OutcomeStored, nil
}

func GenerateRandomObjectID() string {
	return uuid.New().String()
}
