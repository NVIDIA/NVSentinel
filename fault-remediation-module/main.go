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
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"golang.org/x/sync/errgroup"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type config struct {
	namespace                string
	version                  string
	apiGroup                 string
	templateMountPath        string
	templateFileName         string
	metricsPort              string
	mongoClientCertMountPath string
	kubeconfigPath           string
	dryRun                   bool
	enableLogCollector       bool
	updateMaxRetries         int
	updateRetryDelaySeconds  int
}

// parseFlags parses command-line flags and returns a config struct.
func parseFlags() *config {
	cfg := &config{}

	flag.StringVar(&cfg.metricsPort, "metrics-port", "2112", "port to expose Prometheus metrics on")
	flag.StringVar(&cfg.mongoClientCertMountPath, "mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")
	flag.StringVar(&cfg.kubeconfigPath, "kubeconfig-path", "", "path to kubeconfig file")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "flag to run node drainer module in dry-run mode")
	flag.IntVar(&cfg.updateMaxRetries, "update-max-retries", 5,
		"maximum attempts to update remediation status per event")
	flag.IntVar(&cfg.updateRetryDelaySeconds, "update-retry-delay-seconds", 10,
		"delay in seconds between remediation status update retries")
	flag.Parse()

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

func getMongoDBConfig(mongoClientCertMountPath string) (*storewatcher.MongoDBConfig, error) {
	requiredEnvVars := map[string]string{
		"MONGODB_URI":                   "MongoDB URI",
		"MONGODB_DATABASE_NAME":         "MongoDB Database name",
		"MONGODB_COLLECTION_NAME":       "MongoDB collection name",
		"MONGODB_TOKEN_COLLECTION_NAME": "MongoDB token collection name",
	}

	envVars := make(map[string]string)

	for envVar, description := range requiredEnvVars {
		value := os.Getenv(envVar)
		if value == "" {
			return nil, fmt.Errorf("%s is not provided", description)
		}

		envVars[envVar] = value
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_INTERVAL_SECONDS: %w", err)
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_READ_INTERVAL_SECONDS: %w", err)
	}

	return &storewatcher.MongoDBConfig{
		URI:        envVars["MONGODB_URI"],
		Database:   envVars["MONGODB_DATABASE_NAME"],
		Collection: envVars["MONGODB_COLLECTION_NAME"],
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    totalTimeoutSeconds,
		TotalPingIntervalSeconds:   intervalSeconds,
		TotalCACertTimeoutSeconds:  totalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: intervalCACertSeconds,
	}, nil
}

func getTokenConfig() (*storewatcher.TokenConfig, error) {
	tokenDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		return nil, fmt.Errorf("MongoDB token database name is not provided")
	}

	tokenCollection := os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		return nil, fmt.Errorf("MongoDB token collection name is not provided")
	}

	return &storewatcher.TokenConfig{
		ClientName:      "fault-remediation-module",
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}, nil
}

func getMongoPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: "update"},
				bson.E{Key: "$or", Value: bson.A{
					// Watch for quarantine events (for remediation)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.StatusSucceeded, model.AlreadyDrained}},
						}},
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.Quarantined, model.AlreadyQuarantined}},
						}},
					},
					// Watch for unquarantine events (for annotation cleanup)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: model.UnQuarantined},
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: model.StatusSucceeded},
					},
				}},
			}},
		},
	}
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

	// Get MongoDB configuration
	mongoConfig, err := getMongoDBConfig(cfg.mongoClientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to get MongoDB configuration: %w", err)
	}

	// Get token configuration
	tokenConfig, err := getTokenConfig()
	if err != nil {
		return fmt.Errorf("failed to get token configuration: %w", err)
	}

	// Get MongoDB pipeline
	pipeline := getMongoPipeline()

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
		MongoConfig:        *mongoConfig,
		TokenConfig:        *tokenConfig,
		MongoPipeline:      pipeline,
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

func getEnvAsInt(name string, defaultValue int) (int, error) {
	valueStr, exists := os.LookupEnv(name)
	if !exists {
		return defaultValue, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("error converting %s to integer: %w", name, err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value of %s must be a positive integer", name)
	}

	return value, nil
}
