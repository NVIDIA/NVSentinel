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
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	metrics "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/initializer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
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
	logger.SetDefaultStructuredLoggerWithTraceCorrelation("node-drainer", version)
	slog.Info("Starting node-drainer", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("node-drainer"); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	// Initialize OpenTelemetry tracing
	if err := tracing.InitTracing(tracing.ServiceNodeDrainer); err != nil {
		slog.Warn("Failed to initialize tracing", "error", err)
	}

	runErr := run()

	tracingCtx, tracingCancel := context.WithTimeout(context.Background(), 5*time.Second)

	if err := tracing.ShutdownTracing(tracingCtx); err != nil {
		slog.Warn("Failed to shutdown tracing", "error", err)
	}

	tracingCancel()

	if runErr != nil {
		slog.Error("Node drainer module exited with error", "error", runErr)

		if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
			slog.Warn("Failed to close audit logger", "error", closeErr)
		}

		os.Exit(1)
	}

	if err := auditlogger.CloseAuditLogger(); err != nil {
		slog.Warn("Failed to close audit logger", "error", err)
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

	ff := metrics.NewRegistry("node-drainer")
	ff.Set("dry_run", *dryRun)

	// Resolve the certificate path using common logic
	databaseClientCertMountPath := certConfig.ResolveCertPath()

	slog.InfoContext(ctx, "Database client cert", "path", databaseClientCertMountPath)

	params := initializer.InitializationParams{
		DatabaseClientCertMountPath: databaseClientCertMountPath,
		KubeconfigPath:              *kubeconfigPath,
		TomlConfigPath:              *tomlConfigPath,
		MetricsPort:                 *metricsPort,
		DryRun:                      *dryRun,
	}

	// Create and start the health/metrics server BEFORE the potentially slow MongoDB
	// initialization. This ensures Kubernetes liveness probes get HTTP 200 responses
	// immediately, preventing the pod from being killed during initialization.
	srv, err := createMetricsServer(*metricsPort)
	if err != nil {
		return err
	}

	g, gCtx := errgroup.WithContext(ctx)

	startMetricsServer(g, gCtx, srv)

	// Initialize components (may block on MongoDB connectivity / change stream setup)
	components, err := initializer.InitializeAll(gCtx, params)
	if err != nil {
		return fmt.Errorf("failed to initialize components: %w", err)
	}

	ff.Set("custom_drain", components.CustomDrainEnabled)

	// Informers must sync before processing events
	slog.InfoContext(gCtx, "Starting Kubernetes informers")

	if err := components.Informers.Run(gCtx); err != nil {
		return fmt.Errorf("failed to start informers: %w", err)
	}

	slog.InfoContext(gCtx, "Kubernetes informers started and synced")

	slog.InfoContext(gCtx, "Starting queue worker")
	components.QueueManager.Start(gCtx)

	// Handle cold start - re-process any events that were in-progress during restart
	slog.InfoContext(gCtx, "Handling cold start")

	if err := handleColdStart(gCtx, components); err != nil {
		slog.ErrorContext(gCtx, "Cold start handling failed", "error", err)
	}

	slog.InfoContext(gCtx, "Starting database event watcher")

	criticalError := make(chan error)
	startEventWatcher(gCtx, components, criticalError)

	slog.InfoContext(gCtx, "All components started successfully")

	// Monitor for critical errors or graceful shutdown signals.
	g.Go(func() error {
		select {
		case <-gCtx.Done():
			// Context was cancelled (SIGTERM/SIGINT or another goroutine failed)
			slog.InfoContext(gCtx, "Context cancelled, initiating shutdown")
		case err := <-criticalError:
			slog.ErrorContext(gCtx, "Critical component failure", "error", err)
			stop() // Cancel context to trigger shutdown of other components

			if shutdownErr := shutdownComponents(ctx, components); shutdownErr != nil {
				return fmt.Errorf("failed to close event watcher: %w", shutdownErr)
			}

			return fmt.Errorf("critical component failure: %w", err)
		}

		// Normal shutdown path (context cancelled without critical error)
		return shutdownComponents(ctx, components)
	})

	// Wait for all goroutines to finish
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
		slog.InfoContext(gCtx, "Starting metrics server")

		if err := srv.Serve(gCtx); err != nil {
			slog.ErrorContext(gCtx, "Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})
}

// startEventWatcher starts the event watcher goroutine
func startEventWatcher(ctx context.Context, components *initializer.Components, criticalError chan<- error) {
	go func() {
		if components.EventWatcher == nil {
			slog.WarnContext(ctx, "No event watcher available")
			<-ctx.Done()

			return
		}

		// Start the change stream watcher
		components.EventWatcher.Start(ctx)
		slog.InfoContext(ctx, "Event watcher started, consuming events")

		// Consume events from the change stream
		for event := range components.EventWatcher.Events() {
			// Preprocess and enqueue the event
			// This sets the initial status to InProgress and enqueues the event for processing
			if err := components.Reconciler.PreprocessAndEnqueueEvent(ctx, event); err != nil {
				// Don't send to criticalError - just log and continue processing other events
				slog.ErrorContext(ctx, "Failed to preprocess and enqueue event", "error", err)
				continue
			}

			// Mark the event as processed (save resume token) AFTER successful preprocessing
			// Extract the resume token from the event to avoid race condition
			resumeToken := event.GetResumeToken()
			if err := components.EventWatcher.MarkProcessed(ctx, resumeToken); err != nil {
				// Don't send to criticalError - just log and continue
				slog.ErrorContext(ctx, "Error updating resume token", "error", err)
			}
		}

		// The event channel closed. If the context is still active, this means the
		// change stream died unexpectedly (e.g., MongoDB error). Signal a critical
		// failure so the pod exits and Kubernetes restarts it.
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "Event watcher channel closed unexpectedly, event processing has stopped")

			criticalError <- fmt.Errorf("event watcher channel closed unexpectedly")
		} else {
			slog.InfoContext(ctx, "Event watcher stopped")
		}
	}()
}

