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
	"path/filepath"
	"strconv"
	"strings"

	"log/slog"

	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/reconciler"
	pb "github.com/nvidia/nvsentinel/platform-connectors/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// initLogger initializes the structured logger with the appropriate log level.
func initLogger() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	})).With("module", "health-events-analyzer", "version", version)

	slog.SetDefault(logger)
}

//nolint:cyclop // todo
func main() {
	initLogger()
	slog.Info("Starting health-events-analyzer", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	var socket = flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")
	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	flag.Parse()

	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		return fmt.Errorf("MONGODB_URI is not set")
	}

	mongoDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		return fmt.Errorf("MONGODB_DATABASE_NAME is not set")
	}

	mongoCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoCollection == "" {
		return fmt.Errorf("MONGODB_COLLECTION_NAME is not set")
	}

	tokenDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		return fmt.Errorf("MONGODB_DATABASE_NAME is not set")
	}

	tokenCollection := os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		return fmt.Errorf("MONGODB_TOKEN_COLLECTION_NAME is not set")
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return fmt.Errorf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return fmt.Errorf("invalid MONGODB_PING_INTERVAL_SECONDS: %v", err)
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return fmt.Errorf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return fmt.Errorf("invalid CA_CERT_READ_INTERVAL_SECONDS: %v", err)
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
		ClientName:      "health-events-analyzer",
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}

	pipeline := mongo.Pipeline{
		bson.D{
			{Key: "$match", Value: bson.D{
				{Key: "operationType", Value: "insert"},
				{Key: "fullDocument.healthevent.isfatal", Value: false},
				{Key: "fullDocument.healthevent.ishealthy", Value: false},
			}},
		},
	}

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	conn, err := grpc.NewClient(*socket, opts...)
	if err != nil {
		return fmt.Errorf("failed to dial platform connector UDS %s: %v", *socket, err)
	}
	defer conn.Close()

	platformConnectorClient := pb.NewPlatformConnectorClient(conn)
	publisher := publisher.NewPublisher(platformConnectorClient)

	// Parse the TOML content
	tomlConfig, err := config.LoadTomlConfig("/etc/config/config.toml")
	if err != nil {
		return fmt.Errorf("error loading TOML config: %v", err)
	}

	reconcilerCfg := reconciler.HealthEventsAnalyzerReconcilerConfig{
		MongoHealthEventCollectionConfig: mongoConfig,
		TokenConfig:                      tokenConfig,
		MongoPipeline:                    pipeline,
		HealthEventsAnalyzerRules:        tomlConfig,
		Publisher:                        publisher,
	}

	reconciler := reconciler.NewReconciler(reconcilerCfg)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			slog.Error("Failed to start metrics server", "error", err)
			os.Exit(1)
		}
	}()

	reconciler.Start(ctx)

	return nil
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
