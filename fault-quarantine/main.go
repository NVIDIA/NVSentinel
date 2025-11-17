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
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/initializer"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("fault-quarantine", version)
	slog.Info("Starting fault-quarantine", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	metricsPort, databaseClientCertMountPath, kubeconfigPath, dryRun, circuitBreakerEnabled,
		tomlConfigPath := parseFlags()

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
		DatabaseClientCertMountPath: databaseClientCertMountPath,
		KubeconfigPath:              *kubeconfigPath,
		TomlConfigPath:              *tomlConfigPath,
		DryRun:                      *dryRun,
		CircuitBreakerEnabled:       *circuitBreakerEnabled,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	slog.Info("Starting node informer")

	if err := components.K8sClient.NodeInformer.Run(ctx.Done()); err != nil {
		return fmt.Errorf("failed to start node informer: %w", err)
	}

	slog.Info("Node informer started and synced")

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

func parseFlags() (
	metricsPort *string,
	databaseClientCertMountPath string,
	kubeconfigPath *string,
	dryRun, circuitBreakerEnabled *bool,
	tomlConfigPath *string,
) {
	metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	// Register database certificate flags using common package
	certConfig := flags.RegisterDatabaseCertFlags()

	kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	tomlConfigPath = flag.String("config-path", "/etc/config/config.toml",
		"path where the fault quarantine config file is present")

	dryRun = flag.Bool("dry-run", false, "flag to run fault quarantine module in dry-run mode")

	circuitBreakerEnabled = flag.Bool("circuit-breaker-enabled", true,
		"enable or disable fault quarantine circuit breaker")

	flag.Parse()

	// Resolve the certificate path using common logic
	databaseClientCertMountPath = certConfig.ResolveCertPath()

	return
}