const coldStartBatchSize = 1000

// handleColdStart re-processes events that were in-progress or quarantined during a restart.
// Events are fetched in bounded batches via FindHealthEventsByQueryBatched to prevent
// unbounded memory usage. All matching events are loaded (not just latest per node)
// because a single node can have multiple concurrent partial drains.
func handleColdStart(ctx context.Context, components *initializer.Components) error {
	slog.InfoContext(ctx, "Querying for events requiring processing")

	q := query.New().Build(
		query.Or(
			// Events that were in-progress
			query.Eq("healtheventstatus.userpodsevictionstatus.status", string(model.StatusInProgress)),

			// Quarantined events that haven't been processed yet
			query.And(
				query.Eq("healtheventstatus.nodequarantined", string(model.Quarantined)),
				query.In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", string(model.StatusNotStarted)}),
			),

			// AlreadyQuarantined events that haven't been processed yet
			query.And(
				query.Eq("healtheventstatus.nodequarantined", string(model.AlreadyQuarantined)),
				query.In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", string(model.StatusNotStarted)}),
			),
		),
	)

	healthStore := components.DataStore.HealthEventStore()
	dbAdapter := &dataStoreAdapter{DatabaseClient: components.DatabaseClient}

	err := healthStore.FindHealthEventsByQueryBatched(ctx, q, coldStartBatchSize,
		func(batch []datastore.HealthEventWithStatus) error {
			slog.Info("Processing cold start batch", "count", len(batch))

			for _, he := range batch {
				event := he.RawEvent
				if len(event) == 0 {
					slog.ErrorContext(ctx, "RawEvent is empty, skipping cold start event")

					continue
				}

				parsedEvent, err := eventutil.ParseHealthEventFromEvent(event)
				if err != nil {
					slog.ErrorContext(ctx, "Failed to parse health event from cold start event", "error", err)

					continue
				}

				if parsedEvent.HealthEvent == nil {
					slog.ErrorContext(ctx, "Health event is nil in cold start event")

					continue
				}

				nodeName := parsedEvent.HealthEvent.GetNodeName()
				if nodeName == "" {
					slog.ErrorContext(ctx, "Node name is empty in cold start event")

					continue
				}

				documentID, err := utils.ExtractDocumentIDNative(event)
				if err != nil {
					slog.ErrorContext(ctx, "Failed to extract document ID from cold start event", "error", err)

					continue
				}

				if enqueueErr := components.QueueManager.EnqueueEventGeneric(
					ctx, nodeName, event, dbAdapter, healthStore, documentID); enqueueErr != nil {
					slog.Error("Failed to enqueue cold start event", "error", enqueueErr, "nodeName", nodeName)
				} else {
					slog.InfoContext(ctx, "Re-queued event from cold start", "nodeName", nodeName)
				}
			}

			return nil
		})
	if err != nil {
		return fmt.Errorf("failed to process cold start events: %w", err)
	}

	slog.InfoContext(ctx, "Cold start processing completed")

	return nil
}

// shutdownComponents handles the shutdown of components
func shutdownComponents(ctx context.Context, components *initializer.Components) error {
	slog.InfoContext(ctx, "Shutting down node drainer")

	if components.EventWatcher != nil {
		if errStop := components.EventWatcher.Close(ctx); errStop != nil {
			return fmt.Errorf("failed to close event watcher: %w", errStop)
		}
	}

	components.QueueManager.Shutdown()
	slog.InfoContext(ctx, "Node drainer stopped")

	return nil
}
