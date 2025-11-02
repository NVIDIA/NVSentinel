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

	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	sdkconfig "github.com/nvidia/nvsentinel/store-client-sdk/pkg/config"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore/providers"
	"golang.org/x/sync/errgroup"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type config struct {
	namespace                   string
	version                     string
	apiGroup                    string
	templateMountPath           string
	templateFileName            string
	metricsPort                 string
	databaseClientCertMountPath string
	kubeconfigPath              string
	dryRun                      bool
	enableLogCollector          bool
	updateMaxRetries            int
	updateRetryDelaySeconds     int
}

// parseFlags parses command-line flags and returns a config struct.
func parseFlags() *config {
	cfg := &config{}

	flag.StringVar(&cfg.metricsPort, "metrics-port", "2112", "port to expose Prometheus metrics on")

	// Register database certificate flags using common package
	certConfig := flags.RegisterDatabaseCertFlags()

	flag.StringVar(&cfg.kubeconfigPath, "kubeconfig-path", "", "path to kubeconfig file")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "flag to run node drainer module in dry-run mode")
	flag.IntVar(&cfg.updateMaxRetries, "update-max-retries", 5,
		"maximum attempts to update remediation status per event")
	flag.IntVar(&cfg.updateRetryDelaySeconds, "update-retry-delay-seconds", 10,
		"delay in seconds between remediation status update retries")
	flag.Parse()

	// Resolve the certificate path using common logic
	cfg.databaseClientCertMountPath = certConfig.ResolveCertPath()

	return cfg
}

func getRequiredEnvVars() (*config, error) {
	cfg := &config{}

	requiredVars := map[string]*string{
		"MAINTENANCE_NAMESPACE": &cfg.namespace,
		"MAINTENANCE_VERSION":   &cfg.version,
		"MAINTENANCE_API_GROUP": &cfg.apiGroup,
		"TEMPLATE_MOUNT_PATH":   &cfg.templateMountPath,
		"TEMPLATE_FILE_NAME":    &cfg.templateFileName,
	}

	for envVar, ptr := range requiredVars {
		*ptr = os.Getenv(envVar)
		if *ptr == "" {
			return nil, fmt.Errorf("%s is not provided", envVar)
		}
	}

	// Feature flag: default disabled; only "true" enables it
	if v := os.Getenv("ENABLE_LOG_COLLECTOR"); v == "true" {
		cfg.enableLogCollector = true
	}

	slog.Info("Configuration loaded",
		"namespace", cfg.namespace,
		"version", cfg.version,
		"apiGroup", cfg.apiGroup,
		"templateMountPath", cfg.templateMountPath,
		"templateFileName", cfg.templateFileName)

	return cfg, nil
}

func getTokenConfig() (*client.TokenConfig, error) {
	// Use centralized configuration from store-client-sdk
	tokenConfig, err := sdkconfig.TokenConfigFromEnv("fault-remediation-module")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	return &client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}, nil
}

func getDatabasePipeline() interface{} {
	// Filter for quarantine events (for remediation)
	quarantineEventsFilter := client.NewFilterBuilder().
		In("fullDocument.healtheventstatus.userpodsevictionstatus.status",
			[]interface{}{model.StatusSucceeded, model.AlreadyDrained}).
		In("fullDocument.healtheventstatus.nodequarantined",
			[]interface{}{model.Quarantined, model.AlreadyQuarantined}).
		Build()

	// Filter for unquarantine events (for annotation cleanup)
	unquarantineEventsFilter := client.NewFilterBuilder().
		Eq("fullDocument.healtheventstatus.nodequarantined", model.UnQuarantined).
		Eq("fullDocument.healtheventstatus.userpodsevictionstatus.status", model.StatusSucceeded).
		Build()

	// Build the main match condition using database-agnostic builders
	matchCondition := client.NewFilterBuilder().
		Eq("operationType", "update").
		Or(quarantineEventsFilter, unquarantineEventsFilter).
		Build()

	// Use database-agnostic pipeline builder to hide MongoDB-specific operators
	return client.BuildChangeStreamPipeline(matchCondition)
}

// getCertPath checks if the certificate exists at the new path, falls back to legacy path
func getCertPath(databaseClientCertMountPath string) string {
	// Check if ca.crt exists at the new path
	if _, err := os.Stat(databaseClientCertMountPath + "/ca.crt"); err == nil {
		return databaseClientCertMountPath
	}

	// Fall back to legacy mongo-client path
	legacyPath := "/etc/ssl/mongo-client"
	if _, err := os.Stat(legacyPath + "/ca.crt"); err == nil {
		slog.Info("Using legacy certificate path for backward compatibility", "path", legacyPath)
		return legacyPath
	}

	// If neither exists, return the new path (original behavior)
	return databaseClientCertMountPath
}

func run() error {
	// Create a context that listens for OS interrupt signals (SIGINT, SIGTERM).
	// This enables proper graceful shutdown in Kubernetes environments
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Parse flags and get configuration
	cfg := parseFlags()

	// Get required environment variables
	envCfg, err := getRequiredEnvVars()
	if err != nil {
		return fmt.Errorf("failed to get required environment variables: %w", err)
	}

	// Get datastore configuration using the new unified system
	dsConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Override SSL cert path if provided via command line
	if cfg.databaseClientCertMountPath != "" && dsConfig.Connection.SSLCert == "" {
		certPath := getCertPath(cfg.databaseClientCertMountPath)
		dsConfig.Connection.SSLCert = certPath + "/tls.crt"
		dsConfig.Connection.SSLKey = certPath + "/tls.key"
		dsConfig.Connection.SSLRootCert = certPath + "/ca.crt"
	}

	// Get token configuration
	tokenConfig, err := getTokenConfig()
	if err != nil {
		return fmt.Errorf("failed to get token configuration: %w", err)
	}

	// Get database pipeline
	pipeline := getDatabasePipeline()

	// Initialize k8s client
	k8sClient, clientSet, err := reconciler.NewK8sClient(cfg.kubeconfigPath, cfg.dryRun, reconciler.TemplateData{
		Namespace:         envCfg.namespace,
		Version:           envCfg.version,
		ApiGroup:          envCfg.apiGroup,
		TemplateMountPath: envCfg.templateMountPath,
		TemplateFileName:  envCfg.templateFileName,
	})
	if err != nil {
		return fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized k8sclient")

	// Initialize and start reconciler
	reconcilerCfg := reconciler.ReconcilerConfig{
		DataStoreConfig:    dsConfig,
		TokenConfig:        *tokenConfig,
		DatabasePipeline:   pipeline,
		RemediationClient:  k8sClient,
		StateManager:       statemanager.NewStateManager(clientSet),
		EnableLogCollector: envCfg.enableLogCollector,
		UpdateMaxRetries:   cfg.updateMaxRetries,
		UpdateRetryDelay:   time.Duration(cfg.updateRetryDelaySeconds) * time.Second,
	}

	reconciler := reconciler.NewReconciler(reconcilerCfg, cfg.dryRun)

	// Parse the metrics port
	portInt, err := strconv.Atoi(cfg.metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	// Create the server
	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	// Start server and reconciler concurrently
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

	g.Go(func() error {
		return reconciler.Start(gCtx)
	})

	// Wait for both goroutines to finish
	return g.Wait()
}

func main() {
	logger.SetDefaultStructuredLogger("fault-remediation-module", version)
	slog.Info("Starting fault-remediation-module", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}
