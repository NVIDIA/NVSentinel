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
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks/state"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/monitor"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

const (
	defaultAgentName            = "nic-health-monitor"
	defaultStatePollingInterval = "1s"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	checksList = flag.String("checks",
		"InfiniBandStateCheck,EthernetStateCheck",
		"Comma-separated list of checks to enable.")
	platformConnectorSocket = flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket")
	nodeNameEnv = flag.String("node-name", os.Getenv("NODE_NAME"),
		"Node name. Defaults to NODE_NAME env var.")
	statePollingIntervalFlag = flag.String("state-polling-interval", defaultStatePollingInterval,
		"Polling interval for state checks (e.g., 1s, 5s)")
	metricsPort = flag.String("metrics-port", "2112",
		"Port to expose Prometheus metrics on")
	configPath = flag.String("config", "/etc/nic-health-monitor/config.toml",
		"Path to TOML configuration file")
	metadataPath = flag.String("metadata-path", "/var/lib/nvsentinel/gpu_metadata.json",
		"Path to the GPU metadata JSON file produced by the metadata collector. "+
			"The NIC Health Monitor fails to start if this file is missing, "+
			"unreadable, or does not contain gpus[] and nic_topology.")
	stateFilePath = flag.String("state-file", statefile.DefaultStateFilePath,
		"Path to the persistent state file (hostPath-backed JSON). Used to "+
			"seed previous-poll port state across pod restarts and to emit "+
			"healthy baselines after host reboots. Missing or corrupt files "+
			"are treated as a fresh boot; monitoring continues regardless.")
	bootIDPath = flag.String("boot-id-path", statefile.DefaultBootIDPath,
		"Path to the kernel boot ID file. Used to detect host reboots (state "+
			"is cleared and healthy baselines are emitted when it changes).")
	processingStrategyFlag = flag.String("processing-strategy", "EXECUTE_REMEDIATION",
		"Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")
)

func main() {
	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting nic-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

// runtimeConfig groups the parsed inputs that run() produces during
// startup so the various wiring steps can take a single argument.
type runtimeConfig struct {
	nodeName           string
	cfg                *config.Config
	processingStrategy pb.ProcessingStrategy
	stateInterval      time.Duration
	metricsPort        int
}

func run() error {
	flag.Parse()
	slog.Info("Parsed command line flags successfully")

	rc, err := parseRuntimeConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reader := sysfs.NewReader(rc.cfg.SysClassInfinibandPath, rc.cfg.SysClassNetPath)

	classifier, err := loadClassifier(reader)
	if err != nil {
		return err
	}

	stateManager, bootIDChanged := loadStateManager()

	conn, err := dialWithRetry(ctx, *platformConnectorSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create gRPC client after retries: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Error("Error closing gRPC connection", "error", closeErr)
		}
	}()

	client := pb.NewPlatformConnectorClient(conn)

	enabledChecks := buildChecks(rc.nodeName, reader, rc.cfg, classifier,
		rc.processingStrategy, stateManager, bootIDChanged)
	if len(enabledChecks) == 0 {
		return fmt.Errorf("no state checks enabled — set --checks to include at least " +
			"InfiniBandStateCheck or EthernetStateCheck")
	}

	nicMonitor := monitor.NewNICHealthMonitor(rc.nodeName, client, enabledChecks,
		rc.stateInterval)

	return runServerAndLoops(ctx, rc, nicMonitor)
}

// loadStateManager constructs the shared persistent-state Manager and
// calls Load. Errors are logged and swallowed: per design the monitor
// continues on a fresh empty state if persistence is unavailable.
//
// The returned bootIDChanged flag is true when the loaded state was
// discarded (missing/corrupt file or host reboot). Checks use it to
// emit healthy baseline events on their first poll so the platform
// can clear stale FATAL conditions.
func loadStateManager() (*statefile.Manager, bool) {
	mgr := statefile.NewManagerWithPaths(*stateFilePath, *bootIDPath)
	if err := mgr.Load(); err != nil {
		slog.Warn("Could not load state file, starting with empty state",
			"path", *stateFilePath, "error", err)
	}

	bootIDChanged := mgr.BootIDChanged()

	slog.Info("State manager initialised",
		"path", *stateFilePath,
		"boot_id_changed", bootIDChanged,
	)

	return mgr, bootIDChanged
}

// parseRuntimeConfig validates flags, loads the on-disk config (with a
// default fallback), and parses duration/port values once so run() does
// not repeat the work.
func parseRuntimeConfig() (*runtimeConfig, error) {
	nodeName := *nodeNameEnv
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME env not set and --node-name flag not provided, cannot run")
	}

	slog.Info("Configuration",
		"node", nodeName,
		"checks", *checksList,
		"configPath", *configPath,
		"metadataPath", *metadataPath,
		"platformConnectorSocket", *platformConnectorSocket,
		"statePollingInterval", *statePollingIntervalFlag,
		"processingStrategy", *processingStrategyFlag,
	)

	cfg := loadConfigOrDefault(*configPath)

	slog.Info("Configuration loaded",
		"sysClassNetPath", cfg.SysClassNetPath,
		"sysClassInfinibandPath", cfg.SysClassInfinibandPath,
	)

	processingStrategy, err := parseProcessingStrategy(*processingStrategyFlag)
	if err != nil {
		return nil, err
	}

	stateInterval, err := time.ParseDuration(*statePollingIntervalFlag)
	if err != nil {
		return nil, fmt.Errorf("invalid state-polling-interval: %w", err)
	}

	if stateInterval <= 0 {
		return nil, fmt.Errorf("state-polling-interval must be > 0, got %s", stateInterval)
	}

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return nil, fmt.Errorf("invalid metrics port: %w", err)
	}

	return &runtimeConfig{
		nodeName:           nodeName,
		cfg:                cfg,
		processingStrategy: processingStrategy,
		stateInterval:      stateInterval,
		metricsPort:        portInt,
	}, nil
}

