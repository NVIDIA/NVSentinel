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
	"github.com/nvidia/nvsentinel/labeler/pkg/initializer"
	"github.com/nvidia/nvsentinel/labeler/pkg/labeler"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("labeler", version)
	slog.Info("Starting labeler", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("labeler"); err != nil {
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
	kubeconfig, metricsPort, dcgmAppLabel, driverAppLabel, gkeInstallerAppLabel, kataLabel := parseFlags()

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
		KubeconfigPath:       *kubeconfig,
		DCGMAppLabel:         *dcgmAppLabel,
		DriverAppLabel:       *driverAppLabel,
		GKEInstallerAppLabel: *gkeInstallerAppLabel,
		KataLabel:            *kataLabel,
	}

	components, err := initializer.InitializeAll(params)
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
		return components.Labeler.Run(gCtx)
	})

	return g.Wait()
}

func parseFlags() (kubeconfig, metricsPort, dcgmAppLabel, driverAppLabel, gkeInstallerAppLabel, kataLabel *string) {
	kubeconfig = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	metricsPort = flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	dcgmAppLabel = flag.String("dcgm-app-label", "nvidia-dcgm", "App label value for DCGM pods")
	driverAppLabel = flag.String("driver-app-label", "nvidia-driver-daemonset", "App label value for driver pods")
	gkeInstallerAppLabel = flag.String("gke-installer-app-label",
		"nvidia-driver-installer", "App label value for GKE driver installer pods")
	kataLabel = flag.String("kata-label", "",
		fmt.Sprintf("Custom node label to check for Kata Containers support. If empty, uses default '%s'",
			labeler.KataRuntimeDefaultLabel))

	flag.Parse()

	return
}
