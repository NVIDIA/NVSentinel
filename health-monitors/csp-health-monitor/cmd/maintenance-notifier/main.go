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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	trigger "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/triggerengine"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultConfigPathSidecar    = "/etc/config/config.toml"
	defaultMongoCertPathSidecar = "/etc/ssl/mongo-client"
	defaultUdsPathSidecar       = "/run/nvsentinel/nvsentinel.sock"
	defaultMetricsPortSidecar   = "2113"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type appConfig struct {
	configPath               string
	udsPath                  string
	mongoClientCertMountPath string
	metricsPort              string
}

func parseFlags() *appConfig {
	cfg := &appConfig{}
	// Command-line flags
	flag.StringVar(&cfg.configPath, "config", defaultConfigPathSidecar, "Path to the TOML configuration file.")
	flag.StringVar(&cfg.udsPath, "uds-path", defaultUdsPathSidecar, "Path to the Platform Connector UDS socket.")
	flag.StringVar(&cfg.mongoClientCertMountPath,
		"mongo-client-cert-mount-path",
		defaultMongoCertPathSidecar,
		"Directory where MongoDB client tls.crt, tls.key, and ca.crt are mounted.",
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
		"mongoDB client cert mount path", cfg.mongoClientCertMountPath,
		"exposing sidecar metrics on port", cfg.metricsPort,
	)
	slog.Debug("log verbosity level is set based on the -v flag for sidecar.")
}

func startMetricsServer(metricsPort string) {
	// Start metrics endpoint in a separate goroutine
	go func() {
		listenAddress := fmt.Sprintf(":%s", metricsPort)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		server := &http.Server{
			Addr:         listenAddress,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  15 * time.Second,
		}

		slog.Info("metrics server (sidecar) starting to listen", "port", listenAddress)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server (sidecar) failed", "error", err)
		}

		slog.Info("Metrics server (sidecar) stopped.")
	}()
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startMetricsServer(appCfg.metricsPort)

	store, err := datastore.NewStore(ctx, &appCfg.mongoClientCertMountPath)
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

	slog.Info("Trigger engine starting...")
	engine.Start(ctx) // This is blocking

	slog.Info("Quarantine Trigger Engine Sidecar shut down.")

	return nil
}
