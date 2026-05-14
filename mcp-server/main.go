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
	"github.com/nvidia/nvsentinel/mcp-server/pkg/mcp"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
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
	mcpAddr := flag.String("mcp-addr", ":8080", "MCP streamable-HTTP listen address (e.g. :8080)")
	metricsPort := flag.String("metrics-port", "9090", "Prometheus metrics and health probe listen port")
	authToken := flag.String("auth-token", "", "Bearer token required for /mcp access; empty disables auth")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricsServer, err := CreateMetricsServer(*metricsPort)
	if err != nil {
		return fmt.Errorf("create metrics server: %w", err)
	}

	// TODO(Task 6+): replace store.NewFakeReader() with a real
	// store.DataStoreReader built from the store-client provider registry
	// (see event-exporter/pkg/initializer for the pattern). Tool tasks 6-16
	// also wire k8sClient via in-cluster config + kubernetes.NewForConfig.
	mcpServer, err := mcp.New(mcp.Config{
		Version:   version,
		GitCommit: commit,
		HTTPAddr:  *mcpAddr,
		AuthToken: *authToken,
		Store:     store.NewFakeReader(),
		K8sClient: nil,
	})
	if err != nil {
		return fmt.Errorf("create mcp server: %w", err)
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
		slog.Info("MCP server starting", "addr", *mcpAddr, "auth", *authToken != "")

		if err := mcpServer.Run(gCtx); err != nil {
			if gCtx.Err() != nil {
				slog.Info("MCP server stopped on context cancellation")
				return nil
			}

			slog.Error("MCP server failed", "error", err)

			return fmt.Errorf("mcp server: %w", err)
		}

		return nil
	})

	waitErr := g.Wait()

	if shutdownErr := mcpServer.Shutdown(); shutdownErr != nil {
		slog.Warn("MCP server shutdown returned error", "error", shutdownErr)
	}

	if waitErr != nil && gCtx.Err() == nil {
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
