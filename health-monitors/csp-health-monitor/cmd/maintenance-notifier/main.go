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

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	met "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	trigger "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/triggerengine"
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
	processingStrategy          string
	// udsTokenPath is a projected ServiceAccount token presented to
	// platform-connector. This notifier polls a central cloud-provider API and
	// therefore reports on nodes other than its own, which platform-connector
	// only permits for an allowlisted, token-authenticated identity. Empty
	// disables token authentication.
	udsTokenPath string
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
	flag.StringVar(&cfg.processingStrategy, "processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")
	flag.StringVar(&cfg.udsTokenPath, "uds-token-path", "",
		"Path to a projected ServiceAccount token presented to platform-connector. "+
			"Required for reporting health events about nodes other than the one this pod runs on; "+
			"empty disables token authentication.")

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
		"platform connector UDS path (dialed unless HEALTH_PUBLISH_TARGET is set)", cfg.udsPath,
		"database client cert mount path", cfg.databaseClientCertMountPath,
		"exposing sidecar metrics on port", cfg.metricsPort,
		"platform connector token auth", cfg.udsTokenPath != "",
	)
	slog.Debug("log verbosity level is set based on the -v flag for sidecar.")
}

// setupUDSConnection routes the dial through the shared healthpub environment
// switch: with HEALTH_PUBLISH_TARGET set the client talks to the deployment
// platform connector directly, otherwise it dials the node-local socket with
// exactly today's options (an insecure transport plus the token interceptor
// for tokenPath). The option hands the connection to the engine's publisher,
// which closes it in Close.
func setupUDSConnection(udsPath, tokenPath string) (
	pb.PlatformConnectorClient, healthpub.Option, error,
) {
	target := fmt.Sprintf("unix:%s", udsPath)

	conn, client, pubOpt, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		// Legacy socket mode: dial the node-local socket exactly as today.
		// Logged here so the socket path and token flag are only reported
		// when the socket is what actually gets dialed; DialFromEnvOr logs
		// the direct-mode dial itself.
		slog.Info("Sidecar attempting to connect to Platform Connector",
			"unix", udsPath, "tokenAuthEnabled", tokenPath != "")

		return grpc.NewClient(target, grpcclient.InsecureDialOptions(tokenPath)...)
	})
	if err != nil {
		metrics.TriggerUDSSendErrors.Inc()
		slog.Error("Sidecar failed to dial Platform Connector",
			"socketTarget", target,
			"error", err)

		return nil, nil, fmt.Errorf("maintenance-notifier: failed to connect to Platform Connector: %w", err)
	}

	slog.Info("Sidecar successfully connected to Platform Connector.", "target", conn.Target())

	return client, pubOpt, nil
}

func setupKubernetesClient() (kubernetes.Interface, error) {
	var restCfg *rest.Config

	var err error

	restCfg, err = rest.InClusterConfig()
	if err != nil {
		slog.Warn("trigger engine, failed to obtain in-cluster Kubernetes config", "error", err)
		return nil, fmt.Errorf("failed to obtain in-cluster Kubernetes config: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		slog.Error("trigger engine, failed to create Kubernetes clientset", "error", err)
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	slog.Info("Trigger Engine: Kubernetes clientset initialized successfully for node readiness checks.")

	return k8sClient, nil
}

func run() error {
	appCfg := parseFlags()

	ff := met.NewRegistry("maintenance-notifier")
	ff.SetStoreOnlyMode(appCfg.processingStrategy)

	logStartupInfo(appCfg)

	cfg, err := config.LoadConfig(appCfg.configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration from %s: %w", appCfg.configPath, err)
	}

	// Create context with signal handling for graceful shutdown.
	// This context will be cancelled when SIGINT or SIGTERM is received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Parse the metrics port
	portInt, err := strconv.Atoi(appCfg.metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	// Create and start HTTP server with metrics and health endpoints immediately.
	// This allows Kubernetes probes to pass while database connection is established.
	server := srv.NewServer(
		srv.WithPort(portInt),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
	)

	// Use errgroup to manage concurrent goroutines with proper cancellation
	g, gCtx := errgroup.WithContext(ctx)

	// Metrics server failures are logged but do NOT terminate the service.
	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := server.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		return runTriggerEngine(gCtx, appCfg, cfg)
	})

	// Wait for both goroutines to finish
	if err := g.Wait(); err != nil {
		return fmt.Errorf("service error: %w", err)
	}

	slog.Info("Quarantine Trigger Engine Sidecar shut down.")

	return nil
}

// runTriggerEngine wires the datastore, the platform-connector client and the
// Kubernetes client together, then blocks in the trigger engine until ctx is
// cancelled and closes the publisher.
func runTriggerEngine(ctx context.Context, appCfg *appConfig, cfg *config.Config) error {
	slog.Info("Initializing datastore connection for sidecar...")

	store, err := datastore.NewStore(ctx, &appCfg.databaseClientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to initialize datastore: %w", err)
	}

	slog.Info("Datastore initialized successfully for sidecar.")

	platformConnectorClient, pubOpt, err := setupUDSConnection(appCfg.udsPath, appCfg.udsTokenPath)
	if err != nil {
		return fmt.Errorf("platform connector connection setup failed: %w", err)
	}

	k8sClient, err := setupKubernetesClient()
	if err != nil {
		return fmt.Errorf("kubernetes client setup failed: %w", err)
	}

	value, ok := pb.ProcessingStrategy_value[appCfg.processingStrategy]
	if !ok {
		return fmt.Errorf("invalid processingStrategy %q (expected EXECUTE_REMEDIATION or STORE_ONLY)",
			appCfg.processingStrategy)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", appCfg.processingStrategy)

	engine := trigger.NewEngine(cfg, store, platformConnectorClient,
		fmt.Sprintf("unix:%s", appCfg.udsPath),
		k8sClient, pb.ProcessingStrategy(value), pubOpt)
	// Closes the publisher and the connection it owns.
	defer engine.Close()

	slog.Info("Trigger engine starting...")
	engine.Start(ctx)
	slog.Info("Trigger engine stopped.")

	return nil
}
