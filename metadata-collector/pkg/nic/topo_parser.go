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

// Package nic parses the `nvidia-smi topo -m` matrix and publishes it
// verbatim. Interpretation (compute/storage/management, monitoring
// decisions) is left to downstream consumers like the NIC Health
// Monitor.
package nic

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const topoCommandTimeout = 30 * time.Second

// Column patterns recognise NIC-naming conventions in the header:
//   - NICn placeholders (older/OCI drivers), remapped via "NIC Legend"
//   - mlx5_N (modern drivers)
//   - Arbitrary device names like ibp3s0, roceP6p3s0 (GB200 / non-Mellanox)
//
// nicColumnPattern accepts any alphanumeric+underscore token that is NOT
// a GPU column, a known metadata label, or a data cell value.
var (
	gpuColumnPattern = regexp.MustCompile(`^GPU\d+$`)
	nicColumnPattern = regexp.MustCompile(`^(?:mlx5_\d+|NIC\d+|(?:ib|roce|rdma|mlx|cx)\w+)$`)
	nicLegendEntry   = regexp.MustCompile(`^(NIC\d+)\s*:\s*(\S+)\s*$`)
	dataCellPattern  = regexp.MustCompile(`^(X|PIX|PXB|PHB|NODE|SYS|NV\d+)$`)

	// ansiEscape matches ANSI escape sequences: CSI sequences (e.g.,
	// \x1b[4m underline-on, \x1b[0m reset), OSC sequences (terminated
	// by BEL), and the 8-bit CSI introducer (\x9b). nvidia-smi emits
	// SGR sequences in piped output on some driver versions to underline
	// the header row, which breaks field parsing if not stripped first.
	// Pattern from github.com/acarl005/stripansi.
	ansiEscape = regexp.MustCompile(
		"[\u001B\u009B][[\\]()#;?]*" +
			"(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)" +
			"|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))",
	)
)

// TopoMatrix is the parsed relationship map. NICs entries are remapped
// to mlx5_N when a NIC Legend is present. Relationships[nic][i] holds
// the topology level between GPUs[i] and nic — one of "X", "PIX",
// "PXB", "PHB", "NODE", "SYS", or "NV<n>".
//
// GPUNUMANodes[i] is the NUMA Affinity value for GPUs[i], parsed from
// the trailing column in each GPU data row. A value of -1 means the
// column was missing or unparseable.
type TopoMatrix struct {
	GPUs          []string
	NICs          []string
	GPUNUMANodes  []int
	Relationships map[string][]string
}

// RunTopoCommand runs `nvidia-smi topo -m` and returns its stdout.
func RunTopoCommand(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, topoCommandTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "nvidia-smi", "topo", "-m").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nvidia-smi topo -m failed: %w (output: %s)", err, string(output))
	}

	return string(output), nil
}

// ParseTopoMatrix parses `nvidia-smi topo -m` stdout. It tolerates
// header wrapping (seen on OCI H100 pipes where GPU0 lands on its own
// line), out-of-order/duplicated data rows, preamble noise, and both
// NIC naming conventions.
func ParseTopoMatrix(output string) (*TopoMatrix, error) {
	output = ansiEscape.ReplaceAllString(output, "")
	lines := strings.Split(output, "\n")

	headerIdx, headers, err := findHeaderRow(lines)
	if err != nil {
		return nil, fmt.Errorf("parse nvidia-smi topo -m header: %w", err)
	}

	gpuCols, nicCols := collectColumns(headers)
	sort.Slice(gpuCols, func(i, j int) bool { return gpuCols[i].index < gpuCols[j].index })

	// In data rows the NUMA Affinity value sits at a fixed offset past the
	// cell values: row_label + (numGPUs + numNICs) cells + CPU_Affinity.
	numaValueIdx := 1 + len(gpuCols) + len(nicCols) + 1

	slog.Debug("nvidia-smi topo -m header parsed",
		"raw_header", lines[headerIdx],
		"total_fields", len(headers),
		"gpu_columns", columnNames(gpuCols),
		"nic_columns", columnNames(nicCols),
		"numa_value_idx", numaValueIdx,
	)

	matrix := newEmptyMatrix(gpuCols, nicCols, len(gpuCols))
	gpuIndex := buildGPUIndex(matrix.GPUs)

	legend, unknown := fillMatrixBody(matrix, lines[headerIdx+1:], nicCols, gpuIndex, numaValueIdx)

	if len(unknown) > 0 {
		warnUnknownGPURows(unknown, matrix.GPUs)
	}

	if len(legend) > 0 {
		applyNICLegend(matrix, legend)
	}

	return matrix, nil
}

