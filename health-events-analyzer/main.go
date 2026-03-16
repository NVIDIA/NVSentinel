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

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
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

func loadDatabaseConfig(databaseClientCertMountPath string) (*datastore.DataStoreConfig, error) {
	// Load using the new unified datastore configuration
	config, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Override SSL cert path if provided via command line.
	// When TLS is disabled, databaseClientCertMountPath is empty — skip overrides
	// so the datastore connects without TLS.
	if databaseClientCertMountPath != "" && config.Connection.SSLCert == "" {
		config.Connection.SSLCert = databaseClientCertMountPath + "/tls.crt"
		config.Connection.SSLKey = databaseClientCertMountPath + "/tls.key"
		config.Connection.SSLRootCert = databaseClientCertMountPath + "/ca.crt"
	}

	return config, nil
}

func createPipeline() interface{} {
	builder := client.GetPipelineBuilder()
	return builder.BuildProcessableNonFatalUnhealthyInsertsPipeline()
}

func connectToPlatform(socket string, processingStrategy protos.ProcessingStrategy) (
	*publisher.PublisherConfig, *grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	conn, err := grpc.NewClient(socket, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial platform connector UDS %s: %w", socket, err)
	}

	platformConnectorClient := protos.NewPlatformConnectorClient(conn)
	pub := publisher.NewPublisher(platformConnectorClient, processingStrategy)

	return pub, conn, nil
}

func run() error {
	ctx := context.Background()

	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	socket := flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")
	tomlConfigPath := flag.String("config-path", "/etc/config/config.toml", "path to TOML config file")
	certConfig := flags.RegisterDatabaseCertFlags()
	processingStrategyFlag := flag.String("processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy for analyzer output: EXECUTE_REMEDIATION or STORE_ONLY")

	flag.Parse()

	databaseConfig, err := loadDatabaseConfig(certConfig.ResolveCertPath())
	if err != nil {
		return err
	}

	pipeline := createPipeline()

	value, ok := protos.ProcessingStrategy_value[*processingStrategyFlag]
	if !ok {
		return fmt.Errorf("unexpected processingStrategy value: %q", *processingStrategyFlag)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", *processingStrategyFlag)

	pub, conn, err := connectToPlatform(*socket, protos.ProcessingStrategy(value))
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
