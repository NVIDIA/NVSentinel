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

package nic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTopoMatrix_H100DGX8GPU_Mlx5Columns(t *testing.T) {
	// Modern nvidia-smi output with direct mlx5_N column names. No NIC
	// Legend block, no remapping required.
	input := `
	GPU0	GPU1	GPU2	GPU3	GPU4	GPU5	GPU6	GPU7	mlx5_0	mlx5_1	mlx5_2	mlx5_3	mlx5_4	mlx5_5	mlx5_6	mlx5_7	mlx5_8	mlx5_9	CPU Affinity	NUMA Affinity
GPU0	 X 	NV18	NV18	NV18	NV18	NV18	NV18	NV18	PIX	SYS	SYS	SYS	SYS	SYS	SYS	SYS	NODE	SYS	0-47,96-143	0
GPU1	NV18	 X 	NV18	NV18	NV18	NV18	NV18	NV18	SYS	PIX	SYS	SYS	SYS	SYS	SYS	SYS	NODE	SYS	0-47,96-143	0
GPU2	NV18	NV18	 X 	NV18	NV18	NV18	NV18	NV18	SYS	SYS	PIX	SYS	SYS	SYS	SYS	SYS	NODE	SYS	0-47,96-143	0
GPU3	NV18	NV18	NV18	 X 	NV18	NV18	NV18	NV18	SYS	SYS	SYS	PIX	SYS	SYS	SYS	SYS	NODE	SYS	0-47,96-143	0
GPU4	NV18	NV18	NV18	NV18	 X 	NV18	NV18	NV18	SYS	SYS	SYS	SYS	PIX	SYS	SYS	SYS	SYS	NODE	48-95,144-191	1
GPU5	NV18	NV18	NV18	NV18	NV18	 X 	NV18	NV18	SYS	SYS	SYS	SYS	SYS	PIX	SYS	SYS	SYS	NODE	48-95,144-191	1
GPU6	NV18	NV18	NV18	NV18	NV18	NV18	 X 	NV18	SYS	SYS	SYS	SYS	SYS	SYS	PIX	SYS	SYS	NODE	48-95,144-191	1
GPU7	NV18	NV18	NV18	NV18	NV18	NV18	NV18	 X 	SYS	SYS	SYS	SYS	SYS	SYS	SYS	PIX	SYS	NODE	48-95,144-191	1

Legend:

  X    = Self
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)
	require.NotNil(t, matrix)

	assert.Equal(t, []string{"GPU0", "GPU1", "GPU2", "GPU3", "GPU4", "GPU5", "GPU6", "GPU7"}, matrix.GPUs)
	assert.Equal(t, []string{
		"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3",
		"mlx5_4", "mlx5_5", "mlx5_6", "mlx5_7",
		"mlx5_8", "mlx5_9",
	}, matrix.NICs)

	assert.Equal(t,
		[]string{"PIX", "SYS", "SYS", "SYS", "SYS", "SYS", "SYS", "SYS"},
		matrix.Relationships["mlx5_0"],
		"mlx5_0 should show PIX to GPU0 and SYS to the rest")

	assert.Equal(t,
		[]string{"NODE", "NODE", "NODE", "NODE", "SYS", "SYS", "SYS", "SYS"},
		matrix.Relationships["mlx5_8"],
		"mlx5_8 should show NODE to GPUs 0-3 and SYS to GPUs 4-7")

	assert.Equal(t,
		[]int{0, 0, 0, 0, 1, 1, 1, 1},
		matrix.GPUNUMANodes,
		"GPUs 0-3 on NUMA 0, GPUs 4-7 on NUMA 1")
}

func TestParseTopoMatrix_OCIStyleNICColumnsWithLegend(t *testing.T) {
	// OCI H100 / older driver output: NIC columns are labeled NIC0..NICn
	// and a trailing "NIC Legend" block maps each NICn to an mlx5_m.
	// The parser must remap its NIC names so the returned matrix uses
	// real device names.
	input := `        GPU0    GPU1    GPU2    GPU3    NIC0    NIC1    NIC2    NIC3    CPU Affinity    NUMA Affinity   GPU NUMA ID
GPU0     X      NV18    NV18    NV18    PXB     PXB     NODE    NODE    0-55,112-167    0               N/A
GPU1    NV18     X      NV18    NV18    NODE    NODE    NODE    PXB     0-55,112-167    0               N/A
GPU2    NV18    NV18     X      NV18    NODE    NODE    PXB     NODE    0-55,112-167    0               N/A
GPU3    NV18    NV18    NV18     X      NODE    PXB     NODE    NODE    0-55,112-167    0               N/A
NIC0    PXB     NODE    NODE    NODE     X      PIX     NODE    NODE
NIC1    PXB     NODE    NODE    PXB     PIX      X      NODE    NODE
NIC2    NODE    NODE    PXB     NODE    NODE    NODE     X      NODE
NIC3    NODE    PXB     NODE    NODE    NODE    NODE    NODE     X

Legend:

  X    = Self
  SYS  = Connection traversing PCIe as well as the SMP interconnect between NUMA nodes (e.g., QPI/UPI)
  NODE = Connection traversing PCIe as well as the interconnect between PCIe Host Bridges within a NUMA node
  PHB  = Connection traversing PCIe as well as a PCIe Host Bridge (typically the CPU)
  PXB  = Connection traversing multiple PCIe bridges (without traversing the PCIe Host Bridge)
  PIX  = Connection traversing at most a single PCIe bridge
  NV#  = Connection traversing a bonded set of # NVLinks

NIC Legend:

  NIC0: mlx5_0
  NIC1: mlx5_1
  NIC2: mlx5_2
  NIC3: mlx5_3
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	// NIC names must be remapped to mlx5_N.
	assert.Equal(t, []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"}, matrix.NICs)

	// Relationships are keyed by the mapped name.
	assert.Equal(t, []string{"PXB", "NODE", "NODE", "NODE"}, matrix.Relationships["mlx5_0"])
	assert.Equal(t, []string{"PXB", "NODE", "NODE", "PXB"}, matrix.Relationships["mlx5_1"])
	assert.Equal(t, []string{"NODE", "NODE", "PXB", "NODE"}, matrix.Relationships["mlx5_2"])

	// Unmapped key should not exist.
	_, exists := matrix.Relationships["NIC0"]
	assert.False(t, exists, "NIC legend should have been applied")
}

func TestParseTopoMatrix_OutOfOrderGPURows(t *testing.T) {
	// If rows appear in an unexpected order (or a row is duplicated),
	// the parser should still populate the matrix correctly by row
	// label rather than by position.
	input := `        GPU0    GPU1    GPU2    mlx5_0  CPU Affinity
GPU2    NV18    NV18     X      NODE    0-55
GPU0     X      NV18    NV18    PIX     0-55
GPU1    NV18     X      NV18    SYS     0-55
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	// matrix.Relationships["mlx5_0"] should be aligned to GPUs order,
	// regardless of the row order in the source.
	assert.Equal(t, []string{"PIX", "SYS", "NODE"}, matrix.Relationships["mlx5_0"])
}

func TestParseTopoMatrix_PreambleLineWithSingleGPUTokenIsNotTreatedAsHeader(t *testing.T) {
	// Some driver builds emit a status/warning line before the matrix
	// that mentions a single GPU. The parser must skip past it and pick
	// the real matrix header.
	input := `Note: GPU0 detected at startup
        GPU0    GPU1    mlx5_0  CPU Affinity
GPU0     X      NV18    PIX     0-55
GPU1    NV18     X      SYS     0-55
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	assert.Equal(t, []string{"GPU0", "GPU1"}, matrix.GPUs)
	assert.Equal(t, []string{"mlx5_0"}, matrix.NICs)
	assert.Equal(t, []string{"PIX", "SYS"}, matrix.Relationships["mlx5_0"])
}

func TestParseTopoMatrix_WrappedHeaderEightGPUOCI(t *testing.T) {
	// Defensive: 8 GPUs with NICn columns and NIC Legend, header
	// wrapped so GPU0 is on its own line.
	input := `        GPU0
GPU1    GPU2    GPU3    GPU4    GPU5    GPU6    GPU7    NIC0    NIC1    CPU Affinity    NUMA Affinity   GPU NUMA ID
GPU0     X      NV18    NV18    NV18    NV18    NV18    NV18    NV18    PXB     NODE    0-55    0       N/A
GPU1    NV18     X      NV18    NV18    NV18    NV18    NV18    NV18    NODE    PXB     0-55    0       N/A
GPU2    NV18    NV18     X      NV18    NV18    NV18    NV18    NV18    NODE    NODE    0-55    0       N/A
GPU3    NV18    NV18    NV18     X      NV18    NV18    NV18    NV18    NODE    NODE    0-55    0       N/A
GPU4    NV18    NV18    NV18    NV18     X      NV18    NV18    NV18    SYS     SYS     56-111  1       N/A
GPU5    NV18    NV18    NV18    NV18    NV18     X      NV18    NV18    SYS     SYS     56-111  1       N/A
GPU6    NV18    NV18    NV18    NV18    NV18    NV18     X      NV18    SYS     SYS     56-111  1       N/A
GPU7    NV18    NV18    NV18    NV18    NV18    NV18    NV18     X      SYS     SYS     56-111  1       N/A

NIC Legend:
  NIC0: mlx5_0
  NIC1: mlx5_1
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	require.Len(t, matrix.GPUs, 8, "all 8 GPUs must be recognised")
	assert.Equal(t,
		[]string{"GPU0", "GPU1", "GPU2", "GPU3", "GPU4", "GPU5", "GPU6", "GPU7"},
		matrix.GPUs,
	)

	// NICn should be remapped to mlx5_N via the legend.
	assert.Equal(t, []string{"mlx5_0", "mlx5_1"}, matrix.NICs)

	// Every NIC array should have exactly 8 entries, one per GPU.
	for _, nic := range matrix.NICs {
		assert.Len(t, matrix.Relationships[nic], 8, nic)
	}

	// mlx5_0's column values from the input, top-to-bottom: PXB, NODE,
	// NODE, NODE, SYS, SYS, SYS, SYS.
	assert.Equal(t,
		[]string{"PXB", "NODE", "NODE", "NODE", "SYS", "SYS", "SYS", "SYS"},
		matrix.Relationships["mlx5_0"],
	)
}

func TestParseTopoMatrix_GraceGB200AllSYS(t *testing.T) {
	// On Grace/GB200, GPUs connect via NVLink-C2C (not PCIe), so every
	// GPU↔NIC cell shows SYS.
	input := `
	GPU0	GPU1	GPU2	GPU3	mlx5_0	mlx5_1	mlx5_2	mlx5_3	mlx5_4	mlx5_5	CPU Affinity
GPU0	 X 	NV18	NV18	NV18	SYS	SYS	SYS	SYS	SYS	SYS	0-71
GPU1	NV18	 X 	NV18	NV18	SYS	SYS	SYS	SYS	SYS	SYS	0-71
GPU2	NV18	NV18	 X 	NV18	SYS	SYS	SYS	SYS	SYS	SYS	72-143
GPU3	NV18	NV18	NV18	 X 	SYS	SYS	SYS	SYS	SYS	SYS	72-143
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)
	require.Len(t, matrix.GPUs, 4)
	require.Len(t, matrix.NICs, 6)

	for _, nic := range matrix.NICs {
		rel := matrix.Relationships[nic]
		require.Len(t, rel, 4)

		for _, r := range rel {
			assert.Equal(t, "SYS", r)
		}
	}
}

