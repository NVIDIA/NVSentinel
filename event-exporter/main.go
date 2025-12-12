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

	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/initializer"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	slog.Info("Health Events Exporter starting", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/config/config.toml", "Path to configuration file")
	metricsPort := flag.String("metrics-port", "2112", "Port to expose Prometheus metrics and health endpoints")
	oidcSecretPath := flag.String("oidc-secret-path", "/var/secrets/oidc-client-secret", "Path to OIDC client secret file")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	params := initializer.Params{
		ConfigPath:     *configPath,
		OIDCSecretPath: *oidcSecretPath,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to initialize components: %w", err)
	}

	httpServer, err := createMetricsServer(*metricsPort)
	if err != nil {
		return fmt.Errorf("failed to create metrics server: %w", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", *metricsPort)

		if err := httpServer.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		slog.Info("Starting event export")

		if err := components.Exporter.Run(gCtx); err != nil {
			if gCtx.Err() == context.Canceled {
				slog.Info("Exporter stopped gracefully")
			} else {
				slog.Error("Exporter failed", "error", err)
			}

			return fmt.Errorf("exporter failed: %w", err)
		}

		return nil
	})

	err = g.Wait()

	if closeErr := shutdownComponents(components); closeErr != nil {
		slog.Error("Failed to shutdown components", "error", closeErr)

		if err == nil {
			return closeErr
		}
	}

	return err
}

func createMetricsServer(metricsPort string) (server.Server, error) {
	portInt, err := strconv.Atoi(metricsPort)
	if err != nil {
		return nil, fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithSimpleHealth(),
		server.WithHandler("/metrics", promhttp.Handler()),
	)

	return srv, nil
}

func shutdownComponents(components *initializer.Components) error {
	slog.Info("Closing datastore bundle to save resume token")

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()

	if err := components.DatastoreBundle.Close(closeCtx); err != nil {
		return fmt.Errorf("failed to close datastore bundle: %w", err)
	}

	slog.Info("Shutdown complete")

	return nil
}
