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
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/reconciler"
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

func main() {
	logger.SetDefaultStructuredLogger("fault-quarantine-module", version)
	slog.Info("Starting fault-quarantine-module", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)
		os.Exit(1)
	}
}

func parseFlags() (metricsPort, mongoClientCertMountPath, kubeconfigPath *string, dryRun, circuitBreakerEnabled *bool,
	circuitBreakerPercentage *int, circuitBreakerDuration *time.Duration) {
	metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	dryRun = flag.Bool("dry-run", false, "flag to run node drainer module in dry-run mode")

	circuitBreakerPercentage = flag.Int("circuit-breaker-percentage",
		50, "percentage of nodes to cordon before tripping the circuit breaker")

	circuitBreakerDuration = flag.Duration("circuit-breaker-duration",
		5*time.Minute, "duration of the circuit breaker window")

	circuitBreakerEnabled = flag.Bool("circuit-breaker-enabled", true,
		"enable or disable fault quarantine circuit breaker")

	flag.Parse()

	return
}

func loadEnvConfig() (namespace, mongoURI, mongoDatabase, mongoCollection, tokenDatabase, tokenCollection string,
	err error) {
	namespace = os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return "", "", "", "", "", "", fmt.Errorf("POD_NAMESPACE is not provided")
	}

	mongoURI = os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		return "", "", "", "", "", "", fmt.Errorf("MONGODB_URI is not provided")
	}

	mongoDatabase = os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		return "", "", "", "", "", "", fmt.Errorf("MONGODB_DATABASE_NAME is not provided")
	}

	mongoCollection = os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoCollection == "" {
		return "", "", "", "", "", "", fmt.Errorf("MONGODB_COLLECTION_NAME is not provided")
	}

	tokenDatabase = os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		return "", "", "", "", "", "", fmt.Errorf("MONGODB_DATABASE_NAME is not provided")
	}

	tokenCollection = os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		return "", "", "", "", "", "", fmt.Errorf("MongoDB token collection name is not provided")
	}

	return namespace, mongoURI, mongoDatabase, mongoCollection, tokenDatabase, tokenCollection, nil
}

func loadMongoTimeouts() (totalTimeoutSeconds, intervalSeconds, totalCACertTimeoutSeconds,
	intervalCACertSeconds, unprocessedEventsMetricUpdateIntervalSeconds int, err error) {
	totalTimeoutSeconds, err = getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalSeconds, err = getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid MONGODB_PING_INTERVAL_SECONDS: %w", err)
	}

	totalCACertTimeoutSeconds, err = getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalCACertSeconds, err = getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid CA_CERT_READ_INTERVAL_SECONDS: %w", err)
	}

	unprocessedEventsMetricUpdateIntervalSeconds, err =
		getEnvAsInt("UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS", 25)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS: %w", err)
	}

	return
}

func createMongoConfig(
	mongoURI, mongoDatabase, mongoCollection, mongoClientCertMountPath string,
	totalTimeoutSeconds, intervalSeconds, totalCACertTimeoutSeconds, intervalCACertSeconds int,
) storewatcher.MongoDBConfig {
	return storewatcher.MongoDBConfig{
		URI:        mongoURI,
		Database:   mongoDatabase,
		Collection: mongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    totalTimeoutSeconds,
		TotalPingIntervalSeconds:   intervalSeconds,
		TotalCACertTimeoutSeconds:  totalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: intervalCACertSeconds,
	}
}

func createTokenConfig(tokenDatabase, tokenCollection string) storewatcher.TokenConfig {
	return storewatcher.TokenConfig{
		ClientName:      "fault-quarantine-module",
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}
}

func createPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert"}}}}}}},
	}
}

func run() error {
	// Create a context that gets cancelled on OS interrupt signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop() // Ensure the signal listener is cleaned up

	metricsPort, mongoClientCertMountPath, kubeconfigPath, dryRun, circuitBreakerEnabled,
		circuitBreakerPercentage, circuitBreakerDuration := parseFlags()

	namespace, mongoURI, mongoDatabase, mongoCollection, tokenDatabase, tokenCollection, err := loadEnvConfig()
	if err != nil {
		return err
	}

	totalTimeoutSeconds, intervalSeconds, totalCACertTimeoutSeconds,
		intervalCACertSeconds, unprocessedEventsMetricUpdateIntervalSeconds, err := loadMongoTimeouts()
	if err != nil {
		return err
	}

	mongoConfig := createMongoConfig(mongoURI, mongoDatabase, mongoCollection, *mongoClientCertMountPath,
		totalTimeoutSeconds, intervalSeconds, totalCACertTimeoutSeconds, intervalCACertSeconds)

	tokenConfig := createTokenConfig(tokenDatabase, tokenCollection)

	pipeline := createPipeline()

	tomlCfg, err := config.LoadTomlConfig("/etc/config/config.toml")
	if err != nil {
		return fmt.Errorf("error loading TOML config: %w", err)
	}

	if *dryRun {
		slog.Info("Running in dry-run mode")
	}

	// Initialize the k8s client
	k8sClient, err := reconciler.NewFaultQuarantineClient(*kubeconfigPath, *dryRun)
	if err != nil {
		return fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized k8sclient")

	reconcilerCfg := reconciler.ReconcilerConfig{
		TomlConfig:                       *tomlCfg,
		MongoHealthEventCollectionConfig: mongoConfig,
		TokenConfig:                      tokenConfig,
		MongoPipeline:                    pipeline,
		K8sClient:                        k8sClient,
		DryRun:                           *dryRun,
		CircuitBreakerEnabled:            *circuitBreakerEnabled,
		UnprocessedEventsMetricUpdateInterval: time.Duration(unprocessedEventsMetricUpdateIntervalSeconds) *
			time.Second,
		CircuitBreaker: reconciler.CircuitBreakerConfig{
			Namespace:  namespace,
			Name:       "fault-quarantine-circuit-breaker",
			Percentage: *circuitBreakerPercentage,
			Duration:   *circuitBreakerDuration,
		},
	}

	// Create the work signal channel (buffered channel acting as semaphore)
	workSignal := make(chan struct{}, 1) // Buffer size 1 is usually sufficient

	// Pass the workSignal channel to the Reconciler
	rec := reconciler.NewReconciler(ctx, reconcilerCfg, workSignal)

	// Parse the port
	portInt, err := strconv.Atoi(*metricsPort)
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
		return rec.Start(gCtx)
	})

	// Wait for both goroutines to finish
	return g.Wait()
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
