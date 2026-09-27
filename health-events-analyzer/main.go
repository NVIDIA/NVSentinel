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

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	metrics "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
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
	logger.SetDefaultStructuredLoggerWithTraceCorrelation("health-events-analyzer", version)
	slog.Info("Starting health-events-analyzer", "version", version, "commit", commit, "date", date)

	if err := tracing.InitTracing(tracing.ServiceHealthEventsAnalyzer); err != nil {
		slog.Warn("Failed to initialize tracing", "error", err)
	}

	err := run()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	if shutdownErr := tracing.ShutdownTracing(shutdownCtx); shutdownErr != nil {
		slog.Warn("Failed to shutdown tracing", "error", shutdownErr)
	}

	cancel()

	if err != nil {
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

func createPipeline() any {
	builder := client.GetPipelineBuilder()
	return builder.BuildProcessableNonFatalUnhealthyInsertsPipeline()
}

// connectToPlatform routes the dial through the shared healthpub environment
// switch. With HEALTH_PUBLISH_TARGET set it connects directly to the
// deployment platform connector and the publisher runs in direct mode;
// otherwise it dials the node-local socket with exactly today's options (an
// insecure transport plus the token interceptor for tokenPath). The publisher
// owns the connection in both modes and closes it in Close.
func connectToPlatform(socket, tokenPath string, processingStrategy protos.ProcessingStrategy) (
	*publisher.PublisherConfig, error) {
	conn, platformConnectorClient, pubOpt, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		// Legacy socket mode: dial the node-local socket exactly as today.
		// Logged here so the socket path and token flag are only reported
		// when the socket is what actually gets dialed; DialFromEnvOr logs
		// the direct-mode dial itself.
		slog.Info("Dialing platform connector", "socket", socket, "tokenAuthEnabled", tokenPath != "")

		socketConn, dialErr := grpc.NewClient(socket, grpcclient.InsecureDialOptions(tokenPath)...)
		if dialErr != nil {
			return nil, fmt.Errorf("socket %s: %w", socket, dialErr)
		}

		return socketConn, nil
	})
	if err != nil {
		return nil, fmt.Errorf("health-events-analyzer: failed to dial platform connector: %w", err)
	}

	slog.Info("Platform connector client created", "target", conn.Target())

	return publisher.NewPublisher(platformConnectorClient, processingStrategy, pubOpt), nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	socket := flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")
	socketTokenPath := flag.String("socket-token-path", "",
		"Path to a projected ServiceAccount token presented to platform-connector. "+
			"Republished events preserve the node name of the original event, so this is required "+
			"for reporting on nodes other than the one this pod runs on; empty disables token authentication.")
	tomlConfigPath := flag.String("config-path", "/etc/config/config.toml", "path to TOML config file")
	certConfig := flags.RegisterDatabaseCertFlags()
	processingStrategyFlag := flag.String("processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy for analyzer output: EXECUTE_REMEDIATION or STORE_ONLY")
	workersFlag := flag.Int("workers", 1,
		"Number of concurrent worker goroutines for event processing partitioned by node (default: 1)")
	maxInFlightFlag := flag.Int("max-in-flight", 1000,
		"Maximum number of uncheckpointed in-flight events before applying backpressure (default: 1000)")

	flag.Parse()

	ff := metrics.NewRegistry("health-events-analyzer")
	ff.SetStoreOnlyMode(*processingStrategyFlag)

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

	pub, err := connectToPlatform(*socket, *socketTokenPath, protos.ProcessingStrategy(value))
	if err != nil {
		return err
	}

	// Closes the publisher and the connection it owns.
	defer pub.Close()

	// Parse the TOML content
	tomlConfig, err := config.LoadTomlConfig(*tomlConfigPath)
	if err != nil {
		return fmt.Errorf("error loading TOML config: %w", err)
	}

	for _, rule := range tomlConfig.Rules {
		ff.Set(rule.Name, rule.EvaluateRule)
	}

	reconcilerCfg := reconciler.HealthEventsAnalyzerReconcilerConfig{
		DataStoreConfig:           databaseConfig,
		Pipeline:                  pipeline,
		HealthEventsAnalyzerRules: tomlConfig,
		Publisher:                 pub,
		Workers:                   *workersFlag,
		MaxInFlight:               *maxInFlightFlag,
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
