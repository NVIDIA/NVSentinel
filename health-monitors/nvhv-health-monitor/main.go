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

// nvhv-health-monitor polls NVHV (health-validation serve) over loopback gRPC
// for a node verdict and turns it into NVSentinel health events (ADR-076).
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

	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	featureflags "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/initializer"
)

const defaultAgentName = "nvhv-health-monitor"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	nvhvAddress = flag.String("nvhv-address", "127.0.0.1:18090",
		"Loopback address of NVHV's gRPC verdict API")
	pollInterval = flag.Duration("poll-interval", 15*time.Second,
		"How often to call NVHV")
	requestTimeout = flag.Duration("request-timeout", 0,
		"Timeout of one NVHV call; defaults to the poll interval and may not exceed it")
	nodeName = flag.String("node-name", os.Getenv("NODE_NAME"),
		"Node name. Defaults to the NODE_NAME environment variable")
	unreachableAfter = flag.Duration("unreachable-threshold", 5*time.Minute,
		"How long NVHV may stay unreachable before an NVHVUnreachable event is sent")
	collectionFailureAfter = flag.Duration("collection-failure-threshold", 5*time.Minute,
		"How long a kind of check may keep failing to collect its data, while NVHV answers, "+
			"before an NVHV<kind>CollectionFailure event is sent")
	stateFile = flag.String("state-file", "/var/lib/nvhv-health-monitor/state.json",
		"Keeps which outage events (NVHVUnreachable, NVHV<kind>CollectionFailure) are raised, "+
			"so a restarted monitor still sends their healthy events. Empty disables it")
	processingStrategy = flag.String("processing-strategy", "STORE_ONLY",
		"Processing strategy of every event: STORE_ONLY (record only) or EXECUTE_REMEDIATION. "+
			"Faults get no recovery event, so only use EXECUTE_REMEDIATION if something else clears them")
	metricsPort = flag.Int("metrics-port", 2112,
		"Port for Prometheus metrics and the /healthz endpoint")
	platformConnectorSocket = flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Platform connector target; HEALTH_PUBLISH_TARGET selects a central connector instead")
	platformConnectorTokenPath = flag.String("platform-connector-token-path", "",
		"Path to a projected ServiceAccount token presented to the platform connector. Empty disables it")
)

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting nvhv-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	featureflags.NewRegistry(defaultAgentName).SetStoreOnlyMode(*processingStrategy)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	components, err := initializer.InitializeAll(initializer.Params{
		NVHVAddress:             *nvhvAddress,
		NodeName:                *nodeName,
		PollInterval:            *pollInterval,
		RequestTimeout:          *requestTimeout,
		UnreachableAfter:        *unreachableAfter,
		CollectionFailureAfter:  *collectionFailureAfter,
		MetricsPort:             *metricsPort,
		StateFile:               *stateFile,
		ProcessingStrategy:      *processingStrategy,
		PlatformConnectorSocket: *platformConnectorSocket,
		PlatformConnectorToken:  *platformConnectorTokenPath,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize components: %w", err)
	}

	defer components.Close()

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", *metricsPort)

		if err := components.Server.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		slog.Info("Polling NVHV", "address", *nvhvAddress, "interval", *pollInterval)
		components.Poller.Run(gCtx)

		return nil
	})

	return g.Wait()
}
