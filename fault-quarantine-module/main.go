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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// nolint: cyclop //fix this as part of NGCC-21793
func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)

	// Create a context that gets cancelled on OS interrupt signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop() // Ensure the signal listener is cleaned up

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	var kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	var dryRun = flag.Bool("dry-run", false, "flag to run node drainer module in dry-run mode")

	var circuitBreakerPercentage = flag.Int("circuit-breaker-percentage",
		50, "percentage of nodes to cordon before tripping the circuit breaker")

	var circuitBreakerDuration = flag.Duration("circuit-breaker-duration",
		5*time.Minute, "duration of the circuit breaker window")

	var circuitBreakerEnabled = flag.Bool("circuit-breaker-enabled", true,
		"enable or disable fault quarantine circuit breaker")

	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "fault-quarantine-module",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting fault-quarantine-module", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		klog.Fatalf("POD_NAMESPACE is not provided")
	}

	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		klog.Fatalf("MongoDB URI is not provided")
	}

	mongoDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		klog.Fatalf("MongoDB Database name is not provided")
	}

	mongoCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoCollection == "" {
		klog.Fatalf("MongoDB collection name is not provided")
	}

	tokenDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		klog.Fatalf("MongoDB token database name is not provided")
	}

	tokenCollection := os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		klog.Fatalf("MongoDB token collection name is not provided")
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_INTERVAL_SECONDS: %v", err)
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_READ_INTERVAL_SECONDS: %v", err)
	}

	unprocessedEventsMetricUpdateIntervalSeconds, err :=
		getEnvAsInt("UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS", 25)
	if err != nil {
		klog.Fatalf("invalid UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS: %v", err)
	}

	mongoConfig := storewatcher.MongoDBConfig{
		URI:        mongoURI,
		Database:   mongoDatabase,
		Collection: mongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(*mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(*mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(*mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    totalTimeoutSeconds,
		TotalPingIntervalSeconds:   intervalSeconds,
		TotalCACertTimeoutSeconds:  totalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: intervalCACertSeconds,
	}

	tokenConfig := storewatcher.TokenConfig{
		ClientName:      "fault-quarantine-module",
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert"}}}}}}},
	}

	tomlCfg, err := config.LoadTomlConfig("/etc/config/config.toml")
	if err != nil {
		klog.Fatalf("error while loading the toml config: %v", err)
	}

	if *dryRun {
		klog.Info("Running in dry-run mode")
	}

	// Initialize the k8s client
	k8sClient, err := reconciler.NewFaultQuarantineClient(*kubeconfigPath, *dryRun)
	if err != nil {
		klog.Fatalf("error while initializing kubernetes client: %v", err)
	}

	klog.Info("Successfully initialized k8sclient")

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
	reconciler := reconciler.NewReconciler(ctx, reconcilerCfg, workSignal)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	reconciler.Start(ctx)
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
