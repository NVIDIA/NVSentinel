// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	fd "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const (
	defaultAgentName       = "syslog-health-monitor"
	defaultComponentClass  = "GPU"                                // Or a more specific class if applicable
	defaultPollingInterval = "30m"                                // Added default polling interval
	defaultStateFilePath   = "/var/run/syslog_monitor/state.json" // Added default state file path
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// ConfigFile matches the top-level structure of the YAML config file
type ConfigFile struct {
	Checks []fd.CheckDefinition `yaml:"checks"`
}

func main() {
	logger.SetDefaultStructuredLogger("syslog-health-monitor", version)
	slog.Info("Starting syslog-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

//nolint:cyclop,gocognit // todo
func run() error {
	configFile := flag.String("config-file", "/etc/config/config.yaml",
		"Path to the YAML configuration file for log checks.")
	platformConnectorSocket := flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket.")
	nodeNameEnv := flag.String("node-name", os.Getenv("NODE_NAME"), "Node name. Defaults to NODE_NAME env var.")
	pollingIntervalFlag := flag.String("polling-interval", defaultPollingInterval,
		"Polling interval for health checks (e.g., 15m, 1h).")
	stateFileFlag := flag.String("state-file", defaultStateFilePath,
		"Path to state file for cursor persistence.")
	metricsPort := flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	xidAnalyserEndpoint := flag.String("xid-analyser-endpoint", "",
		"Endpoint to the XID analyser service.")

	flag.Parse()

	slog.Info("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME env not set and --node-name flag not provided, cannot run")
	}

	slog.Info("Using node name", "node", nodeName)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	slog.Info("Creating gRPC client to platform connector", "socket", *platformConnectorSocket)

	// Add retry logic for platform connector socket with detailed diagnostics
	var conn *grpc.ClientConn

	var err error

	maxRetries := 10
	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Info("Checking platform connector socket availability",
			"attempt", attempt,
			"maxRetries", maxRetries,
			"socket", *platformConnectorSocket)

		// Check if socket file exists before attempting connection
		socketPath := strings.TrimPrefix(*platformConnectorSocket, "unix://")
		if _, statErr := os.Stat(socketPath); statErr != nil {
			slog.Warn("Platform connector socket file does not exist",
				"attempt", attempt,
				"maxRetries", maxRetries,
				"error", statErr)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return fmt.Errorf("platform connector socket file not found after retries: %w", statErr)
		}

		conn, err = grpc.NewClient(*platformConnectorSocket, opts...)
		if err != nil {
			slog.Warn("Error creating gRPC client",
				"attempt", attempt,
				"maxRetries", maxRetries,
				"error", err)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return fmt.Errorf("failed to create gRPC client after retries: %w", err)
		}

		slog.Info("Successfully connected to platform connector", "attempt", attempt)

		break
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Error("Error closing gRPC connection", "error", closeErr)
		}
	}()

	client := pb.NewPlatformConnectorClient(conn)

	slog.Info("Loading checks from config file", "file", *configFile)

	// Add retry logic for config file reading with detailed diagnostics
	var yamlFile []byte

	maxConfigRetries := 5
	for attempt := 1; attempt <= maxConfigRetries; attempt++ {
		slog.Info("Reading config file",
			"attempt", attempt,
			"maxRetries", maxConfigRetries,
			"file", *configFile)

		// Check if config file exists
		if _, statErr := os.Stat(*configFile); statErr != nil {
			slog.Warn("Config file does not exist",
				"attempt", attempt,
				"maxRetries", maxConfigRetries,
				"error", statErr)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return fmt.Errorf("config file not found after retries: %w", statErr)
		}

		yamlFile, err = os.ReadFile(*configFile)

		if err != nil {
			slog.Warn("Error reading config file",
				"attempt", attempt,
				"maxRetries", maxConfigRetries,
				"error", err)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return fmt.Errorf("failed to read config file after retries: %w", err)
		}

		slog.Info("Successfully read config file", "attempt", attempt)

		break
	}

	var config ConfigFile

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		return fmt.Errorf("error unmarshalling config file: %w", err)
	}

	if len(config.Checks) == 0 {
		return fmt.Errorf("no checks defined in the config file")
	}

	slog.Info("Creating syslog monitor", "checksCount", len(config.Checks))

	fdHealthMonitor, err := fd.NewSyslogMonitor(nodeName, config.Checks, client, defaultAgentName,
		defaultComponentClass, *pollingIntervalFlag, *stateFileFlag, *xidAnalyserEndpoint)
	if err != nil {
		return fmt.Errorf("error creating syslog health monitor: %w", err)
	}

	// Parse polling interval
	pollingInterval, err := time.ParseDuration(*pollingIntervalFlag)
	if err != nil {
		return fmt.Errorf("error parsing polling interval: %w", err)
	}

	slog.Info("Polling interval configured", "interval", pollingInterval)

	// Start metrics server
	slog.Info("Starting metrics server", "port", *metricsPort)

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

	// Start server in errgroup alongside polling loop
	g, gCtx := errgroup.WithContext(context.Background())

	g.Go(func() error {
		return srv.Serve(gCtx)
	})

	g.Go(func() error {
		ticker := time.NewTicker(pollingInterval)
		defer ticker.Stop()

		slog.Info("Configured checks", "checks", config.Checks)
		slog.Info("Syslog health monitor initialization complete, starting polling loop...")

		// Polling loop
		for range ticker.C {
			slog.Info("Performing scheduled health check run...")

			if err := fdHealthMonitor.Run(); err != nil {
				return fmt.Errorf("error running syslog health monitor: %w", err)
			}
		}

		return nil
	})

	// Wait for both goroutines to finish
	return g.Wait()
}
