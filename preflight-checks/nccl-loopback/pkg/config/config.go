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
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Config holds the configuration for the NCCL loopback preflight check.
type Config struct {
	// BWThresholdGbps is the minimum acceptable bus bandwidth in GB/s.
	// Test fails if measured bandwidth is below this threshold.
	BWThresholdGbps float64

	// SkipBandwidthCheck skips the bandwidth threshold validation.
	// When true, the test passes as long as the NCCL benchmark completes successfully.
	SkipBandwidthCheck bool

	// TestSizeMB is the message size for the all-reduce test in megabytes.
	TestSizeMB int

	// NumGPUs is the number of GPUs to use in the test.
	// Must match the GPUs visible via NVIDIA_VISIBLE_DEVICES.
	NumGPUs int

	// NCCLTestBinary is the path to the all_reduce_perf binary.
	NCCLTestBinaryPath string

	// ConnectorSocket is the Unix socket path for the Platform Connector.
	ConnectorSocket string

	// NodeName is the Kubernetes node name for health events.
	NodeName string

	// ProcessingStrategy determines how downstream modules handle the event.
	ProcessingStrategy pb.ProcessingStrategy
}

// FromEnv loads configuration from environment variables.
func FromEnv(ctx context.Context) (*Config, error) {
	bwThreshold, err := parsePositiveFloat("BW_THRESHOLD_GBPS", 150.0)
	if err != nil {
		return nil, err
	}

	skipBWCheck := parseBool("SKIP_BANDWIDTH_CHECK", false)

	testSize, err := parsePositiveInt("TEST_SIZE_MB", 256)
	if err != nil {
		return nil, err
	}

	numGPUs, err := detectGPUCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to detect GPU count: %w", err)
	}

	binaryPath, err := parseBinaryPath()
	if err != nil {
		return nil, err
	}

	connectorSocket, err := requireEnv("PLATFORM_CONNECTOR_SOCKET")
	if err != nil {
		return nil, err
	}

	nodeName, err := requireEnv("NODE_NAME")
	if err != nil {
		return nil, err
	}

	strategy, err := parseProcessingStrategy()
	if err != nil {
		return nil, err
	}

	return &Config{
		BWThresholdGbps:    bwThreshold,
		SkipBandwidthCheck: skipBWCheck,
		TestSizeMB:         testSize,
		NumGPUs:            numGPUs,
		NCCLTestBinaryPath: binaryPath,
		ConnectorSocket:    connectorSocket,
		NodeName:           nodeName,
		ProcessingStrategy: strategy,
	}, nil
}

func parsePositiveFloat(envKey string, defaultVal float64) (float64, error) {
	v := os.Getenv(envKey)
	if v == "" {
		slog.Debug("Using default value for env var", "key", envKey, "default", defaultVal)
		return defaultVal, nil
	}

	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Error("Failed to parse env var as float", "key", envKey, "value", v, "error", err)
		return 0, fmt.Errorf("invalid %s: %w", envKey, err)
	}

	if f <= 0 {
		slog.Error("Env var must be positive", "key", envKey, "value", f)
		return 0, fmt.Errorf("%s must be positive, got %f", envKey, f)
	}

	slog.Debug("Parsed env var", "key", envKey, "value", f)

	return f, nil
}

func parsePositiveInt(envKey string, defaultVal int) (int, error) {
	v := os.Getenv(envKey)
	if v == "" {
		slog.Debug("Using default value for env var", "key", envKey, "default", defaultVal)
		return defaultVal, nil
	}

	i, err := strconv.Atoi(v)
	if err != nil {
		slog.Error("Failed to parse env var as int", "key", envKey, "value", v, "error", err)
		return 0, fmt.Errorf("invalid %s: %w", envKey, err)
	}

	if i <= 0 {
		slog.Error("Env var must be positive", "key", envKey, "value", i)
		return 0, fmt.Errorf("%s must be positive, got %d", envKey, i)
	}

	slog.Debug("Parsed env var", "key", envKey, "value", i)

	return i, nil
}

func parseBool(envKey string, defaultVal bool) bool {
	v := os.Getenv(envKey)
	if v == "" {
		slog.Debug("Using default value for env var", "key", envKey, "default", defaultVal)
		return defaultVal
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("Failed to parse env var as bool, using default", "key", envKey, "value", v, "default", defaultVal)
		return defaultVal
	}

	slog.Debug("Parsed env var", "key", envKey, "value", b)

	return b
}

func parseBinaryPath() (string, error) {
	path := os.Getenv("NCCL_TEST_BINARY_PATH")
	if path == "" {
		path = "/opt/nccl-tests/build/all_reduce_perf"
	}

	if err := validateExecutable(path); err != nil {
		slog.Error("Binary validation failed", "path", path, "error", err)
		return "", fmt.Errorf("invalid NCCL_TEST_BINARY_PATH: %w", err)
	}

	slog.Debug("Binary validated successfully", "path", path)

	return path, nil
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("Required env var not set", "key", key)
		return "", fmt.Errorf("%s is required", key)
	}

	slog.Debug("Loaded required env var", "key", key)

	return v, nil
}

func parseProcessingStrategy() (pb.ProcessingStrategy, error) {
	strategyStr := os.Getenv("PROCESSING_STRATEGY")
	if strategyStr == "" {
		strategyStr = "EXECUTE_REMEDIATION"
		slog.Debug("Using default processing strategy", "strategy", strategyStr)
	}

	strategy, ok := pb.ProcessingStrategy_value[strategyStr]
	if !ok {
		slog.Error("Invalid processing strategy", "strategy", strategyStr)
		return 0, fmt.Errorf("invalid PROCESSING_STRATEGY: %s", strategyStr)
	}

	slog.Debug("Parsed processing strategy", "strategy", strategyStr)

	return pb.ProcessingStrategy(strategy), nil
}

// detectGPUCount uses nvidia-smi to count visible GPUs.
// Works regardless of whether GPUs were allocated via device plugin or DRA.
//
// We use nvidia-smi instead of go-nvml to avoid CGO version compatibility issues
// with NVML libraries across different driver versions. For this simple use case
// (just counting GPUs), shelling out to nvidia-smi is more portable.
//
// Example output:
//
//	$ nvidia-smi --query-gpu=name --format=csv,noheader
//	NVIDIA H100 80GB HBM3
//	NVIDIA H100 80GB HBM3
//	NVIDIA H100 80GB HBM3
//	NVIDIA H100 80GB HBM3
func detectGPUCount(ctx context.Context) (int, error) {
	slog.Debug("Detecting GPU count using nvidia-smi")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")

	var stdout bytes.Buffer

	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Error("nvidia-smi timed out", "timeout", "30s")
			return 0, fmt.Errorf("nvidia-smi timed out after 30s")
		}

		slog.Error("nvidia-smi command failed", "error", err)

		return 0, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		slog.Error("nvidia-smi returned empty output, no GPUs found")
		return 0, fmt.Errorf("no GPUs found")
	}

	count := len(strings.Split(output, "\n"))
	slog.Info("Detected GPUs", "count", count)

	return count, nil
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		slog.Error("File stat failed", "path", path, "error", err)
		return fmt.Errorf("file not found: %w", err)
	}

	if info.IsDir() {
		slog.Error("Path is a directory", "path", path)
		return fmt.Errorf("path is a directory, not an executable")
	}

	if info.Mode()&0111 == 0 {
		slog.Error("File is not executable", "path", path, "mode", info.Mode())
		return fmt.Errorf("file is not executable")
	}

	return nil
}
