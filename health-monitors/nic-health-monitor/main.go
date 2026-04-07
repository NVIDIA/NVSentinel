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
	"os/signal"
	"strconv"
	"syscall"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
)

const (
	defaultAgentName = "nic-health-monitor"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	checksList = flag.String("checks",
		"InfiniBandStateCheck,InfiniBandDegradationCheck,EthernetStateCheck,EthernetDegradationCheck",
		"Comma-separated list of checks to enable")
	platformConnectorSocket = flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket")
	nodeNameEnv = flag.String("node-name", os.Getenv("NODE_NAME"),
		"Node name. Defaults to NODE_NAME env var.")
	statePollingIntervalFlag = flag.String("state-polling-interval", "1s",
		"Polling interval for state checks (e.g., 1s, 5s)")
	counterPollingIntervalFlag = flag.String("counter-polling-interval", "5s",
		"Polling interval for counter checks (e.g., 5s, 10s)")
	metricsPort = flag.String("metrics-port", "2112",
		"Port to expose Prometheus metrics on")
	configPath = flag.String("config", "/etc/nic-health-monitor/config.yaml",
		"Path to YAML configuration file")
	metadataPath = flag.String("metadata-path", "/var/lib/nvsentinel/gpu_metadata.json",
		"Path to GPU metadata JSON file (used for NUMA-based management NIC exclusion)")
	processingStrategyFlag = flag.String("processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")
	_ = flag.String("kata-enabled", "false",
		"Indicates if this monitor is running in Kata Containers mode (set by DaemonSet variant)")
)

func main() {
	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting nic-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()
	slog.Info("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME env not set and --node-name flag not provided, cannot run")
	}

	slog.Info("Configuration",
		"node", nodeName,
		"checks", *checksList,
		"configPath", *configPath,
		"metadataPath", *metadataPath,
		"platformConnectorSocket", *platformConnectorSocket,
		"statePollingInterval", *statePollingIntervalFlag,
		"counterPollingInterval", *counterPollingIntervalFlag,
		"processingStrategy", *processingStrategyFlag,
	)

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Warn("Failed to load config file, using defaults", "error", err, "path", *configPath)

		cfg = &config.Config{
			SysClassInfinibandPath: "/nvsentinel/sys/class/infiniband",
			SysClassNetPath:        "/nvsentinel/sys/class/net",
		}

		slog.Info("Using default configuration with CLI flags")
	}

	slog.Info("Configuration loaded",
		"sysClassNetPath", cfg.SysClassNetPath,
		"sysClassInfinibandPath", cfg.SysClassInfinibandPath,
		"counterDetectionEnabled", cfg.CounterDetection.Enabled,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	slog.Info("Starting metrics server", "port", portInt)

	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("metrics server failed: %w", err)
	}

	slog.Info("Shutting down nic-health-monitor")

	return nil
}
