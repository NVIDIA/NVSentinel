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

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/initializer"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("fault-remediation", version)
	slog.Info("Starting fault-remediation", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("fault-remediation"); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)

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
	metricsPort, kubeconfigPath, tomlConfigPath, dryRun, enableLogCollector := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	params := initializer.InitializationParams{
		KubeconfigPath:     *kubeconfigPath,
		TomlConfigPath:     *tomlConfigPath,
		DryRun:             *dryRun,
		EnableLogCollector: *enableLogCollector,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		return components.Reconciler.Start(gCtx)
	})

	return g.Wait()
}

func parseFlags() (metricsPort, kubeconfigPath, tomlConfigPath *string, dryRun, enableLogCollector *bool) {
	metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	tomlConfigPath = flag.String("config-path", "/etc/config/config.toml",
		"path where the fault remediation config file is present")

	dryRun = flag.Bool("dry-run", false, "flag to run fault remediation module in dry-run mode")

	enableLogCollector = flag.Bool("enable-log-collector", false,
		"enable log collector feature for gathering logs from affected nodes")

	flag.Parse()

	return
}
