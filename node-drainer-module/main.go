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

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/initializer"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("node-drainer-module", version)
	slog.Info("Starting node-drainer-module", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Node drainer module exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	mongoClientCertMountPath := flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	kubeconfigPath := flag.String("kubeconfig-path", "", "path to kubeconfig file")

	tomlConfigPath := flag.String("config-path", "/etc/config/config.toml",
		"path where the node drainer config file is present")

	dryRun := flag.Bool("dry-run", false, "flag to run node drainer module in dry-run mode")

	flag.Parse()

	slog.Info("Mongo client cert", "path", *mongoClientCertMountPath)

	params := initializer.InitializationParams{
		MongoClientCertMountPath: *mongoClientCertMountPath,
		KubeconfigPath:           *kubeconfigPath,
		TomlConfigPath:           *tomlConfigPath,
		MetricsPort:              *metricsPort,
		DryRun:                   *dryRun,
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

	slog.Info("Starting MongoDB event watcher")

	criticalError := make(chan error)

	go func() {
		if err := components.EventWatcher.Start(ctx); err != nil {
			slog.Error("Event watcher failed", "error", err)
			criticalError <- err
		}
	}()

	slog.Info("All components started successfully")

	// Parse the metrics port
	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	// Create the server
	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	// Start server in errgroup alongside event watcher monitoring
	g, gCtx := errgroup.WithContext(ctx)

	// Start the metrics/health server.
	// Metrics server failures are logged but do NOT terminate the service.
	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

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

			slog.Info("Shutting down node drainer")

			if errStop := components.EventWatcher.Stop(); errStop != nil {
				return fmt.Errorf("failed to stop event watcher: %w", errStop)
			}

			components.QueueManager.Shutdown()
			slog.Info("Node drainer stopped")

			return fmt.Errorf("critical component failure: %w", err)
		}

		// Normal shutdown path (context cancelled without critical error)
		slog.Info("Shutting down node drainer")

		if errStop := components.EventWatcher.Stop(); errStop != nil {
			return fmt.Errorf("failed to stop event watcher: %w", errStop)
		}

		components.QueueManager.Shutdown()
		slog.Info("Node drainer stopped")

		return nil
	})

	// Wait for both goroutines to finish
	return g.Wait()
}