// fillMatrixBody walks the post-header lines, collecting NIC legend
// entries and GPU data rows. Returns the legend and any GPU-labelled
// rows whose label was not in the header (a signal the header parse
// was incomplete).
func fillMatrixBody(
	matrix *TopoMatrix,
	body []string,
	nicCols []column,
	gpuIndex map[string]int,
	numaValueIdx int,
) (map[string]string, map[string]struct{}) {
	legend := make(map[string]string)
	unknown := make(map[string]struct{})

	for _, raw := range body {
		line := strings.TrimSpace(raw)
		if line == "" || isLegendLine(line) {
			continue
		}

		if m := nicLegendEntry.FindStringSubmatch(line); m != nil {
			legend[m[1]] = m[2]
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		row, ok := gpuIndex[fields[0]]
		if !ok {
			if gpuColumnPattern.MatchString(fields[0]) {
				unknown[fields[0]] = struct{}{}
			}

			continue
		}

		writeRowCells(matrix, fields, nicCols, row)
		writeGPUNUMANode(matrix, fields, row, numaValueIdx)
	}

	return legend, unknown
}

func warnUnknownGPURows(unknown map[string]struct{}, headerGPUs []string) {
	names := make([]string, 0, len(unknown))
	for n := range unknown {
		names = append(names, n)
	}

	sort.Strings(names)
	slog.Warn("nvidia-smi topo -m produced GPU rows whose labels were not in the header",
		"missing_gpu_rows", names,
		"header_gpus", headerGPUs,
		"hint", "header parsing dropped one or more GPU columns; "+
			"relationship arrays will not cover every GPU",
	)
}

func newEmptyMatrix(gpuCols, nicCols []column, numGPUs int) *TopoMatrix {
	m := &TopoMatrix{
		GPUs:          make([]string, 0, len(gpuCols)),
		NICs:          make([]string, 0, len(nicCols)),
		GPUNUMANodes:  make([]int, numGPUs),
		Relationships: make(map[string][]string, len(nicCols)),
	}

	for i := range m.GPUNUMANodes {
		m.GPUNUMANodes[i] = -1
	}

	for _, c := range gpuCols {
		m.GPUs = append(m.GPUs, c.name)
	}

	for _, c := range nicCols {
		m.NICs = append(m.NICs, c.name)
		m.Relationships[c.name] = make([]string, len(gpuCols))
	}

	return m
}

func buildGPUIndex(gpus []string) map[string]int {
	idx := make(map[string]int, len(gpus))
	for i, g := range gpus {
		idx[g] = i
	}

	return idx
}

// writeGPUNUMANode reads the NUMA Affinity value from a GPU data row.
// numaValueIdx is the pre-computed field index where the NUMA Affinity
// value sits in the data row (= 1 + numGPUs + numNICs + 1).
//
// On Grace/GB200, NUMA Affinity can be a comma-separated list of NUMA
// nodes (e.g., "0,2-17") because Grace exposes GPU memory as additional
// NUMA nodes. We take the first integer as the primary CPU NUMA node.
// writeGPUNUMANode extracts the CPU-socket NUMA node from the "NUMA
// Affinity" column. On x86 this is a plain integer ("0", "1"). On
// Grace/GB200 it is a comma/dash-separated set such as "0,2-17", where
// the first integer is the CPU socket (node 0) and the remainder are
// GPU-memory and MIG NUMA nodes (2-17). We only need the CPU socket
// because NIC sysfs numa_node always reports the CPU NUMA node — NICs
// are PCIe devices on the CPU, never on a GPU memory NUMA domain.
func writeGPUNUMANode(matrix *TopoMatrix, fields []string, rowIdx, numaValueIdx int) {
	if numaValueIdx >= len(fields) {
		return
	}

	raw := strings.TrimSpace(fields[numaValueIdx])

	first := raw
	if idx := strings.IndexAny(raw, ",-"); idx > 0 {
		first = raw[:idx]
	}

	n, err := strconv.Atoi(first)
	if err != nil {
		return
	}

	matrix.GPUNUMANodes[rowIdx] = n
}

func writeRowCells(matrix *TopoMatrix, fields []string, nicCols []column, rowIdx int) {
	for _, nicCol := range nicCols {
		cellIdx := nicCol.index + 1
		if cellIdx >= len(fields) {
			continue
		}

		matrix.Relationships[nicCol.name][rowIdx] = strings.TrimSpace(fields[cellIdx])
	}
}

// applyNICLegend rewrites NICn names to mlx5_N. NICs missing from the
// legend keep their original names so partial legends still work.
func applyNICLegend(matrix *TopoMatrix, legend map[string]string) {
	renamed := make([]string, len(matrix.NICs))
	rels := make(map[string][]string, len(matrix.NICs))

	for i, name := range matrix.NICs {
		target := name
		if mapped, ok := legend[name]; ok {
			target = mapped
		}

		renamed[i] = target
		rels[target] = matrix.Relationships[name]
	}

	matrix.NICs = renamed
	matrix.Relationships = rels
}

// column records the position of a header column by its 0-based index
// among the header fields.
type column struct {
	name  string
	index int
}

func columnNames(cols []column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}

	return names
}

