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
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	trigger "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/triggerengine"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultConfigPathSidecar       = "/etc/config/config.toml"
	defaultDatabaseCertPathSidecar = "/etc/ssl/database-client"
	defaultUdsPathSidecar          = "/run/nvsentinel/nvsentinel.sock"
	defaultMetricsPortSidecar      = "2113"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type appConfig struct {
	configPath                  string
	udsPath                     string
	databaseClientCertMountPath string
	metricsPort                 string
}

func parseFlags() *appConfig {
	cfg := &appConfig{}
	// Command-line flags
	flag.StringVar(&cfg.configPath, "config", defaultConfigPathSidecar, "Path to the TOML configuration file.")
	flag.StringVar(&cfg.udsPath, "uds-path", defaultUdsPathSidecar, "Path to the Platform Connector UDS socket.")
	flag.StringVar(&cfg.databaseClientCertMountPath,
		"database-client-cert-mount-path",
		defaultDatabaseCertPathSidecar,
		"Directory where database client tls.crt, tls.key, and ca.crt are mounted.",
	)
	flag.StringVar(&cfg.metricsPort, "metrics-port", defaultMetricsPortSidecar, "Port for the sidecar Prometheus metrics.")

	// Parse flags after initialising klog
	flag.Parse()

	return cfg
}

func main() {
	logger.SetDefaultStructuredLogger("maintenance-notifier", version)
	slog.Info("Starting maintenance-notifier", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func logStartupInfo(cfg *appConfig) {
	slog.Info("Using",
		"configuration file", cfg.configPath,
		"platform connector UDS path", cfg.udsPath,
		"database client cert mount path", cfg.databaseClientCertMountPath,
		"exposing sidecar metrics on port", cfg.metricsPort,
	)
	slog.Debug("log verbosity level is set based on the -v flag for sidecar.")
}

func setupUDSConnection(udsPath string) (*grpc.ClientConn, pb.PlatformConnectorClient) {
	slog.Info("Sidecar attempting to connect to Platform Connector UDS", "unix", udsPath)
	target := fmt.Sprintf("unix:%s", udsPath)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		metrics.TriggerUDSSendErrors.Inc()
		slog.Error("Sidecar failed to dial Platform Connector UDS",
			"target", target,
			"error", err)
	}

	slog.Info("Sidecar successfully connected to Platform Connector UDS.")

	return conn, pb.NewPlatformConnectorClient(conn)
}

func setupKubernetesClient() kubernetes.Interface {
	var restCfg *rest.Config

	var err error

	restCfg, err = rest.InClusterConfig()
	if err != nil {
		slog.Warn("trigger engine, failed to obtain in-cluster Kubernetes config", "error", err)
		return nil
	}

	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		slog.Error("trigger engine, failed to create Kubernetes clientset", "error", err)
		return nil
	}

	slog.Info("Trigger Engine: Kubernetes clientset initialized successfully for node readiness checks.")

	return k8sClient
}

func run() error {
	appCfg := parseFlags()
	logStartupInfo(appCfg)

	cfg, err := config.LoadConfig(appCfg.configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration from %s: %w", appCfg.configPath, err)
	}

	// Create context with signal handling for graceful shutdown.
	// This context will be cancelled when SIGINT or SIGTERM is received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := datastore.NewStore(ctx, &appCfg.databaseClientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to initialize datastore: %w", err)
	}

	slog.Info("Datastore initialized successfully for sidecar.")

	conn, platformConnectorClient := setupUDSConnection(appCfg.udsPath)

	defer func() {
		slog.Info("Closing UDS connection for sidecar.")

		if errClose := conn.Close(); errClose != nil {
			slog.Error("Error closing sidecar UDS connection", "error", errClose)
		}
	}()

	k8sClient := setupKubernetesClient()

	engine := trigger.NewEngine(cfg, store, platformConnectorClient, k8sClient)

	// Parse the metrics port
	portInt, err := strconv.Atoi(appCfg.metricsPort)
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

	// Start the trigger engine in a separate goroutine
	g.Go(func() error {
		slog.Info("Trigger engine starting...")
		engine.Start(gCtx) // This is blocking
		slog.Info("Trigger engine stopped.")

		return nil
	})

	// Wait for both goroutines to finish
	if err := g.Wait(); err != nil {
		return fmt.Errorf("service error: %w", err)
	}

	slog.Info("Quarantine Trigger Engine Sidecar shut down.")

	return nil
}
