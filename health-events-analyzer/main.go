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
	"github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"
	"golang.org/x/sync/errgroup"

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

func main() {
	logger.SetDefaultStructuredLogger("health-events-analyzer", version)
	slog.Info("Starting health-events-analyzer", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func createPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{
			{Key: "$match", Value: bson.D{
				{Key: "operationType", Value: "insert"},
				{Key: "fullDocument.healthevent.agent", Value: bson.D{{Key: "$ne", Value: "health-events-analyzer"}}},
				{Key: "fullDocument.healthevent.ishealthy", Value: false},
			}},
		},
	}
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

	flag.Parse()

	mongoConfig, tokenConfig, err := storewatcher.LoadConfigFromEnv("health-events-analyzer")
	if err != nil {
		return fmt.Errorf("failed to load MongoDB configuration: %w", err)
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
		MongoHealthEventCollectionConfig: mongoConfig,
		TokenConfig:                      tokenConfig,
		MongoPipeline:                    pipeline,
		HealthEventsAnalyzerRules:        tomlConfig,
		Publisher:                        pub,
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
