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
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

type GPUNVLinkTopology struct {
	GPUID       int
	Connections []model.NVLink
}

var (
	gpuHeaderPattern = regexp.MustCompile(`^GPU (\d+):`)
	linkPattern      = regexp.MustCompile(`^\s*Link (\d+):\s+Remote Device\s+([0-9a-fA-F:\.]+):\s+Link\s+(\d+)`)
)

func DetectNVLinkTopology(ctx context.Context) (map[int]GPUNVLinkTopology, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi", "nvlink", "-R")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi nvlink -R failed: %w (output: %s)", err, string(output))
	}

	return parseNVLinkOutput(string(output))
}

func parseNVLinkOutput(output string) (map[int]GPUNVLinkTopology, error) {
	topology := make(map[int]GPUNVLinkTopology)
	lines := strings.Split(output, "\n")

	currentGPUID := -1

	var currentConnections []model.NVLink

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// GPU 3: NVIDIA A100-SXM4-80GB (UUID: GPU-0df17138-4661-76d1-a4ef-9aa0857f7941)
		if gpuID, ok := parseGPUHeader(line); ok {
			if currentGPUID != -1 {
				saveGPUTopology(topology, currentGPUID, currentConnections)
			}

			currentGPUID = gpuID
			currentConnections = make([]model.NVLink, 0)

			continue
		}

		// Link 0: Remote Device 00000008:00:00.0: Link 34
		if connection, ok := parseLinkLine(line); ok && currentGPUID != -1 {
			currentConnections = append(currentConnections, connection)
		}
	}

	if currentGPUID != -1 {
		saveGPUTopology(topology, currentGPUID, currentConnections)
	}

	if len(topology) == 0 {
		return nil, fmt.Errorf("no GPU NVLink topology found in nvidia-smi output")
	}

	return topology, nil
}

func parseGPUHeader(line string) (int, bool) {
	matches := gpuHeaderPattern.FindStringSubmatch(line)
	if matches == nil {
		return 0, false
	}

	gpuID, err := strconv.Atoi(matches[1])
	if err != nil {
		slog.Warn("Invalid GPU ID in nvidia-smi output", "gpu_id_str", matches[1])
		return 0, false
	}

	return gpuID, true
}

func parseLinkLine(line string) (model.NVLink, bool) {
	matches := linkPattern.FindStringSubmatch(line)
	if matches == nil {
		return model.NVLink{}, false
	}

	localLink, err := strconv.Atoi(matches[1])
	if err != nil {
		slog.Warn("Invalid local link ID", "link_id_str", matches[1])
		return model.NVLink{}, false
	}

	remotePCI := normalizePCIAddress(matches[2])

	remoteLink, err := strconv.Atoi(matches[3])
	if err != nil {
		slog.Warn("Invalid remote link ID", "link_id_str", matches[3])
		return model.NVLink{}, false
	}

	return model.NVLink{
		LinkID:           localLink,
		RemotePCIAddress: remotePCI,
		RemoteLinkID:     remoteLink,
	}, true
}

func saveGPUTopology(topology map[int]GPUNVLinkTopology, gpuID int, connections []model.NVLink) {
	topology[gpuID] = GPUNVLinkTopology{
		GPUID:       gpuID,
		Connections: connections,
	}
}
