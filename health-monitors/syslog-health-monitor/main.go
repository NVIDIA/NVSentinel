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
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/stringutil"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	fd "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
)

const (
	defaultAgentName       = "syslog-health-monitor"
	defaultComponentClass  = "GPU"                                // Or a more specific class if applicable
	defaultPollingInterval = "30m"                                // Default polling interval
	defaultStateFilePath   = "/var/run/syslog_monitor/state.json" // Default state file path
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// Command-line flags
	checksList = flag.String("checks", "SysLogsXIDError,SysLogsSXIDError,SysLogsGPUFallenOff",
		"Comma separated listed of checks to enable")
	platformConnectorSocket = flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket.")
	nodeNameEnv         = flag.String("node-name", os.Getenv("NODE_NAME"), "Node name. Defaults to NODE_NAME env var.")
	pollingIntervalFlag = flag.String("polling-interval", defaultPollingInterval,
		"Polling interval for health checks (e.g., 15m, 1h).")
	stateFileFlag = flag.String("state-file", defaultStateFilePath,
		"Path to state file for cursor persistence.")
	metricsPort         = flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	xidAnalyserEndpoint = flag.String("xid-analyser-endpoint", "",
		"Endpoint to the XID analyser service.")
	kataEnabled = flag.String("kata-enabled", "false",
		"Indicates if this monitor is running in Kata Containers mode (set by DaemonSet variant).")
	metadataPath = flag.String("metadata-path", "/var/lib/nvsentinel/gpu_metadata.json",
		"Path to GPU metadata JSON file.")
	processingStrategyFlag = flag.String("processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")
)

var checks []fd.CheckDefinition

func main() {
	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting syslog-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	nodeName, err := validateNodeName()
	if err != nil {
		return err
	}

	root := context.Background()

	ctx, stop := signal.NotifyContext(root, os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := dialPlatformConnector(ctx, *platformConnectorSocket)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Error("Error closing gRPC connection", "error", closeErr)
		}
	}()

	client := pb.NewPlatformConnectorClient(conn)

	checks, err = buildChecksFromFlag()
	if err != nil {
		return err
	}

	checks = applyKataConfig(checks)

	monitor, pollingInterval, err := createSyslogMonitor(nodeName, checks, client)
	if err != nil {
		return err
	}

	srv, portInt, err := createMetricsServer()
	if err != nil {
		return err
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		return runPollingLoop(gCtx, monitor, pollingInterval, checks)
	})

	return g.Wait()
}

func validateNodeName() (string, error) {
	flag.Parse()
	slog.Info("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		return "", fmt.Errorf("NODE_NAME env not set and --node-name flag not provided, cannot run")
	}

	slog.Info("Configuration", "node", nodeName, "kata-enabled", *kataEnabled)

	return nodeName, nil
}

func dialPlatformConnector(ctx context.Context, socket string) (*grpc.ClientConn, error) {
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	slog.Info("Creating gRPC client to platform connector", "socket", socket)

	conn, err := dialWithRetry(ctx, socket, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client after retries: %w", err)
	}

	return conn, nil
}

func buildChecksFromFlag() ([]fd.CheckDefinition, error) {
	list := make([]fd.CheckDefinition, 0)

	for c := range strings.SplitSeq((*checksList), ",") {
		name := strings.TrimSpace(c)
		if name == "" {
			continue
		}

		list = append(list, fd.CheckDefinition{
			Name:        name,
			JournalPath: "/nvsentinel/var/log/journal/",
		})
	}

	if len(list) == 0 {
		return nil, fmt.Errorf("no checks defined in the config file")
	}

	return list, nil
}

func applyKataConfig(list []fd.CheckDefinition) []fd.CheckDefinition {
	if !stringutil.IsTruthyValue(*kataEnabled) {
		return list
	}

	slog.Info("Kata mode enabled, adding containerd service filter and removing SysLogsSXIDError check")

	for i := range list {
		if list[i].Tags == nil {
			list[i].Tags = []string{"-u containerd.service"}
		} else {
			list[i].Tags = append(list[i].Tags, "-u containerd.service")
		}
	}

	filtered := make([]fd.CheckDefinition, 0, len(list))

	for _, check := range list {
		if check.Name != "SysLogsSXIDError" {
			filtered = append(filtered, check)
		}
	}

	return filtered
}

