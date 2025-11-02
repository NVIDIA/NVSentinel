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

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/factory"

	"github.com/google/uuid"
)

type DatabaseStoreConnector struct {
	// databaseClient is the database-agnostic client
	databaseClient client.DatabaseClient
	// resourceSinkClients are client for pushing data to the resource count sink
	ringBuffer *ringbuffer.RingBuffer
	nodeName   string
}

func new(
	databaseClient client.DatabaseClient,
	ringBuffer *ringbuffer.RingBuffer,
	nodeName string,
) *DatabaseStoreConnector {
	return &DatabaseStoreConnector{
		databaseClient: databaseClient,
		ringBuffer:     ringBuffer,
		nodeName:       nodeName,
	}
}

func InitializeDatabaseStoreConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	clientCertMountPath string) (*DatabaseStoreConnector, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME is not set")
	}

	// Create database client factory using store-client-sdk
	clientFactory, err := createClientFactory(clientCertMountPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client factory: %w", err)
	}

	// Create database client
	databaseClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	slog.Info("Successfully initialized database store connector")

	return new(databaseClient, ringbuffer, nodeName), nil
}

func createClientFactory(databaseClientCertMountPath string) (*factory.ClientFactory, error) {
	if databaseClientCertMountPath != "" {
		return factory.NewClientFactoryFromEnvWithCertPath(databaseClientCertMountPath)
	}

	return factory.NewClientFactoryFromEnv()
}

func (r *DatabaseStoreConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	// Build an in-memory cache of entity states from existing documents in the database
	for {
		select {
		case <-ctx.Done():
			slog.Info("Context canceled, exiting health metric processing loop")
			return
		default:
			healthEvents := r.ringBuffer.Dequeue()
			if healthEvents == nil || len(healthEvents.GetEvents()) == 0 {
				continue
			}

			err := r.insertHealthEvents(ctx, healthEvents)
			if err != nil {
				slog.Error("Error inserting health events", "error", err)
				r.ringBuffer.HealthMetricEleProcessingFailed(healthEvents)
			} else {
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
			}
		}
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
		slog.Warn("Error disconnecting database client (may already be disconnected)", "error", err)

		return nil
	}

	slog.Info("Successfully disconnected database client")

	return nil
}

func (r *DatabaseStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) error {
	callback := func(sessionContext client.SessionContext) error {
		for _, healthEvent := range healthEvents.GetEvents() {
			healthEventWithStatusObj := model.HealthEventWithStatus{
				CreatedAt:   time.Now().UTC(),
				HealthEvent: healthEvent,
			}

			// Insert each document individually using the database-agnostic interface
			filter := map[string]interface{}{
				"healthevent.nodename":           healthEvent.NodeName,
				"healthevent.generatedtimestamp": healthEvent.GeneratedTimestamp,
				"healthevent.checkname":          healthEvent.CheckName,
			}

			_, err := r.databaseClient.UpsertDocument(sessionContext, filter, healthEventWithStatusObj)
			if err != nil {
				return fmt.Errorf("upsert failed: %w", err)
			}
		}

		return nil
	}

	err := r.databaseClient.WithTransaction(ctx, callback)
	if err != nil {
		return fmt.Errorf("transaction failed: %w", err)
	}

	return nil
}

func GenerateRandomObjectID() string {
	return uuid.New().String()
}
