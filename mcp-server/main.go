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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
)

const serviceName = "mcp-server"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLoggerWithTraceCorrelation(serviceName, version)
	slog.Info("NVSentinel MCP Server starting", "version", version, "commit", commit, "date", date)

	if err := tracing.InitTracing(serviceName); err != nil {
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

func run() error {
	httpPort := flag.String("port", "8080", "MCP HTTP/SSE listen port (wired in Task 4)")
	metricsPort := flag.String("metrics-port", "9090", "Prometheus metrics and health probe listen port")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricsServer, err := CreateMetricsServer(*metricsPort)
	if err != nil {
		return fmt.Errorf("create metrics server: %w", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics and health server", "port", *metricsPort)

		if err := metricsServer.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		slog.Info("MCP transport not yet wired (Task 4)", "reserved_port", *httpPort)
		<-gCtx.Done()

		return nil
	})

	if waitErr := g.Wait(); waitErr != nil && gCtx.Err() == nil {
		return fmt.Errorf("server group: %w", waitErr)
	}

	slog.Info("Shutdown complete")

	return nil
}

func CreateMetricsServer(port string) (server.Server, error) {
	portInt, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid metrics port %q: %w", port, err)
	}

	return server.NewServer(
		server.WithPort(portInt),
		server.WithSimpleHealth(),
		server.WithHandler("/metrics", promhttp.Handler()),
	), nil
}
