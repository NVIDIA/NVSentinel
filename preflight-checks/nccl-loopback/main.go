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
	"fmt"
	"log/slog"
	"os"

	"github.com/nvidia/nvsentinel/preflight-checks/nccl-loopback/pkg/benchmark"
	"github.com/nvidia/nvsentinel/preflight-checks/nccl-loopback/pkg/config"
	"github.com/nvidia/nvsentinel/preflight-checks/nccl-loopback/pkg/health"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const (
	exitSuccess     = 0
	exitTestFailed  = 1
	exitConfigError = 2
)

func main() {
	// Configure structured logging
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))

	slog.Info("Starting preflight NCCL loopback check",
		"version", version,
		"commit", commit,
		"date", date)

	exitCode := run()
	os.Exit(exitCode)
}

func run() int {
	ctx := context.Background()

	// Load configuration
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("Configuration error", "error", err)
		return exitConfigError
	}

	slog.Info("Configuration loaded",
		"bw_threshold_gbps", cfg.BWThresholdGbps,
		"test_size_mb", cfg.TestSizeMB,
		"num_gpus", cfg.NumGPUs,
		"binary", cfg.NCCLTestBinary,
		"node_name", cfg.NodeName)

	// Create health reporter
	reporter := health.NewReporter(
		cfg.ConnectorSocket,
		cfg.NodeName,
		cfg.ProcessingStrategy,
	)

	// Run benchmark
	runner := benchmark.NewRunner(cfg.NCCLTestBinary)

	result, err := runner.Run(cfg.NumGPUs, cfg.TestSizeMB)
	if err != nil {
		slog.Error("NCCL benchmark failed", "error", err)

		// Send failure health event
		sendErr := reporter.SendEvent(ctx,
			false, // isHealthy
			true,  // isFatal
			fmt.Sprintf("NCCL loopback test failed: %v", err),
			"NCCL_TEST_ERROR",
		)
		if sendErr != nil {
			slog.Error("Failed to send health event", "error", sendErr)
		}

		return exitTestFailed
	}

	slog.Info("Benchmark completed",
		"bus_bandwidth_gbps", result.BusBandwidthGbps,
		"algo_bandwidth_gbps", result.AlgoBandwidthGbps,
		"num_gpus", result.NumGPUs,
		"test_size_bytes", result.TestSizeBytes)

	// Check if bandwidth meets threshold
	if result.BusBandwidthGbps < cfg.BWThresholdGbps {
		slog.Error("NCCL loopback test FAILED: bandwidth below threshold",
			"measured_gbps", result.BusBandwidthGbps,
			"threshold_gbps", cfg.BWThresholdGbps)

		message := fmt.Sprintf(
			"NCCL all-reduce bus bandwidth %.2f GB/s is below threshold %.2f GB/s",
			result.BusBandwidthGbps,
			cfg.BWThresholdGbps,
		)

		sendErr := reporter.SendEvent(ctx,
			false, // isHealthy
			true,  // isFatal
			message,
			"NCCL_BW_DEGRADED",
		)
		if sendErr != nil {
			slog.Error("Failed to send health event", "error", sendErr)
		}

		return exitTestFailed
	}

	// Test passed
	slog.Info("NCCL loopback test PASSED",
		"measured_gbps", result.BusBandwidthGbps,
		"threshold_gbps", cfg.BWThresholdGbps)

	// Send success health event
	message := fmt.Sprintf(
		"NCCL all-reduce bus bandwidth %.2f GB/s meets threshold %.2f GB/s",
		result.BusBandwidthGbps,
		cfg.BWThresholdGbps,
	)

	sendErr := reporter.SendEvent(ctx,
		true,  // isHealthy
		false, // isFatal
		message,
		"",
	)
	if sendErr != nil {
		slog.Error("Failed to send health event", "error", sendErr)
		// Don't fail the test just because health event failed to send
	}

	return exitSuccess
}