func TestParseTopoMatrix_MissingHeader(t *testing.T) {
	_, err := ParseTopoMatrix("no GPU columns here\njust noise\n")
	assert.Error(t, err)
}

func TestParseTopoMatrix_SingleGPUIsNotMistakenForHeader(t *testing.T) {
	// A single-GPU machine's header has only one GPU column. We require
	// ≥2, so single-GPU systems will fail to parse — this is acceptable
	// because the NIC-topology matrix is only meaningful when there are
	// multiple GPUs to route between. The test codifies the behaviour.
	input := `        GPU0    mlx5_0  CPU Affinity
GPU0     X      PIX     0-55
`

	_, err := ParseTopoMatrix(input)
	require.Error(t, err)
}

func TestParseTopoMatrix_NoNICsProducesEmptyMatrix(t *testing.T) {
	// When a node has GPUs but no InfiniBand NICs, the header still shows
	// GPU columns but no mlx5 columns. The parser returns an empty NICs
	// slice but still records GPUs.
	input := `
	GPU0	GPU1	CPU Affinity	NUMA Affinity
GPU0	 X 	NV18	0-47	0
GPU1	NV18	 X 	0-47	0
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)
	assert.Equal(t, []string{"GPU0", "GPU1"}, matrix.GPUs)
	assert.Empty(t, matrix.NICs)
	assert.Empty(t, matrix.Relationships)
}

func TestParseTopoMatrix_PartialLegendKeepsUnmappedColumns(t *testing.T) {
	// If the legend is present but only maps some NICs, the rest should
	// retain their original NICn name rather than dropping off the map.
	input := `        GPU0    GPU1    NIC0    NIC1    CPU Affinity
GPU0     X      NV18    PXB     NODE    0-55
GPU1    NV18     X      NODE    PXB     0-55

NIC Legend:

  NIC0: mlx5_0
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"mlx5_0", "NIC1"}, matrix.NICs)
	assert.Contains(t, matrix.Relationships, "mlx5_0")
	assert.Contains(t, matrix.Relationships, "NIC1")
}