// loadConfigOrDefault reads the TOML config and falls back to in-memory
// defaults on any error. The fallback preserves current deployments that
// don't ship the ConfigMap yet.
func loadConfigOrDefault(path string) *config.Config {
	cfg, err := config.LoadConfig(path)
	if err == nil {
		return cfg
	}

	slog.Warn("Failed to load config file, using defaults", "error", err, "path", path)

	slog.Info("Using default configuration with CLI flags")

	return &config.Config{
		SysClassInfinibandPath: "/nvsentinel/sys/class/infiniband",
		SysClassNetPath:        "/nvsentinel/sys/class/net",
	}
}

// loadClassifier wraps topology.LoadFromMetadata with an actionable
// error that points operators at the metadata-collector DaemonSet.
func loadClassifier(reader sysfs.Reader) (*topology.Classifier, error) {
	classifier, err := topology.LoadFromMetadata(*metadataPath, reader)
	if err != nil {
		return nil, fmt.Errorf("NIC monitor cannot start without GPU metadata at %s: %w "+
			"(hint: ensure the metadata-collector DaemonSet is running and has "+
			"written nic_topology)", *metadataPath, err)
	}

	return classifier, nil
}

// runServerAndLoops starts the metrics server alongside the state
// polling loop under an errgroup.
func runServerAndLoops(ctx context.Context, rc *runtimeConfig, nicMonitor *monitor.NICHealthMonitor) error {
	srv := server.NewServer(
		server.WithPort(rc.metricsPort),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", rc.metricsPort)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		return pollingLoop(gCtx, "state", rc.stateInterval, nicMonitor.RunStateChecks)
	})

	return g.Wait()
}

// buildChecks instantiates the enabled checks.
func buildChecks(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	stateManager *statefile.Manager,
	bootIDChanged bool,
) []checks.Check {
	var result []checks.Check

	for _, c := range strings.Split(*checksList, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}

		switch c {
		case checks.InfiniBandStateCheckName:
			result = append(result, state.NewInfiniBandStateCheck(
				nodeName, reader, cfg, classifier, processingStrategy,
				stateManager, bootIDChanged,
			))
		case checks.EthernetStateCheckName:
			result = append(result, state.NewEthernetStateCheck(
				nodeName, reader, cfg, classifier, processingStrategy,
				stateManager, bootIDChanged,
			))
		default:
			slog.Warn("Unknown check, skipping", "check", c)
		}
	}

	return result
}

// parseProcessingStrategy maps the string flag to the protobuf enum.
func parseProcessingStrategy(s string) (pb.ProcessingStrategy, error) {
	value, ok := pb.ProcessingStrategy_value[s]
	if !ok {
		return 0, fmt.Errorf("unexpected processing strategy value: %q", s)
	}

	return pb.ProcessingStrategy(value), nil
}

// pollingLoop runs fn at interval until ctx is cancelled.
func pollingLoop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("Starting polling loop", "name", name, "interval", interval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Polling loop stopped", "name", name)
			return nil
		case <-ticker.C:
			if err := fn(ctx); err != nil {
				slog.Error("Poll cycle failed", "name", name, "error", err)
			}
		}
	}
}

// dialWithRetry dials a gRPC target with bounded retries and per-attempt
// timeout. Socket-existence is checked up front for unix:// targets so
// we give a clearer error than "connection refused".
func dialWithRetry(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	const (
		maxRetries        = 10
		perAttemptTimeout = 5 * time.Second
	)

	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Info("Connecting to platform connector",
			"attempt", attempt, "maxRetries", maxRetries, "target", target)

		conn, err := tryDial(ctx, target, perAttemptTimeout, opts...)
		if err == nil {
			slog.Info("Successfully connected to platform connector", "attempt", attempt)
			return conn, nil
		}

		lastErr = err
		slog.Warn("Dial attempt failed", "attempt", attempt, "error", err)

		if attempt < maxRetries {
			if waitErr := backoffWait(ctx, attempt); waitErr != nil {
				return nil, waitErr
			}
		}
	}

	return nil, fmt.Errorf("failed to connect after %d retries: %w", maxRetries, lastErr)
}

// tryDial performs a single connection attempt: socket existence check,
// client creation, and readiness wait.
func tryDial(
	ctx context.Context, target string, timeout time.Duration, opts ...grpc.DialOption,
) (*grpc.ClientConn, error) {
	if strings.HasPrefix(target, "unix://") {
		socketPath := strings.TrimPrefix(target, "unix://")
		if _, err := os.Stat(socketPath); err != nil {
			return nil, fmt.Errorf("socket file not found: %w", err)
		}
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("gRPC NewClient: %w", err)
	}

	if err := waitUntilReady(ctx, conn, timeout); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("client not ready: %w", err)
	}

	return conn, nil
}

// backoffWait sleeps for attempt seconds, aborting early if ctx is cancelled.
func backoffWait(ctx context.Context, attempt int) error {
	t := time.NewTimer(time.Duration(attempt) * time.Second)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitUntilReady blocks until a gRPC connection reaches Ready state.
func waitUntilReady(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	conn.Connect()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}

		if !conn.WaitForStateChange(ctx, state) {
			return ctx.Err()
		}
	}
}