func createSyslogMonitor(
	nodeName string,
	list []fd.CheckDefinition,
	client pb.PlatformConnectorClient,
) (*fd.SyslogMonitor, time.Duration, error) {
	value, ok := pb.ProcessingStrategy_value[*processingStrategyFlag]
	if !ok {
		return nil, 0, fmt.Errorf("unexpected processingStrategy value: %q", *processingStrategyFlag)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", *processingStrategyFlag)

	processingStrategy := pb.ProcessingStrategy(value)

	slog.Info("Creating syslog monitor", "checksCount", len(list))

	monitor, err := fd.NewSyslogMonitor(
		nodeName,
		list,
		client,
		defaultAgentName,
		defaultComponentClass,
		*pollingIntervalFlag,
		*stateFileFlag,
		*xidAnalyserEndpoint,
		*metadataPath,
		processingStrategy,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("error creating syslog health monitor: %w", err)
	}

	pollingInterval, err := time.ParseDuration(*pollingIntervalFlag)
	if err != nil {
		return nil, 0, fmt.Errorf("error parsing polling interval: %w", err)
	}

	slog.Info("Polling interval configured", "interval", pollingInterval)

	return monitor, pollingInterval, nil
}

func createMetricsServer() (server.Server, int, error) {
	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	return srv, portInt, nil
}

func runPollingLoop(
	ctx context.Context,
	monitor *fd.SyslogMonitor,
	interval time.Duration,
	list []fd.CheckDefinition,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("Configured checks", "checks", list)
	slog.Info("Syslog health monitor initialization complete, starting polling loop...")

	var backoff time.Duration

	for {
		select {
		case <-ctx.Done():
			slog.Info("Polling loop stopped due to context cancellation")

			return nil
		case <-ticker.C:
			slog.Info("Performing scheduled health check run...")

			for {
				if err := monitor.Run(); err != nil {
					if backoff == 0 {
						backoff = 2 * time.Second
					} else {
						backoff *= 2
					}

					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}

					slog.Error("Health check run failed; will retry after backoff", "error", err, "backoff", backoff)

					timer := time.NewTimer(backoff)

					select {
					case <-ctx.Done():
						timer.Stop()
						slog.Info("Polling loop stopped during backoff due to context cancellation")

						return nil
					case <-timer.C:
					}

					continue
				}

				backoff = 0

				break
			}
		}
	}
}

// dialWithRetry dials a gRPC target with bounded retries and per-attempt timeout.
// It also verifies a unix domain socket path exists when scheme unix:// is used.
func dialWithRetry(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	const (
		maxRetries        = 10
		perAttemptTimeout = 5 * time.Second
	)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Info("Checking platform connector socket availability",
			"attempt", attempt,
			"maxRetries", maxRetries,
			"target", target,
		)

		// For unix:// ensure the socket path exists before dialing.
		if strings.HasPrefix(target, "unix://") {
			socketPath := strings.TrimPrefix(target, "unix://")
			if _, statErr := os.Stat(socketPath); statErr != nil {
				slog.Warn("Platform connector socket file does not exist",
					"attempt", attempt, "maxRetries", maxRetries, "error", statErr)

				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt) * time.Second)
					continue
				}

				return nil, fmt.Errorf("platform connector socket file not found after retries: %w", statErr)
			}
		}

		// Create client connection (non-blocking).
		conn, err := grpc.NewClient(target, opts...)
		if err != nil {
			slog.Warn("Error creating gRPC client", "attempt", attempt, "maxRetries", maxRetries, "error", err)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("failed to create gRPC client after retries: %w", err)
		}

		// Actively connect and wait until Ready (or timeout/cancel).
		if err := waitUntilReady(ctx, conn, perAttemptTimeout); err != nil {
			_ = conn.Close()

			slog.Warn("gRPC client not ready before timeout",
				"attempt", attempt,
				"maxRetries", maxRetries,
				"error", err,
			)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("gRPC client not ready after retries: %w", err)
		}

		slog.Info("Successfully connected to platform connector", "attempt", attempt)

		return conn, nil
	}

	// Unreachable, but keeps compiler happy.
	return nil, fmt.Errorf("exhausted retries without creating gRPC client")
}

// waitUntilReady triggers connection establishment and blocks until the ClientConn
// reaches connectivity.Ready or the timeout/context expires.
func waitUntilReady(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	conn.Connect()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}

		// Wait for a state change or context expiry.
		if !conn.WaitForStateChange(ctx, state) {
			// Context expired or canceled.
			return ctx.Err()
		}
	}
}
