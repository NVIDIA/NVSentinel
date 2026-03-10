// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/initializer"
	_ "github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/metrics"
)

const (
	defaultAgentName = "slurm-drain-monitor"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	configPath = flag.String(
		"config-path",
		"/etc/nvsentinel/config/slurm-drain-monitor.toml",
		"Path to slurm-drain-monitor configuration file",
	)
	metricsBindAddress = flag.String(
		"metrics-bind-address",
		":8080",
		"Address to bind Prometheus metrics endpoint",
	)
	healthProbeBindAddress = flag.String(
		"health-probe-bind-address",
		":8081",
		"Address to bind health probe endpoints",
	)
	resyncPeriod = flag.Duration(
		"resync-period",
		5*time.Minute,
		"Periodic reconciliation interval",
	)
	maxConcurrentReconciles = flag.Int(
		"max-concurrent-reconciles",
		1,
		"Maximum number of pods to reconcile concurrently",
	)
	platformConnectorSocket = flag.String(
		"platform-connector-socket",
		"unix:///var/run/nvsentinel.sock",
		"Platform Connector gRPC socket",
	)
	processingStrategyFlag = flag.String(
		"processing-strategy",
		"EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY",
	)
)

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting slurm-drain-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	params := initializer.Params{
		ConfigPath:              *configPath,
		MetricsBindAddress:      *metricsBindAddress,
		HealthProbeBindAddress:  *healthProbeBindAddress,
		ResyncPeriod:            *resyncPeriod,
		MaxConcurrentReconciles: *maxConcurrentReconciles,
		PlatformConnectorSocket: *platformConnectorSocket,
		ProcessingStrategy:      *processingStrategyFlag,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to initialize components: %w", err)
	}
	defer components.GRPCConn.Close()

	slog.Info("Starting manager")

	if err := components.Manager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start manager: %w", err)
	}

	return nil
}
