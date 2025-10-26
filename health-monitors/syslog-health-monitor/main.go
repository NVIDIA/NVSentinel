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

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	fd "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
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
	configFile = flag.String("config-file", "/etc/config/config.yaml",
		"Path to the YAML configuration file for log checks.")
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
)

// ConfigFile matches the top-level structure of the YAML config file
type ConfigFile struct {
	Checks []fd.CheckDefinition `yaml:"checks"`
}

func main() {
	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting syslog-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

//nolint:cyclop,gocognit // function coordinates process wiring, IO, and retries
func run() error {
	flag.Parse()
	slog.Info("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME env not set and --node-name flag not provided, cannot run")
	}

	slog.Info("Using node name", "node", nodeName)

	// Root context canceled on SIGINT/SIGTERM so goroutines can exit cleanly.
	root := context.Background()
	ctx, stop := signal.NotifyContext(root, os.Interrupt, syscall.SIGTERM)

	defer stop()

	// Build gRPC dial options (mTLS can replace insecure credentials in production).
	var dialOpts []grpc.DialOption
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	// Create gRPC client to platform connector with retries and per-attempt timeout.
	slog.Info("Creating gRPC client to platform connector", "socket", *platformConnectorSocket)

	conn, err := dialWithRetry(ctx, *platformConnectorSocket, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to create gRPC client after retries: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Error("Error closing gRPC connection", "error", closeErr)
		}
	}()

	client := pb.NewPlatformConnectorClient(conn)

	slog.Info("Loading checks from config file", "file", *configFile)

	config, err := loadConfigWithRetry(ctx, *configFile)
	if err != nil {
		return err
	}

	if len(config.Checks) == 0 {
		return fmt.Errorf("no checks defined in the config file")
	}

	slog.Info("Creating syslog monitor", "checksCount", len(config.Checks))

	fdHealthMonitor, err := fd.NewSyslogMonitor(
		nodeName,
		config.Checks,
		client,
		defaultAgentName,
		defaultComponentClass,
		*pollingIntervalFlag,
		*stateFileFlag,
		*xidAnalyserEndpoint,
	)
	if err != nil {
		return fmt.Errorf("error creating syslog health monitor: %w", err)
	}

	pollingInterval, err := time.ParseDuration(*pollingIntervalFlag)
	if err != nil {
		return fmt.Errorf("error parsing polling interval: %w", err)
	}

	slog.Info("Polling interval configured", "interval", pollingInterval)

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	// Run the HTTP server and the polling loop under an errgroup bound to ctx.
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

	// Polling loop with context-aware cancellation and tolerant error handling.
	g.Go(func() error {
		ticker := time.NewTicker(pollingInterval)
		defer ticker.Stop()

		slog.Info("Configured checks", "checks", config.Checks)

		slog.Info(
			"Syslog health monitor initialization complete, starting polling loop...",
		)

		// Simple backoff for transient Run() errors.
		var backoff time.Duration

		for {
			select {
			case <-gCtx.Done():
				slog.Info("Polling loop stopped due to context cancellation")
				return nil // graceful shutdown (do not surface as error)
			case <-ticker.C:
				slog.Info("Performing scheduled health check run...")

				if err := fdHealthMonitor.Run(); err != nil {
					// Log and continue; apply a capped backoff to avoid hot-looping on persistent failures.
					if backoff == 0 {
						backoff = 2 * time.Second
					} else {
						backoff *= 2
					}

					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}

					slog.Error(
						"Health check run failed; will retry after backoff",
						"error", err,
						"backoff", backoff,
					)

					timer := time.NewTimer(backoff)

					select {
					case <-gCtx.Done():
						timer.Stop()
						slog.Info("Polling loop stopped during backoff due to context cancellation")

						return nil
					case <-timer.C:
					}

					continue
				}

				// On success, reset backoff.
				backoff = 0
			}
		}
	})

	// Wait until either goroutine returns.
	return g.Wait()
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

// loadConfigWithRetry reads and unmarshals the YAML config with bounded retries.
func loadConfigWithRetry(ctx context.Context, path string) (*ConfigFile, error) {
	const (
		maxConfigRetries = 5
	)

	var (
		yamlFile []byte
		err      error
	)

	for attempt := 1; attempt <= maxConfigRetries; attempt++ {
		slog.Info("Reading config file", "attempt", attempt, "maxRetries", maxConfigRetries, "file", path)

		if _, statErr := os.Stat(path); statErr != nil {
			slog.Warn("Config file does not exist", "attempt", attempt, "maxRetries", maxConfigRetries, "error", statErr)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("config file not found after retries: %w", statErr)
		}

		yamlFile, err = os.ReadFile(path)
		if err != nil {
			slog.Warn("Error reading config file", "attempt", attempt, "maxRetries", maxConfigRetries, "error", err)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("failed to read config file after retries: %w", err)
		}

		slog.Info("Successfully read config file", "attempt", attempt)

		break
	}

	var config ConfigFile

	if err := yaml.Unmarshal(yamlFile, &config); err != nil {
		return nil, fmt.Errorf("error unmarshalling config file: %w", err)
	}

	return &config, nil
}
