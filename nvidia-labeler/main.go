// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/nvidia-labeler/pkg/initializer"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("nvidia-labeler", version)
	slog.Info("Starting NVIDIA labeler", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("nvidia-labeler"); err != nil {
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
	options := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	portInt, err := strconv.Atoi(options.metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	components, err := initializer.InitializeAll(options.initializationParams())
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

type cliOptions struct {
	kubeconfig                   string
	metricsPort                  string
	dcgmAppLabel                 string
	driverAppLabel               string
	gkeInstallerAppLabel         string
	assumeDCGMAvailable          bool
	assumeDriverInstalled        bool
	requireDCGMReadyForBootstrap bool
	kubernetesClientRateLimits   kubeclient.RateLimitConfig
}

func (o cliOptions) initializationParams() initializer.InitializationParams {
	return initializer.InitializationParams{
		KubeconfigPath:               o.kubeconfig,
		DCGMAppLabel:                 o.dcgmAppLabel,
		DriverAppLabel:               o.driverAppLabel,
		GKEInstallerAppLabel:         o.gkeInstallerAppLabel,
		AssumeDCGMAvailable:          o.assumeDCGMAvailable,
		AssumeDriverInstalled:        o.assumeDriverInstalled,
		RequireDCGMReadyForBootstrap: o.requireDCGMReadyForBootstrap,
		KubernetesClientRateLimits:   o.kubernetesClientRateLimits,
	}
}

func parseFlags() cliOptions {
	kubeconfig := flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	metricsPort := flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	dcgmAppLabel := flag.String("dcgm-app-label", "nvidia-dcgm,nvidia-dcgm-dra",
		"Comma-separated app label values for DCGM pods")
	driverAppLabel := flag.String("driver-app-label", "nvidia-driver-daemonset", "App label value for driver pods")
	gkeInstallerAppLabel := flag.String("gke-installer-app-label",
		"nvidia-driver-installer", "App label value for GKE driver installer pods")
	assumeDCGMAvailable := flag.Bool("assume-dcgm-available", false,
		"Assume DCGM is available when a valid dcgm.version node label exists and no DCGM pod source is found.")
	assumeDriverInstalled := flag.Bool("assume-driver-installed", false,
		"Assume GPU drivers are pre-installed on GPU nodes (nvidia.com/gpu.present=true). "+
			"Sets driver.installed=true unconditionally for those nodes, skipping driver pod detection. "+
			"Use for clusters with host-installed drivers.")
	requireDCGMReadyForBootstrap := flag.Bool("require-dcgm-ready-for-bootstrap", true,
		"Require the DCGM pod to be ready before setting the DCGM version label for initial bootstrap.")
	rateLimits := kubeclient.RegisterRateLimitFlags()

	flag.Parse()

	return cliOptions{
		kubeconfig:                   *kubeconfig,
		metricsPort:                  *metricsPort,
		dcgmAppLabel:                 *dcgmAppLabel,
		driverAppLabel:               *driverAppLabel,
		gkeInstallerAppLabel:         *gkeInstallerAppLabel,
		assumeDCGMAvailable:          *assumeDCGMAvailable,
		assumeDriverInstalled:        *assumeDriverInstalled,
		requireDCGMReadyForBootstrap: *requireDCGMReadyForBootstrap,
		kubernetesClientRateLimits:   *rateLimits,
	}
}
