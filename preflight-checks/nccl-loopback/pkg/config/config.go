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

package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Config holds the configuration for the NCCL loopback preflight check.
type Config struct {
	// BWThresholdGbps is the minimum acceptable bus bandwidth in GB/s.
	// Test fails if measured bandwidth is below this threshold.
	BWThresholdGbps float64

	// TestSizeMB is the message size for the all-reduce test in megabytes.
	TestSizeMB int

	// NumGPUs is the number of GPUs to use in the test.
	// Must match the GPUs visible via NVIDIA_VISIBLE_DEVICES.
	NumGPUs int

	// NCCLTestBinary is the path to the all_reduce_perf binary.
	NCCLTestBinary string

	// ConnectorSocket is the Unix socket path for the Platform Connector.
	ConnectorSocket string

	// NodeName is the Kubernetes node name for health events.
	NodeName string

	// ProcessingStrategy determines how downstream modules handle the event.
	ProcessingStrategy pb.ProcessingStrategy
}

// FromEnv loads configuration from environment variables.
func FromEnv() (*Config, error) {
	cfg := &Config{
		BWThresholdGbps: 150.0,
		TestSizeMB:      256,
		NCCLTestBinary:  "/opt/nccl-tests/build/all_reduce_perf",
	}

	if v := os.Getenv("BW_THRESHOLD_GBPS"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid BW_THRESHOLD_GBPS: %w", err)
		}

		if f <= 0 {
			return nil, fmt.Errorf("BW_THRESHOLD_GBPS must be positive, got %f", f)
		}

		cfg.BWThresholdGbps = f
	}

	if v := os.Getenv("TEST_SIZE_MB"); v != "" {
		i, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid TEST_SIZE_MB: %w", err)
		}

		if i <= 0 {
			return nil, fmt.Errorf("TEST_SIZE_MB must be positive, got %d", i)
		}

		cfg.TestSizeMB = i
	}

	// Detect GPU count at runtime - works for both device plugin and DRA
	detected, err := detectGPUCount()
	if err != nil {
		return nil, fmt.Errorf("failed to detect GPU count: %w", err)
	}

	cfg.NumGPUs = detected

	if v := os.Getenv("NCCL_TEST_BINARY"); v != "" {
		cfg.NCCLTestBinary = v
	}

	cfg.ConnectorSocket = os.Getenv("PLATFORM_CONNECTOR_SOCKET")
	if cfg.ConnectorSocket == "" {
		return nil, fmt.Errorf("PLATFORM_CONNECTOR_SOCKET is required")
	}

	cfg.NodeName = os.Getenv("NODE_NAME")
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("NODE_NAME is required")
	}

	strategyStr := os.Getenv("PROCESSING_STRATEGY")
	if strategyStr == "" {
		strategyStr = "EXECUTE_REMEDIATION"
	}

	strategy, ok := pb.ProcessingStrategy_value[strategyStr]
	if !ok {
		return nil, fmt.Errorf("invalid PROCESSING_STRATEGY: %s", strategyStr)
	}

	cfg.ProcessingStrategy = pb.ProcessingStrategy(strategy)

	return cfg, nil
}

// detectGPUCount uses nvidia-smi to count visible GPUs.
// Works regardless of whether GPUs were allocated via device plugin or DRA.
func detectGPUCount() (int, error) {
	// Use nvidia-smi with CSV format for reliable parsing
	cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	// Count non-empty lines (each line is one GPU)
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return 0, fmt.Errorf("no GPUs found")
	}

	count := len(strings.Split(output, "\n"))

	return count, nil
}
