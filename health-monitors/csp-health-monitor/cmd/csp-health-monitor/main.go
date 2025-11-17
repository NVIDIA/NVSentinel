// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/csp"
	awsclient "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/csp/aws"
	gcpclient "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/csp/gcp"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	defaultConfigPath       = "/etc/config/config.toml"
	defaultDatabaseCertPath = "/etc/ssl/database-client"
	defaultKubeconfig       = ""
	defaultMetricsPort      = "2112"
	eventChannelSize        = 100
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// main is the entry point for the CSP Health Monitor application.
func main() {
	logger.SetDefaultStructuredLogger("csp-health-monitor", version)
	slog.Info("Starting csp-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("CSP Health Monitor exited with error", "error", err)
		os.Exit(1)
	}
}

// startActiveMonitorAndLog starts the provided CSP monitor in a new goroutine
// and logs its lifecycle and any runtime errors.
func startActiveMonitorAndLog(
	ctx context.Context,
	wg *sync.WaitGroup,
	activeMonitor csp.Monitor,
	eventChan chan<- model.MaintenanceEvent,
) {
	if activeMonitor == nil {
		// If no monitor is configured, the application cannot perform its core
		// function.
		slog.Error("No active CSP monitor configured or enabled. Application cannot start.")

		return
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		slog.Info("Starting active monitor", "name", activeMonitor.GetName())

		monitorErr := activeMonitor.StartMonitoring(ctx, eventChan)
		if monitorErr != nil {
			if !errors.Is(monitorErr, context.Canceled) && !errors.Is(monitorErr, context.DeadlineExceeded) {
				metrics.CSPMonitorErrors.WithLabelValues(string(activeMonitor.GetName()), "runtime_error").Inc()
				slog.Error("Monitor stopped with critical error", "name", activeMonitor.GetName(), "error", monitorErr)
			} else {
				slog.Info("Monitor shut down due to context", "name", activeMonitor.GetName(), "error", monitorErr)
			}
		} else {
			slog.Info("Monitor shut down cleanly", "name", activeMonitor.GetName())
		}
	}()
}

func run() error {
	configPath := flag.String("config", defaultConfigPath, "Path to the TOML configuration file.")
	metricsPort := flag.String("metrics-port", defaultMetricsPort, "Port to expose Prometheus metrics on.")
	kubeconfig := flag.String(
		"kubeconfig",
		defaultKubeconfig,
		"Path to a kubeconfig file. Only required if running out-of-cluster.",
	)
	databaseClientCertMountPath := flag.String(
		"database-client-cert-mount-path",
		defaultDatabaseCertPath,
		"Directory where database client tls.crt, tls.key, and ca.crt are mounted.",
	)

	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration from %s: %w", *configPath, err)
	}

	effectiveKubeconfigPath := *kubeconfig

	// Create context with signal handling for graceful shutdown.
	// This context will be cancelled when SIGINT or SIGTERM is received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := datastore.NewStore(ctx, databaseClientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to initialize datastore: %w", err)
	}

	slog.Info("Datastore initialized successfully.")

	eventChan := make(chan model.MaintenanceEvent, eventChannelSize)
	// Processor is lightweight; it already encapsulates required dependencies.
	eventProcessor, err := eventpkg.NewProcessor(cfg, store)
	if err != nil {
		return fmt.Errorf("failed to initialize event processor: %w", err)
	}

	slog.Info("Event processor initialized successfully.")

	activeMonitor := initActiveMonitor(
		ctx,
		cfg,
		effectiveKubeconfigPath,
		store,
	) // Pass kubeconfigPath for clients to init their own k8s clients

	// Parse the metrics port
	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	// Create common HTTP server with metrics and health endpoints
	server := srv.NewServer(
		srv.WithPort(portInt),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
	)

	// Use errgroup to manage concurrent goroutines with proper cancellation
	g, gCtx := errgroup.WithContext(ctx)

	// Start the metrics/health server.
	// Metrics server failures are logged but do NOT terminate the service.
	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := server.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	// Start the CSP monitor
	g.Go(func() error {
		var wg sync.WaitGroup

		startActiveMonitorAndLog(gCtx, &wg, activeMonitor, eventChan)
		wg.Wait()
		slog.Info("Active monitor stopped.")

		return nil
	})

	// Start the event processor
	g.Go(func() error {
		runEventProcessorLoop(gCtx, eventChan, eventProcessor)
		slog.Info("Event processing loop stopped.")

		return nil
	})

	slog.Info("CSP Health Monitor (Main Container) components started successfully.")

	// Wait for all goroutines to finish
	if err := g.Wait(); err != nil {
		return fmt.Errorf("service error: %w", err)
	}

	slog.Info("CSP Health Monitor (Main Container) shut down completed.")

	return nil
}

