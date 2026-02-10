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

package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result holds the results of an NCCL all-reduce benchmark.
type Result struct {
	// BusBandwidthGbps is the measured bus bandwidth in GB/s.
	BusBandwidthGbps float64

	// AlgoBandwidthGbps is the algorithm bandwidth in GB/s.
	AlgoBandwidthGbps float64

	// NumGPUs is the number of GPUs used in the test.
	NumGPUs int

	// TestSizeBytes is the message size used for the test.
	TestSizeBytes int64

	// RawOutput is the full output from all_reduce_perf.
	RawOutput string
}

// Runner executes NCCL benchmarks.
type Runner struct {
	binaryPath string
}

// NewRunner creates a new benchmark runner.
func NewRunner(binaryPath string) *Runner {
	return &Runner{binaryPath: binaryPath}
}

var gpuRankPattern = regexp.MustCompile(`#\s+Rank\s+\d+\s+Group`)

const benchmarkTimeout = 5 * time.Minute

// Run executes the all_reduce_perf benchmark and returns the results.
// numGPUs specifies how many GPUs to use (must match NVIDIA_VISIBLE_DEVICES count).
// testSizeMB specifies the message size in megabytes.
func (r *Runner) Run(ctx context.Context, numGPUs, testSizeMB int) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, benchmarkTimeout)
	defer cancel()

	sizeArg := fmt.Sprintf("%dM", testSizeMB)

	//nolint:gosec // Validated binary path
	cmd := exec.CommandContext(ctx, r.binaryPath,
		"-b", sizeArg,
		"-e", sizeArg,
		"-g", strconv.Itoa(numGPUs),
	)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Info("Running NCCL all-reduce benchmark",
		"binary", r.binaryPath,
		"size_mb", testSizeMB,
		"num_gpus", numGPUs,
		"timeout", benchmarkTimeout)

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Error("NCCL benchmark timed out", "timeout", benchmarkTimeout)
			return nil, fmt.Errorf("all_reduce_perf timed out after %s", benchmarkTimeout)
		}

		// NCCL errors often go to stdout, so log both
		slog.Error("NCCL benchmark failed",
			"error", err,
			"stdout", stdout.String(),
			"stderr", stderr.String())

		return nil, fmt.Errorf("all_reduce_perf failed: %w\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	output := stdout.String()
	slog.Debug("Benchmark output", "output", output)

	result, err := parseOutput(output, testSizeMB)
	if err != nil {
		return nil, fmt.Errorf("failed to parse output: %w", err)
	}

	result.RawOutput = output

	return result, nil
}

// parseOutput extracts benchmark results from all_reduce_perf output.
//
// Example output line:
//
//	268435456  67108864  float  sum  -1  2362.7  113.62  198.83  0  2354.8  113.99  199.49  0
//
// Columns: size, count, type, redop, root, time, algbw, busbw, wrong, time, algbw, busbw, wrong
func parseOutput(output string, testSizeMB int) (*Result, error) {
	expectedSize := int64(testSizeMB) * 1024 * 1024

	numGPUs := countGPUs(output)
	if numGPUs == 0 {
		return nil, fmt.Errorf("could not determine number of GPUs from output")
	}

	algbw, busbw, err := extractBandwidth(output, expectedSize)
	if err != nil {
		return nil, err
	}

	return &Result{
		BusBandwidthGbps:  busbw,
		AlgoBandwidthGbps: algbw,
		NumGPUs:           numGPUs,
		TestSizeBytes:     expectedSize,
	}, nil
}

func countGPUs(output string) int {
	count := 0

	for line := range strings.SplitSeq(output, "\n") {
		if gpuRankPattern.MatchString(line) {
			count++
		}
	}

	return count
}

// extractBandwidth parses the data line matching expectedSize and returns bandwidth values.
//
// Input line format (from all_reduce_perf):
//
//	#       size    count  type  redop  root   time  algbw  busbw  #wrong   time  algbw  busbw  #wrong
//	   268435456 67108864 float    sum    -1 2362.7 113.62 198.83       0 2354.8 113.99 199.49       0
//	   ^size(B)                                ^time  ^algbw ^busbw (out-of-place results)
//
// Returns algbw=113.62, busbw=198.83 for the line above.
func extractBandwidth(output string, expectedSize int64) (algbw, busbw float64, err error) {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		// Column 0: size
		size, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil || size != expectedSize {
			continue
		}

		// Column 6: algbw
		algbw, err = strconv.ParseFloat(fields[6], 64)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to parse algbw: %w", err)
		}

		// Column 7: busbw
		busbw, err = strconv.ParseFloat(fields[7], 64)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to parse busbw: %w", err)
		}

		return algbw, busbw, nil
	}

	return 0, 0, fmt.Errorf("could not find results for size %d bytes in output", expectedSize)
}
