// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package main implements the Device API Server.
//
// The Device API Server is a node-local gRPC cache server deployed as a
// Kubernetes DaemonSet. It acts as an intermediary between providers
// (health monitors) that update GPU device states and consumers
// (device plugins, DRA drivers) that read device states.
//
// Key features:
//   - Read-blocking semantics: Reads are blocked during provider updates
//     to prevent consumers from reading stale data
//   - Multiple provider support: Multiple health monitors can update
//     different conditions on the same GPUs
//   - Multiple consumer support: Device plugins, DRA drivers, and other
//     consumers can read and watch GPU states
//   - Observability: Prometheus metrics, structured logging with klog/v2
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/nvidia/nvsentinel/pkg/deviceapiserver"
	"github.com/nvidia/nvsentinel/pkg/version"
)

const (
	// ComponentName is the name of this component for logging.
	ComponentName = "device-api-server"
)

func main() {
	// Create config with defaults
	config := deviceapiserver.DefaultConfig()

	// Initialize klog flags first
	klog.InitFlags(nil)

	// Bind our flags
	showVersion := flag.Bool("version", false, "Show version and exit")

	// Manual duration flags (since BindFlags can't handle them properly)
	var shutdownTimeout, shutdownDelay int
	flag.StringVar(&config.GRPCAddress, "grpc-address", config.GRPCAddress,
		"TCP address for gRPC server (e.g., :50051)")
	flag.StringVar(&config.UnixSocket, "unix-socket", config.UnixSocket,
		"Path to Unix socket for node-local IPC (empty to disable)")
	flag.IntVar(&config.HealthPort, "health-port", config.HealthPort,
		"Port for HTTP health endpoints (/healthz, /readyz)")
	flag.IntVar(&config.MetricsPort, "metrics-port", config.MetricsPort,
		"Port for Prometheus metrics (/metrics)")
	flag.IntVar(&shutdownTimeout, "shutdown-timeout", int(config.ShutdownTimeout.Seconds()),
		"Maximum time in seconds to wait for graceful shutdown")
	flag.IntVar(&shutdownDelay, "shutdown-delay", int(config.ShutdownDelay.Seconds()),
		"Time in seconds to wait before starting shutdown (for k8s readiness propagation)")
	flag.StringVar(&config.LogFormat, "log-format", config.LogFormat,
		"Log output format: text or json")
	flag.StringVar(&config.NodeName, "node-name", config.NodeName,
		"Kubernetes node name (defaults to NODE_NAME env var)")

	// NVML provider flags
	flag.BoolVar(&config.NVMLEnabled, "enable-nvml-provider", config.NVMLEnabled,
		"Enable the built-in NVML provider for device enumeration and health monitoring")
	flag.StringVar(&config.NVMLDriverRoot, "nvml-driver-root", config.NVMLDriverRoot,
		"Root path where NVIDIA driver libraries are located")
	flag.StringVar(&config.NVMLIgnoredXids, "nvml-ignored-xids", config.NVMLIgnoredXids,
		"Comma-separated list of additional XID error codes to ignore")
	flag.BoolVar(&config.NVMLHealthCheckEnabled, "nvml-health-check", config.NVMLHealthCheckEnabled,
		"Enable XID event monitoring for health checks")

	flag.Parse()

	// Apply duration conversions
	config.ShutdownTimeout = time.Duration(shutdownTimeout) * time.Second
	config.ShutdownDelay = time.Duration(shutdownDelay) * time.Second

	// Apply environment overrides
	config.ApplyEnvironment()

	// Handle version flag
	if *showVersion {
		v := version.Get()
		if config.LogFormat == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(v); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to encode version: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Println(v.String())
		}
		os.Exit(0)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid configuration: %v\n", err)
		os.Exit(1)
	}

	// Configure klog for JSON output if requested
	if config.LogFormat == "json" {
		// Set JSON format flags
		flag.Set("logging_format", "json")
	}

	// Create root logger with component name
	logger := klog.Background().WithName(ComponentName)
	if config.NodeName != "" {
		logger = logger.WithValues("node", config.NodeName)
	}

	ctx := klog.NewContext(context.Background(), logger)

	// Log startup
	versionInfo := version.Get()
	logger.Info("Starting server",
		"version", versionInfo.Version,
		"commit", versionInfo.GitCommit,
		"buildDate", versionInfo.BuildDate,
		"config", map[string]interface{}{
			"grpcAddress":        config.GRPCAddress,
			"unixSocket":         config.UnixSocket,
			"healthPort":         config.HealthPort,
			"metricsPort":        config.MetricsPort,
			"shutdownTimeout":    config.ShutdownTimeout.String(),
			"shutdownDelay":      config.ShutdownDelay.String(),
			"logFormat":          config.LogFormat,
			"nvmlEnabled":        config.NVMLEnabled,
			"nvmlDriverRoot":     config.NVMLDriverRoot,
			"nvmlHealthCheck":    config.NVMLHealthCheckEnabled,
		},
	)

	// Set up signal handling for graceful shutdown
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create and start server
	server := deviceapiserver.New(config, logger)
	if err := server.Start(ctx); err != nil {
		logger.Error(err, "Server error")
		os.Exit(1)
	}

	logger.Info("Server stopped gracefully")
}
