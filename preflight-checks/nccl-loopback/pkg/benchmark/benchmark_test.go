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
	"strings"
	"testing"
)

// Sample outputs for testing.
const (
	sample8GPU = `# nThread 1 nGpus 8 minBytes 268435456 maxBytes 268435456
#  Rank  0 Group  0 Pid 12 on test device  0 [0001:00:00] NVIDIA A100-SXM4-80GB
#  Rank  1 Group  0 Pid 12 on test device  1 [0002:00:00] NVIDIA A100-SXM4-80GB
#  Rank  2 Group  0 Pid 12 on test device  2 [0003:00:00] NVIDIA A100-SXM4-80GB
#  Rank  3 Group  0 Pid 12 on test device  3 [0004:00:00] NVIDIA A100-SXM4-80GB
#  Rank  4 Group  0 Pid 12 on test device  4 [000b:00:00] NVIDIA A100-SXM4-80GB
#  Rank  5 Group  0 Pid 12 on test device  5 [000c:00:00] NVIDIA A100-SXM4-80GB
#  Rank  6 Group  0 Pid 12 on test device  6 [000d:00:00] NVIDIA A100-SXM4-80GB
#  Rank  7 Group  0 Pid 12 on test device  7 [000e:00:00] NVIDIA A100-SXM4-80GB
   268435456  67108864  float  sum  -1  2374.1  113.07  197.87  0
`
	sample4GPU = `# nThread 1 nGpus 4
#  Rank  0 Group  0 Pid 12 on test device  0 NVIDIA A100
#  Rank  1 Group  0 Pid 12 on test device  1 NVIDIA A100
#  Rank  2 Group  0 Pid 12 on test device  2 NVIDIA A100
#  Rank  3 Group  0 Pid 12 on test device  3 NVIDIA A100
   268435456  67108864  float  sum  -1  2500.0  107.37  161.06  0
`
	sampleWithNCCLDebug = `NCCL INFO NET/Plugin: Could not find: libnccl-net.so.
NCCL INFO Using network Socket
#  Rank  0 Group  0 Pid 12 on test device  0 NVIDIA A100
#  Rank  1 Group  0 Pid 12 on test device  1 NVIDIA A100
NCCL INFO Channel 00/02: 0 1
   268435456  67108864  float  sum  -1  2374.1  113.07  197.87  0
NCCL INFO Destroying communicator
`
	sample1GPU = `#  Rank  0 Group  0 Pid 12 on test device  0 NVIDIA A100
   268435456  67108864  float  sum  -1  1000.0  268.44  0.00  0
`
	sampleWithTabs = `#  Rank  0 Group  0 Pid 12 on test device  0 NVIDIA A100
#  Rank  1 Group  0 Pid 12 on test device  1 NVIDIA A100
		268435456		67108864		float		sum		-1		2374.1		113.07		197.87		0
`
)

func TestParseOutput(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		sizeMB     int
		wantGPUs   int
		wantAlgbw  float64
		wantBusbw  float64
		wantErr    bool
		errContain string
	}{
		// Valid cases
		{"8 GPUs", sample8GPU, 256, 8, 113.07, 197.87, false, ""},
		{"4 GPUs", sample4GPU, 256, 4, 107.37, 161.06, false, ""},
		{"with NCCL_DEBUG", sampleWithNCCLDebug, 256, 2, 113.07, 197.87, false, ""},
		{"single GPU", sample1GPU, 256, 1, 268.44, 0.00, false, ""},
		{"whitespace/tabs", sampleWithTabs, 256, 2, 113.07, 197.87, false, ""},
		// Error cases
		{"empty output", "", 256, 0, 0, 0, true, "could not determine number of GPUs"},
		{"wrong size", sample8GPU, 128, 0, 0, 0, true, "could not find results for size"},
		{"no GPUs detected", "   268435456  67108864  float  sum  -1  2374.1  113.07  197.87  0\n", 256, 0, 0, 0, true, "could not determine number of GPUs"},
		{"only comments", "# comment\n#  Rank  0 Group  0 Pid 12 on test\n", 256, 0, 0, 0, true, "could not find results"},
		{"too few fields", "#  Rank  0 Group  0 Pid 12 on test\n268435456 67108864 float\n", 256, 0, 0, 0, true, "could not find results"},
		{"invalid algbw", "#  Rank  0 Group  0 Pid 12 on test\n268435456 67108864 float sum -1 2374.1 xxx 197.87 0\n", 256, 0, 0, 0, true, "failed to parse algbw"},
		{"invalid busbw", "#  Rank  0 Group  0 Pid 12 on test\n268435456 67108864 float sum -1 2374.1 113.07 BAD 0\n", 256, 0, 0, 0, true, "failed to parse busbw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseOutput(tt.output, tt.sizeMB)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContain)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.NumGPUs != tt.wantGPUs {
				t.Errorf("NumGPUs = %d, want %d", result.NumGPUs, tt.wantGPUs)
			}
			if result.AlgoBandwidthGbps != tt.wantAlgbw {
				t.Errorf("AlgoBW = %v, want %v", result.AlgoBandwidthGbps, tt.wantAlgbw)
			}
			if result.BusBandwidthGbps != tt.wantBusbw {
				t.Errorf("BusBW = %v, want %v", result.BusBandwidthGbps, tt.wantBusbw)
			}
		})
	}
}

func TestCountGPUs(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{"empty", "", 0},
		{"no rank lines", "# Some header\n", 0},
		{"single GPU", "#  Rank  0 Group  0 Pid 12 on test device 0\n", 1},
		{"multiple GPUs", "#  Rank  0 Group  0 Pid 12\n#  Rank  1 Group  0 Pid 12\n#  Rank  2 Group  0 Pid 12\n", 3},
		{"wrong format", "# mentions Rank but wrong format\n", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countGPUs(tt.output); got != tt.want {
				t.Errorf("countGPUs() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractBandwidth(t *testing.T) {
	const size256MB int64 = 268435456

	tests := []struct {
		name      string
		output    string
		size      int64
		wantAlgbw float64
		wantBusbw float64
		wantErr   bool
	}{
		{"valid", "268435456 67108864 float sum -1 2374.1 113.07 197.87 0\n", size256MB, 113.07, 197.87, false},
		{"size mismatch", "134217728 33554432 float sum -1 1200.0 111.85 195.74 0\n", size256MB, 0, 0, true},
		{"empty", "", size256MB, 0, 0, true},
		{"only comments", "# header\n", size256MB, 0, 0, true},
		{"too few fields", "268435456 67108864 float sum -1 2374.1\n", size256MB, 0, 0, true},
		{"invalid algbw", "268435456 67108864 float sum -1 2374.1 BAD 197.87 0\n", size256MB, 0, 0, true},
		{"invalid busbw", "268435456 67108864 float sum -1 2374.1 113.07 BAD 0\n", size256MB, 0, 0, true},
		{"finds correct line", "# header\n134217728 x y\n268435456 67108864 float sum -1 2374.1 113.07 197.87 0\n", size256MB, 113.07, 197.87, false},
		{"skips NCCL debug", "NCCL INFO test\n268435456 67108864 float sum -1 2374.1 113.07 197.87 0\n", size256MB, 113.07, 197.87, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			algbw, busbw, err := extractBandwidth(tt.output, tt.size)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if algbw != tt.wantAlgbw {
				t.Errorf("algbw = %v, want %v", algbw, tt.wantAlgbw)
			}
			if busbw != tt.wantBusbw {
				t.Errorf("busbw = %v, want %v", busbw, tt.wantBusbw)
			}
		})
	}
}