// findHeaderRow accumulates fields from contiguous header-like lines at
// the top of the output and returns the index of the LAST such line.
// Multiple header lines are merged into one logical header to handle
// potential line wrapping in narrow pipe contexts.
func findHeaderRow(lines []string) (int, []string, error) {
	const minGPUTokens = 2

	var accumulated []string

	lastIdx := -1

	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if lastIdx >= 0 {
				break
			}

			continue
		}

		fields := strings.Fields(trimmed)

		if !isHeaderLine(fields) {
			if lastIdx >= 0 {
				break
			}

			continue
		}

		accumulated = append(accumulated, fields...)
		lastIdx = idx
	}

	if countGPUTokens(accumulated) < minGPUTokens {
		return 0, nil, fmt.Errorf(
			"no header row with multiple GPU columns found in nvidia-smi topo -m output (found %d)",
			countGPUTokens(accumulated),
		)
	}

	return lastIdx, accumulated, nil
}

// isHeaderLine reports whether a line looks like part of the header
// block. Two conditions must hold:
//
//  1. The first field must be a column label (GPU0, NIC0, mlx5_0).
//     This filters out preamble lines like "Note: GPU0 detected".
//  2. No field may be a data-cell value (X, PIX, PXB, NODE, SYS,
//     NV<n>). This filters out data rows.
//
// Unrecognised tokens (e.g., "NIC28NIC29" from column-label
// concatenation, or "Affinity", "CPU", "N/A") are tolerated so that
// header wrapping and trailing annotation words don't break the parse.
func isHeaderLine(fields []string) bool {
	if len(fields) == 0 || !isColumnLabel(fields[0]) {
		return false
	}

	for _, f := range fields {
		if dataCellPattern.MatchString(f) {
			return false
		}
	}

	return true
}

func isColumnLabel(s string) bool {
	return gpuColumnPattern.MatchString(s) || nicColumnPattern.MatchString(s)
}

func countGPUTokens(fields []string) int {
	n := 0

	for _, f := range fields {
		if gpuColumnPattern.MatchString(f) {
			n++
		}
	}

	return n
}

func collectColumns(headers []string) (gpus, nics []column) {
	for i, h := range headers {
		switch {
		case gpuColumnPattern.MatchString(h):
			gpus = append(gpus, column{name: h, index: i})
		case nicColumnPattern.MatchString(h):
			nics = append(nics, column{name: h, index: i})
		}
	}

	return gpus, nics
}

func isLegendLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(lower, "legend") || strings.HasPrefix(lower, "nic legend")
}
