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
	"strconv"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("health-events-analyzer", version)
	slog.Info("Starting health-events-analyzer", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
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

func loadDatabaseConfig(databaseClientCertMountPath string) (*datastore.DataStoreConfig, error) {
	// Load using the new unified datastore configuration
	config, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Override SSL cert path if provided via command line
	if databaseClientCertMountPath != "" && config.Connection.SSLCert == "" {
		certPath := getCertPath(databaseClientCertMountPath)
		config.Connection.SSLCert = certPath + "/tls.crt"
		config.Connection.SSLKey = certPath + "/tls.key"
		config.Connection.SSLRootCert = certPath + "/ca.crt"
	}

	return config, nil
}

func createPipeline() interface{} {
	return client.BuildNonFatalUnhealthyInsertsPipeline()
}

func connectToPlatform(socket string) (*publisher.PublisherConfig, *grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	conn, err := grpc.NewClient(socket, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial platform connector UDS %s: %w", socket, err)
	}

	platformConnectorClient := protos.NewPlatformConnectorClient(conn)
	pub := publisher.NewPublisher(platformConnectorClient)

	return pub, conn, nil
}

func run() error {
	ctx := context.Background()

	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	socket := flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")
	tomlConfigPath := flag.String("config-path", "/etc/config/config.toml", "path to TOML config file")
	databaseClientCertMountPath := flag.String("database-client-cert-mount-path", "/etc/ssl/database-client",
		"path where the database client cert is mounted")

	flag.Parse()

	databaseConfig, err := loadDatabaseConfig(*databaseClientCertMountPath)
	if err != nil {
		return err
	}

	pipeline := createPipeline()

	pub, conn, err := connectToPlatform(*socket)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Parse the TOML content
	tomlConfig, err := config.LoadTomlConfig(*tomlConfigPath)
	if err != nil {
		return fmt.Errorf("error loading TOML config: %w", err)
	}

	reconcilerCfg := reconciler.HealthEventsAnalyzerReconcilerConfig{
		DataStoreConfig:           databaseConfig,
		Pipeline:                  pipeline,
		HealthEventsAnalyzerRules: tomlConfig,
		Publisher:                 pub,
	}

	rec := reconciler.NewReconciler(reconcilerCfg)

	// Parse the metrics port
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