// initActiveMonitor instantiates the appropriate CSP monitor (GCP/AWS) based on
// the supplied configuration. It returns nil when no CSP is enabled.
func initActiveMonitor(
	ctx context.Context,
	cfg *config.Config,
	kubeconfigPath string,
	store datastore.Store,
) csp.Monitor {
	if cfg.GCP.Enabled {
		slog.Info("GCP configuration is enabled.")

		gcpMonitor, err := gcpclient.NewClient(ctx, cfg.GCP, cfg.ClusterName, kubeconfigPath, store)
		if err != nil {
			metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPGCP), "init_error").Inc()
			slog.Error("Failed to initialize GCP monitor. GCP will not be monitored.", "error", err)

			return nil
		}

		slog.Info("GCP monitor initialized", "project", cfg.GCP.TargetProjectID)

		return gcpMonitor
	}

	if cfg.AWS.Enabled {
		slog.Info("AWS configuration is enabled.")

		awsMonitor, err := awsclient.NewClient(ctx, cfg.AWS, cfg.ClusterName, kubeconfigPath, store)
		if err != nil {
			metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "init_error").Inc()
			slog.Error("Failed to initialize AWS monitor. AWS will not be monitored.", "error", err)

			return nil
		}

		slog.Info("AWS monitor initialized",
			"account", cfg.AWS.AccountID,
			"region", cfg.AWS.Region)

		return awsMonitor
	}

	slog.Info("No CSP is explicitly enabled in the configuration (GCP or AWS).")

	return nil
}

// runEventProcessorLoop consumes normalized events from eventChan and hands
// them to the datastore-backed Processor until the context is cancelled.
func runEventProcessorLoop(
	ctx context.Context,
	eventChan <-chan model.MaintenanceEvent,
	processor *eventpkg.Processor,
) {
	slog.Info("Starting event processing worker loop (main monitor)...")

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping event processing worker loop (main monitor).")
			return
		case receivedEvent, ok := <-eventChan:
			if !ok {
				slog.Info("Event channel closed, stopping event processing worker loop (main monitor).")
				return
			}

			metrics.MainEventsReceived.WithLabelValues(string(receivedEvent.CSP)).Inc()
			slog.Info("Processor received event",
				"eventID", receivedEvent.EventID,
				"csp", receivedEvent.CSP,
				"node", receivedEvent.NodeName,
				"status", receivedEvent.Status)

			start := time.Now()
			err := processor.ProcessEvent(ctx, &receivedEvent)
			duration := time.Since(start).Seconds()
			metrics.MainEventProcessingDuration.WithLabelValues(string(receivedEvent.CSP)).Observe(duration)

			if err != nil {
				metrics.MainProcessingErrors.WithLabelValues(string(receivedEvent.CSP), "process_event").Inc()
				slog.Error(
					"Error processing event",
					"eventID", receivedEvent.EventID,
					"node", receivedEvent.NodeName,
					"error", err,
				)
			} else {
				metrics.MainEventsProcessedSuccess.WithLabelValues(string(receivedEvent.CSP)).Inc()
				slog.Debug("Successfully processed event",
					"eventID", receivedEvent.EventID,
				)
			}
		}
	}
}
