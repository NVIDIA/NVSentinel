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

package nvml

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

func TestParseNVLinkOutput_A100(t *testing.T) {
	output := `GPU 0: NVIDIA A100-SXM4-80GB (UUID: GPU-4af877ac-a2b7-9586-06e1-d900b47f2505)
	 Link 0: Remote Device 00000008:00:00.0: Link 29
	 Link 1: Remote Device 00000008:00:00.0: Link 28
	 Link 2: Remote Device 00000005:00:00.0: Link 34
	 Link 3: Remote Device 00000005:00:00.0: Link 35
	 Link 4: Remote Device 00000007:00:00.0: Link 34
	 Link 5: Remote Device 00000007:00:00.0: Link 35
	 Link 6: Remote Device 0000000A:00:00.0: Link 26
	 Link 7: Remote Device 0000000A:00:00.0: Link 27
	 Link 8: Remote Device 00000006:00:00.0: Link 8
	 Link 9: Remote Device 00000006:00:00.0: Link 9
	 Link 10: Remote Device 00000009:00:00.0: Link 12
	 Link 11: Remote Device 00000009:00:00.0: Link 13
`

	topology, err := parseNVLinkOutput(output)
	require.NoError(t, err)
	require.Len(t, topology, 1)

	gpu0 := topology[0]
	require.Equal(t, 0, gpu0.GPUID)
	require.Len(t, gpu0.Connections, 12)

	require.Equal(t, 0, gpu0.Connections[0].LinkID)
	require.Equal(t, "0008:00:00.0", gpu0.Connections[0].RemotePCIAddress)
	require.Equal(t, 29, gpu0.Connections[0].RemoteLinkID)

	require.Equal(t, 11, gpu0.Connections[11].LinkID)
	require.Equal(t, "0009:00:00.0", gpu0.Connections[11].RemotePCIAddress)
	require.Equal(t, 13, gpu0.Connections[11].RemoteLinkID)

	nvswitchCounts := make(map[string]int)
	for _, conn := range gpu0.Connections {
		nvswitchCounts[conn.RemotePCIAddress]++
	}
	require.Len(t, nvswitchCounts, 6)
	for _, count := range nvswitchCounts {
		require.Equal(t, 2, count)
	}
}

func TestParseNVLinkOutput_MultipleGPUs(t *testing.T) {
	output := `GPU 0: NVIDIA H100 80GB HBM3
	Link 0: Remote Device 0000:42:00.0: Link 5
GPU 1: NVIDIA H100 80GB HBM3
	Link 0: Remote Device 0000:44:00.0: Link 7
GPU 2: NVIDIA H100 80GB HBM3
	Link 0: Remote Device 0000:46:00.0: Link 9`

	topology, err := parseNVLinkOutput(output)
	require.NoError(t, err)
	require.Len(t, topology, 3)

	require.Equal(t, 0, topology[0].GPUID)
	require.Len(t, topology[0].Connections, 1)
	require.Equal(t, 1, topology[1].GPUID)
	require.Len(t, topology[1].Connections, 1)
	require.Equal(t, 2, topology[2].GPUID)
	require.Len(t, topology[2].Connections, 1)
}

func TestParseNVLinkOutput_ErrorCases(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "empty output",
			output: "",
		},
		{
			name: "no GPU headers",
			output: `Some random output
	Link 0: Remote Device 0000:42:00.0: Link 5`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topology, err := parseNVLinkOutput(tt.output)
			require.Error(t, err)
			require.Nil(t, topology)
		})
	}
}

func TestParseGPUHeader(t *testing.T) {
	tests := []struct {
		line     string
		expected int
		isValid  bool
	}{
		{"GPU 0: NVIDIA H100 80GB HBM3", 0, true},
		{"GPU 15: Some GPU Name", 15, true},
		{"This is not a GPU line", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		gpuID, isValid := parseGPUHeader(tt.line)
		require.Equal(t, tt.isValid, isValid)
		if isValid {
			require.Equal(t, tt.expected, gpuID)
		}
	}
}

func TestParseLinkLine(t *testing.T) {
	tests := []struct {
		line     string
		expected model.NVLink
		isValid  bool
	}{
		{
			line: "	Link 0: Remote Device 0000:42:00.0: Link 5",
			expected: model.NVLink{
				LinkID:           0,
				RemotePCIAddress: "0000:42:00.0",
				RemoteLinkID:     5,
			},
			isValid: true,
		},
		{
			line: "Link 17: Remote Device 0000:FF:1F.7: Link 31",
			expected: model.NVLink{
				LinkID:           17,
				RemotePCIAddress: "0000:ff:1f.7",
				RemoteLinkID:     31,
			},
			isValid: true,
		},
		{
			line:    "This is not a link line",
			isValid: false,
		},
		{
			line:    "",
			isValid: false,
		},
	}

	for _, tt := range tests {
		link, isValid := parseLinkLine(tt.line)
		require.Equal(t, tt.isValid, isValid)
		if isValid {
			require.Equal(t, tt.expected, link)
		}
	}
}
