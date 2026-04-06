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
	"os"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/proto"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

const (
	ServicePlatformConnector = "platform-connector"
)

type DatabaseStoreConnector struct {
	// databaseClient is the database-agnostic client
	databaseClient client.DatabaseClient
	// resourceSinkClients are client for pushing data to the resource count sink
	ringBuffer *ringbuffer.RingBuffer
	nodeName   string
	maxRetries int
}

func new(
	databaseClient client.DatabaseClient,
	ringBuffer *ringbuffer.RingBuffer,
	nodeName string,
	maxRetries int,
) *DatabaseStoreConnector {
	return &DatabaseStoreConnector{
		databaseClient: databaseClient,
		ringBuffer:     ringBuffer,
		nodeName:       nodeName,
		maxRetries:     maxRetries,
	}
}

func InitializeDatabaseStoreConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	clientCertMountPath string, maxRetries int) (*DatabaseStoreConnector, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME is not set")
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

	slog.InfoContext(ctx, "Successfully initialized database store connector", "maxRetries", maxRetries)

	return new(databaseClient, ringbuffer, nodeName, maxRetries), nil
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

			err := r.insertHealthEvents(batchCtx, healthEvents)
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

func (r *DatabaseStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) error {
	// Prepare all documents for batch insertion
	ctx, span := tracing.StartSpan(ctx, "platform_connector.store.insert_health_events")
	defer span.End()

	healthEventWithStatusList := make([]interface{}, 0, len(healthEvents.GetEvents()))
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

		slog.DebugContext(ctx, "Processing health event for insertion", "index", i, "nodeName", clonedHealthEvent.NodeName)

		tracing.AddHealthEventAttributes(eventSpan, clonedHealthEvent)

		healthEventWithStatusObj := model.HealthEventWithStatus{
			CreatedAt:   time.Now().UTC(),
			HealthEvent: clonedHealthEvent,
			HealthEventStatus: &protos.HealthEventStatus{
				UserPodsEvictionStatus: &protos.OperationStatus{},
				SpanIds: map[string]string{
					ServicePlatformConnector: tracing.SpanIDFromSpan(eventSpan),
				},
			},
		}
		healthEventWithStatusList = append(healthEventWithStatusList, healthEventWithStatusObj)

		eventSpan.End()
	}

	slog.DebugContext(ctx, "Inserting health events batch", "documentCount", len(healthEventWithStatusList))

	dbCtx, dbSpan := tracing.StartSpan(ctx, "platform_connector.db.insert")
	defer dbSpan.End()

	// Insert all documents in a single batch operation
	// This ensures MongoDB generates INSERT operations (not UPDATE) for change streams
	// Note: InsertMany is already atomic - either all documents are inserted or none are
	_, err := r.databaseClient.InsertMany(dbCtx, healthEventWithStatusList)
	if err != nil {
		slog.ErrorContext(ctx, "InsertMany failed", "error", err)
		tracing.RecordError(dbSpan, err)
		dbSpan.SetAttributes(
			attribute.String("platform_connector.error.type", "insert_many_failed"),
			attribute.String("platform_connector.error.message", err.Error()),
		)

		return fmt.Errorf("insertMany failed: %w", err)
	}

	slog.DebugContext(ctx, "InsertMany completed successfully")

	return nil
}

func GenerateRandomObjectID() string {
	return uuid.New().String()
}
