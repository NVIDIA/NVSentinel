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
	"testing"
)

// Sample output from all_reduce_perf on 8x A100 GPUs
const sampleNCCLOutput = `# nThread 1 nGpus 8 minBytes 268435456 maxBytes 268435456 step: 2(factor) warmup iters: 5 iters: 20 agg iters: 1 validation: 1 graph: 0
#
# Using devices
#  Rank  0 Group  0 Pid     12 on nccl-test-1 device  0 [0001:00:00] NVIDIA A100-SXM4-80GB
#  Rank  1 Group  0 Pid     12 on nccl-test-1 device  1 [0002:00:00] NVIDIA A100-SXM4-80GB
#  Rank  2 Group  0 Pid     12 on nccl-test-1 device  2 [0003:00:00] NVIDIA A100-SXM4-80GB
#  Rank  3 Group  0 Pid     12 on nccl-test-1 device  3 [0004:00:00] NVIDIA A100-SXM4-80GB
#  Rank  4 Group  0 Pid     12 on nccl-test-1 device  4 [000b:00:00] NVIDIA A100-SXM4-80GB
#  Rank  5 Group  0 Pid     12 on nccl-test-1 device  5 [000c:00:00] NVIDIA A100-SXM4-80GB
#  Rank  6 Group  0 Pid     12 on nccl-test-1 device  6 [000d:00:00] NVIDIA A100-SXM4-80GB
#  Rank  7 Group  0 Pid     12 on nccl-test-1 device  7 [000e:00:00] NVIDIA A100-SXM4-80GB
#
#                                                              out-of-place                       in-place          
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)       
   268435456      67108864     float     sum      -1   2374.1  113.07  197.87      0   2375.8  112.99  197.73      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 197.8 
`

const sampleNCCLOutput4GPU = `# nThread 1 nGpus 4 minBytes 268435456 maxBytes 268435456 step: 2(factor) warmup iters: 5 iters: 20 agg iters: 1 validation: 1 graph: 0
#
# Using devices
#  Rank  0 Group  0 Pid     12 on nccl-test-1 device  0 [0001:00:00] NVIDIA A100-SXM4-80GB
#  Rank  1 Group  0 Pid     12 on nccl-test-1 device  1 [0002:00:00] NVIDIA A100-SXM4-80GB
#  Rank  2 Group  0 Pid     12 on nccl-test-1 device  2 [0003:00:00] NVIDIA A100-SXM4-80GB
#  Rank  3 Group  0 Pid     12 on nccl-test-1 device  3 [0004:00:00] NVIDIA A100-SXM4-80GB
#
#                                                              out-of-place                       in-place          
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)       
   268435456      67108864     float     sum      -1   2500.0  107.37  161.06      0   2510.0  106.95  160.42      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 160.74
`

func TestParseOutput_8GPUs(t *testing.T) {
	result, err := parseOutput(sampleNCCLOutput, 256)
	if err != nil {
		t.Fatalf("parseOutput failed: %v", err)
	}

	if result.NumGPUs != 8 {
		t.Errorf("expected 8 GPUs, got %d", result.NumGPUs)
	}

	if result.TestSizeBytes != 268435456 {
		t.Errorf("expected test size 268435456, got %d", result.TestSizeBytes)
	}

	// Check busbw is approximately 197.87
	if result.BusBandwidthGbps < 197.0 || result.BusBandwidthGbps > 198.0 {
		t.Errorf("expected busbw ~197.87, got %f", result.BusBandwidthGbps)
	}

	// Check algbw is approximately 113.07
	if result.AlgoBandwidthGbps < 113.0 || result.AlgoBandwidthGbps > 114.0 {
		t.Errorf("expected algbw ~113.07, got %f", result.AlgoBandwidthGbps)
	}

	t.Logf("Parsed result: NumGPUs=%d, BusBW=%.2f GB/s, AlgoBW=%.2f GB/s",
		result.NumGPUs, result.BusBandwidthGbps, result.AlgoBandwidthGbps)
}

func TestParseOutput_4GPUs(t *testing.T) {
	result, err := parseOutput(sampleNCCLOutput4GPU, 256)
	if err != nil {
		t.Fatalf("parseOutput failed: %v", err)
	}

	if result.NumGPUs != 4 {
		t.Errorf("expected 4 GPUs, got %d", result.NumGPUs)
	}

	if result.BusBandwidthGbps < 161.0 || result.BusBandwidthGbps > 162.0 {
		t.Errorf("expected busbw ~161.06, got %f", result.BusBandwidthGbps)
	}

	t.Logf("Parsed result: NumGPUs=%d, BusBW=%.2f GB/s", result.NumGPUs, result.BusBandwidthGbps)
}

func TestParseOutput_WrongSize(t *testing.T) {
	_, err := parseOutput(sampleNCCLOutput, 128) // Looking for 128MB, but output has 256MB
	if err == nil {
		t.Error("expected error for wrong size, got nil")
	}

	t.Logf("Got expected error: %v", err)
}

func TestParseOutput_NoGPUs(t *testing.T) {
	badOutput := `# Some header
# No GPU rank lines here
   268435456      67108864     float     sum      -1   2374.1  113.07  197.87      0
`
	_, err := parseOutput(badOutput, 256)
	if err == nil {
		t.Error("expected error for no GPUs detected, got nil")
	}

	t.Logf("Got expected error: %v", err)
}

func TestParseOutput_MalformedData(t *testing.T) {
	badOutput := `# Using devices
#  Rank  0 Group  0 Pid     12 on nccl-test-1 device  0 [0001:00:00] NVIDIA A100
   not_a_number      67108864     float     sum      -1   2374.1  113.07  197.87      0
`
	_, err := parseOutput(badOutput, 256)
	if err == nil {
		t.Error("expected error for malformed data, got nil")
	}

	t.Logf("Got expected error: %v", err)
}
