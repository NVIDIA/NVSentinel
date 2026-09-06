// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// The nvcre-certification-monitor command runs the NVCRE Certification Monitor,
// a health monitor that turns terminal NVCRE Certification results into
// NVSentinel health events.
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

	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	metrics "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/initializer"
)

const (
	defaultAgentName = "nvcre-certification-monitor"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

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
	resyncInterval = flag.Duration(
		"resync-interval",
		15*time.Minute,
		"Periodic resync interval for full reconciliation",
	)
	platformConnectorSocket = flag.String(
		"platform-connector-socket",
		"unix:///var/run/nvsentinel.sock",
		"Platform Connector gRPC socket",
	)
	platformConnectorTokenPath = flag.String(
		"platform-connector-token-path",
		"",
		"Path to a projected ServiceAccount token presented to platform-connector. "+
			"Required for reporting health events about nodes other than the one this pod runs on; "+
			"empty disables token authentication.",
	)
	processingStrategyFlag = flag.String(
		"processing-strategy",
		"EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY",
	)
	configPath = flag.String(
		"config",
		"/etc/nvcre-certification-monitor/config.toml",
		"Path to the policy configuration file",
	)
)

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting nvcre-certification-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ff := metrics.NewRegistry(defaultAgentName,
		metrics.WithRegisterer(crmetrics.Registry),
	)
	ff.SetStoreOnlyMode(*processingStrategyFlag)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	params := initializer.Params{
		MetricsBindAddress:      *metricsBindAddress,
		HealthProbeBindAddress:  *healthProbeBindAddress,
		ResyncInterval:          *resyncInterval,
		PlatformConnectorSocket: *platformConnectorSocket,
		PlatformConnectorToken:  *platformConnectorTokenPath,
		ProcessingStrategy:      *processingStrategyFlag,
		ConfigPath:              *configPath,
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
