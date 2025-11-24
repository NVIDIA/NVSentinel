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

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/initializer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// dataStoreAdapter adapts client.DatabaseClient to queue.DataStore
type dataStoreAdapter struct {
	client.DatabaseClient
}

func (d *dataStoreAdapter) FindDocument(ctx context.Context, filter interface{},
	options *client.FindOneOptions) (client.SingleResult, error) {
	return d.FindOne(ctx, filter, options)
}

func (d *dataStoreAdapter) FindDocuments(ctx context.Context, filter interface{},
	options *client.FindOptions) (client.Cursor, error) {
	return d.Find(ctx, filter, options)
}

func main() {
	logger.SetDefaultStructuredLogger("node-drainer", version)
	slog.Info("Starting node-drainer", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Node drainer module exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	// Register database certificate flags using common package
	certConfig := flags.RegisterDatabaseCertFlags()
	kubeconfigPath := flag.String("kubeconfig-path", "", "path to kubeconfig file")

	tomlConfigPath := flag.String("config-path", "/etc/config/config.toml",
		"path where the node drainer config file is present")

	dryRun := flag.Bool("dry-run", false, "flag to run node drainer module in dry-run mode")

	flag.Parse()

	// Resolve the certificate path using common logic
	databaseClientCertMountPath := certConfig.ResolveCertPath()

	slog.Info("Database client cert", "path", databaseClientCertMountPath)

	params := initializer.InitializationParams{
		DatabaseClientCertMountPath: databaseClientCertMountPath,
		KubeconfigPath:              *kubeconfigPath,
		TomlConfigPath:              *tomlConfigPath,
		MetricsPort:                 *metricsPort,
		DryRun:                      *dryRun,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to initialize components: %w", err)
	}

	// Informers must sync before processing events
	slog.Info("Starting Kubernetes informers")

	if err := components.Informers.Run(ctx); err != nil {
		return fmt.Errorf("failed to start informers: %w", err)
	}

	slog.Info("Kubernetes informers started and synced")

	slog.Info("Starting queue worker")
	components.QueueManager.Start(ctx)

	// Handle cold start - re-process any events that were in-progress during restart
	slog.Info("Handling cold start")

	if err := handleColdStart(ctx, components); err != nil {
		slog.Error("Cold start handling failed", "error", err)
	}

	slog.Info("Starting database event watcher")

	criticalError := make(chan error)
	startEventWatcher(ctx, components, criticalError)

	slog.Info("All components started successfully")

	srv, err := createMetricsServer(*metricsPort)
	if err != nil {
		return err
	}

	// Start server in errgroup alongside event watcher monitoring
	g, gCtx := errgroup.WithContext(ctx)

	// Start the metrics/health server
	startMetricsServer(g, gCtx, srv)

	// Monitor for critical errors or graceful shutdown signals.
	g.Go(func() error {
		select {
		case <-gCtx.Done():
			// Context was cancelled (SIGTERM/SIGINT or another goroutine failed)
			slog.Info("Context cancelled, initiating shutdown")
		case err := <-criticalError:
			// Critical component (event watcher) failed
			slog.Error("Critical component failure", "error", err)
			stop() // Cancel context to trigger shutdown of other components

			if shutdownErr := shutdownComponents(ctx, components); shutdownErr != nil {
				return fmt.Errorf("failed to close event watcher: %w", shutdownErr)
			}

			return fmt.Errorf("critical component failure: %w", err)
		}

		// Normal shutdown path (context cancelled without critical error)
		return shutdownComponents(ctx, components)
	})

	// Wait for both goroutines to finish
	return g.Wait()
}

// createMetricsServer creates and configures the metrics server
func createMetricsServer(metricsPort string) (server.Server, error) {
	portInt, err := strconv.Atoi(metricsPort)
	if err != nil {
		return nil, fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	return srv, nil
}

// startMetricsServer starts the metrics server in an errgroup
func startMetricsServer(g *errgroup.Group, gCtx context.Context, srv server.Server) {
	g.Go(func() error {
		slog.Info("Starting metrics server")

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})
}

// startEventWatcher starts the event watcher goroutine
func startEventWatcher(ctx context.Context, components *initializer.Components, criticalError chan<- error) {
	go func() {
		// Start the change stream watcher
		components.EventWatcher.Start(ctx)
		slog.Info("Event watcher started, consuming events")

		// Consume events from the change stream
		for event := range components.EventWatcher.Events() {
			// Preprocess and enqueue the event
			// This sets the initial status to InProgress and enqueues the event for processing
			if err := components.Reconciler.PreprocessAndEnqueueEvent(ctx, event); err != nil {
				// Don't send to criticalError - just log and continue processing other events
				slog.Error("Failed to preprocess and enqueue event", "error", err)
				continue
			}

			// Mark the event as processed (save resume token) AFTER successful preprocessing
			// Extract the resume token from the event to avoid race condition
			resumeToken := event.GetResumeToken()
			if err := components.EventWatcher.MarkProcessed(ctx, resumeToken); err != nil {
				// Don't send to criticalError - just log and continue
				slog.Error("Error updating resume token", "error", err)
			}
		}

		slog.Info("Event watcher stopped")
	}()
}

// handleColdStart re-processes events that were in-progress or quarantined during a restart
func handleColdStart(ctx context.Context, components *initializer.Components) error {
	slog.Info("Querying for events requiring processing")

	// Query for events that need processing:
	// 1. Events with StatusInProgress (actively being processed when we went down)
	// 2. Events that are Quarantined but haven't started processing yet (status is empty or NotStarted)
	// This handles cases where node-drainer was restarted after quarantine but before processing started
	filter := map[string]interface{}{
		"$or": []interface{}{
			// Case 1: Events that were in-progress
			map[string]interface{}{
				"healtheventstatus.userpodsevictionstatus.status": string(model.StatusInProgress),
			},
			// Case 2: Quarantined events that haven't been processed yet
			map[string]interface{}{
				"healtheventstatus.nodequarantined": string(model.Quarantined),
				"healtheventstatus.userpodsevictionstatus.status": map[string]interface{}{
					"$in": []interface{}{"", string(model.StatusNotStarted)},
				},
			},
			// Case 3: AlreadyQuarantined events that haven't been processed yet
			map[string]interface{}{
				"healtheventstatus.nodequarantined": string(model.AlreadyQuarantined),
				"healtheventstatus.userpodsevictionstatus.status": map[string]interface{}{
					"$in": []interface{}{"", string(model.StatusNotStarted)},
				},
			},
		},
	}

	// Use Find to get all events requiring processing
	cursor, err := components.DatabaseClient.Find(ctx, filter, nil)
	if err != nil {
		return fmt.Errorf("failed to query events for cold start: %w", err)
	}
	defer cursor.Close(ctx)

	var events []datastore.Event
	if err := cursor.All(ctx, &events); err != nil {
		return fmt.Errorf("failed to decode events for cold start: %w", err)
	}

	slog.Info("Found events to re-process", "count", len(events))

	// Re-process each event
	for _, event := range events {
		// Extract health event (datastore.Event is map[string]interface{})
		healthEvent, ok := event["healthevent"].(datastore.Event)
		if !ok {
			slog.Error("Failed to extract healthevent from cold start event")
			continue
		}

		nodeName, ok := healthEvent["nodename"].(string)
		if !ok {
			slog.Error("Failed to extract node name from cold start event")
			continue
		}

		// Create adapter to bridge interface differences
		dbAdapter := &dataStoreAdapter{DatabaseClient: components.DatabaseClient}
		if err := components.QueueManager.EnqueueEventGeneric(ctx, nodeName, event, dbAdapter); err != nil {
			slog.Error("Failed to enqueue cold start event", "error", err, "nodeName", nodeName)
		} else {
			slog.Info("Re-queued event from cold start", "nodeName", nodeName)
		}
	}

	slog.Info("Cold start processing completed")

	return nil
}

// shutdownComponents handles the shutdown of components
func shutdownComponents(ctx context.Context, components *initializer.Components) error {
	slog.Info("Shutting down node drainer")

	if errStop := components.EventWatcher.Close(ctx); errStop != nil {
		return fmt.Errorf("failed to close event watcher: %w", errStop)
	}

	components.QueueManager.Shutdown()
	slog.Info("Node drainer stopped")

	return nil
}
