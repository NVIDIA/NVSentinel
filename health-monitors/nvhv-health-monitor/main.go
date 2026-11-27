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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/nvhvclient"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/poller"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
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
)

func main() {
	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting nvhv-health-monitor", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	timeout, err := validateFlags()
	if err != nil {
		return fmt.Errorf("failed to validate the command-line flags: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.NewClient(*nvhvAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create the gRPC client for NVHV at %s: %w", *nvhvAddress, err)
	}
	defer conn.Close()

	p := &poller.Poller{
		Evaluator: nvhvclient.New(conn),
		Handle:    logVerdict,
		Interval:  *pollInterval,
		Timeout:   timeout,
	}

	slog.Info("Polling NVHV", "address", *nvhvAddress, "interval", *pollInterval, "timeout", timeout)
	p.Run(ctx)

	return nil
}

// validateFlags checks the flags and returns the effective request timeout.
func validateFlags() (time.Duration, error) {
	if *nodeName == "" {
		return 0, errors.New("node name is required (--node-name or NODE_NAME)")
	}

	if *pollInterval <= 0 {
		return 0, errors.New("poll-interval must be positive")
	}

	timeout := *requestTimeout
	if timeout == 0 {
		timeout = *pollInterval
	}

	if timeout < 0 || timeout > *pollInterval {
		return 0, fmt.Errorf("request-timeout must be positive and at most the poll interval (%s)", *pollInterval)
	}

	return timeout, nil
}

// logVerdict is a placeholder until verdicts are turned into health events.
func logVerdict(_ context.Context, v *verdict.Verdict, err error) {
	if err != nil {
		return
	}

	slog.Info("NVHV verdict", "schemaVersion", v.SchemaVersion, "checks", len(v.Results))
}
