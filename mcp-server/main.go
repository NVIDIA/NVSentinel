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
	"errors"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/mcp"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
)

const (
	serviceName           = "mcp-server"
	datastoreInitTimeout  = 30 * time.Second
	datastoreCloseTimeout = 10 * time.Second
)

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
	useFakeStore := flag.Bool("use-fake-store", false,
		"Use an in-memory FakeReader instead of connecting to a real datastore "+
			"(local development only; not for production).")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricsServer, err := CreateMetricsServer(*metricsPort)
	if err != nil {
		return fmt.Errorf("create metrics server: %w", err)
	}

	reader, closeStore, err := initStoreReader(ctx, *useFakeStore)
	if err != nil {
		return fmt.Errorf("init store reader: %w", err)
	}
	defer closeStoreSafely(closeStore)

	k8sClient, err := initK8sClient()
	if err != nil {
		return fmt.Errorf("init k8s client: %w", err)
	}

	mcpServer, err := mcp.New(mcp.Config{
		Version:   version,
		GitCommit: commit,
		HTTPAddr:  *mcpAddr,
		AuthToken: *authToken,
		Store:     reader,
		K8sClient: k8sClient,
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
		slog.Info("MCP server starting", "addr", *mcpAddr, "auth", *authToken != "", "k8s", k8sClient != nil)

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

// initStoreReader builds the store.Reader the MCP server reads from. With
// --use-fake-store, returns an in-memory FakeReader. Without, loads the
// datastore config from env vars and constructs the real DataStoreReader
// against the configured provider (Mongo or Postgres, picked by env).
func initStoreReader(ctx context.Context, useFake bool) (store.Reader, func(context.Context) error, error) {
	if useFake {
		slog.Warn("Using FakeReader; all tools will return empty data (dev mode only)")

		return store.NewFakeReader(), func(context.Context) error { return nil }, nil
	}

	cfg, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load datastore config: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, datastoreInitTimeout)
	defer cancel()

	ds, err := datastore.NewDataStore(initCtx, *cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("new datastore: %w", err)
	}

	slog.Info("Datastore initialized", "provider", ds.Provider())

	return store.NewDataStoreReader(ds.HealthEventStore()), ds.Close, nil
}

// initK8sClient builds an in-cluster Kubernetes client. When the binary is
// run outside a cluster (rest.ErrNotInCluster), the function returns a nil
// clientset rather than an error: K8s-dependent tools then return a
// structured "k8s API not configured" warning per their handler contract.
func initK8sClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			slog.Warn("Not running in a cluster; K8s API tools will be disabled")

			return nil, nil //nolint:nilnil // documented "disabled" signal — callers handle nil
		}

		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("new k8s client: %w", err)
	}

	return c, nil
}

// closeStoreSafely runs the datastore close callback with a bounded timeout
// so a hung connection cannot block process shutdown indefinitely.
func closeStoreSafely(closeFn func(context.Context) error) {
	if closeFn == nil {
		return
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), datastoreCloseTimeout)
	defer cancel()

	if err := closeFn(closeCtx); err != nil {
		slog.Warn("datastore close returned error", "error", err)
	}
}
