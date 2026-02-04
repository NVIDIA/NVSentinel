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
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

// Run executes the all_reduce_perf benchmark and returns the results.
// numGPUs specifies how many GPUs to use (must match NVIDIA_VISIBLE_DEVICES count).
// testSizeMB specifies the message size in megabytes.
func (r *Runner) Run(numGPUs, testSizeMB int) (*Result, error) {
	sizeArg := fmt.Sprintf("%dM", testSizeMB)

	// -b: min bytes, -e: max bytes, -g: num GPUs
	cmd := exec.Command(r.binaryPath,
		"-b", sizeArg,
		"-e", sizeArg,
		"-g", strconv.Itoa(numGPUs),
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Info("Running NCCL all-reduce benchmark",
		"binary", r.binaryPath,
		"size_mb", testSizeMB)

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("all_reduce_perf failed: %w\nstderr: %s", err, stderr.String())
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
// Example line:
//
//	268435456  67108864  float  sum  -1  2362.7  113.62  198.83  0  2354.8  113.99  199.49  0
//
// Columns: size, count, type, redop, root, time, algbw, busbw, wrong, time, algbw, busbw, wrong
func parseOutput(output string, testSizeMB int) (*Result, error) {
	expectedSize := int64(testSizeMB) * 1024 * 1024

	// Find number of GPUs from "Using devices" section
	numGPUs := 0
	gpuPattern := regexp.MustCompile(`#\s+Rank\s+\d+\s+Group`)

	for line := range strings.SplitSeq(output, "\n") {
		if gpuPattern.MatchString(line) {
			numGPUs++
		}
	}

	if numGPUs == 0 {
		return nil, fmt.Errorf("could not determine number of GPUs from output")
	}

	// Find the data line with our test size
	// Lines starting with whitespace followed by the size in bytes
	var busbw, algbw float64

	found := false

	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		// First field is size in bytes
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}

		if size == expectedSize {
			// Parse out-of-place busbw (column 7, 0-indexed)
			algbw, err = strconv.ParseFloat(fields[6], 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse algbw: %w", err)
			}

			busbw, err = strconv.ParseFloat(fields[7], 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse busbw: %w", err)
			}

			found = true

			break
		}
	}

	if !found {
		return nil, fmt.Errorf("could not find results for size %d bytes in output", expectedSize)
	}

	return &Result{
		BusBandwidthGbps:  busbw,
		AlgoBandwidthGbps: algbw,
		NumGPUs:           numGPUs,
		TestSizeBytes:     expectedSize,
	}, nil
}