func TestParseTopoMatrix_ANSIEscapeCodesInHeader(t *testing.T) {
	input := "\t\x1b[4mGPU0\tGPU1\tNIC0\tNIC1\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\x1b[0m\n" +
		"GPU0\t X \tNV18\tPXB\tNODE\t0-55\t0\t\tN/A\n" +
		"GPU1\tNV18\t X \tNODE\tPXB\t0-55\t0\t\tN/A\n" +
		"\n" +
		"NIC Legend:\n" +
		"  NIC0: mlx5_0\n" +
		"  NIC1: mlx5_1\n"

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	assert.Equal(t, []string{"GPU0", "GPU1"}, matrix.GPUs)
	assert.Equal(t, []string{"mlx5_0", "mlx5_1"}, matrix.NICs)
	assert.Equal(t, []string{"PXB", "NODE"}, matrix.Relationships["mlx5_0"])
	assert.Equal(t, []string{"NODE", "PXB"}, matrix.Relationships["mlx5_1"])
	assert.Equal(t, []int{0, 0}, matrix.GPUNUMANodes)
}

func TestParseTopoMatrix_ConcatenatedNICLabelsInHeader(t *testing.T) {
	// Defensive: if adjacent NIC labels are concatenated without spaces
	// (e.g., "NIC28NIC29"), the merged token is silently skipped and
	// the line is still treated as a header.
	input := `        GPU0    GPU1    NIC0    NIC1    NIC28NIC29    NIC30    CPU Affinity    NUMA Affinity   GPU NUMA ID
GPU0     X      NV18    PXB     NODE    SYS     SYS     0-55    0       N/A
GPU1    NV18     X      NODE    PXB     SYS     SYS     0-55    0       N/A

NIC Legend:
  NIC0: mlx5_0
  NIC1: mlx5_1
  NIC30: mlx5_30
`

	matrix, err := ParseTopoMatrix(input)
	require.NoError(t, err)

	assert.Equal(t, []string{"GPU0", "GPU1"}, matrix.GPUs)

	// NIC28NIC29 is not a valid column label, so only NIC0, NIC1, and
	// NIC30 are parsed as NIC columns (3 NICs, not 4).
	assert.Len(t, matrix.NICs, 3)
	assert.Contains(t, matrix.Relationships, "mlx5_0")
	assert.Contains(t, matrix.Relationships, "mlx5_1")
	assert.Contains(t, matrix.Relationships, "mlx5_30")

	assert.Equal(t, []string{"PXB", "NODE"}, matrix.Relationships["mlx5_0"])
}
