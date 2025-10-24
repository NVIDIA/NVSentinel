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

package lsnvlink

import (
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TopologyLink represents a single NVLink connection
type TopologyLink struct {
	RemotePCI  string `json:"remote_pci"`
	RemoteLink int    `json:"remote_link"`
}

// GPUTopology represents the NVLink connections for a single GPU
type GPUTopology struct {
	Links map[string]TopologyLink `json:"links"`
}

// NVLinkTopology represents the complete NVLink topology
type NVLinkTopology struct {
	HasNVSwitch          bool                   `json:"has_nvswitch"`
	Timestamp            string                 `json:"timestamp"`
	Topology             map[string]GPUTopology `json:"topology"`
	NVSwitchPCIAddresses []string               `json:"nvswitch_pci_addresses"`
}

// DynamicTopologyProvider provides GPU ID lookup based on discovered topology
type DynamicTopologyProvider struct {
	topology *NVLinkTopology

	// Reverse lookup maps for efficient queries
	// Map from PCI address to map of nvlink_id to gpu_id
	nvswitchPCIToGPUMap map[string]map[int]int // [pci_address][nvlink_id] -> gpu_id
}

var (
	globalTopologyProvider *DynamicTopologyProvider
	providerOnce           sync.Once
)

// GetTopologyProvider returns the singleton topology provider
func GetTopologyProvider() *DynamicTopologyProvider {
	providerOnce.Do(func() {
		globalTopologyProvider = &DynamicTopologyProvider{
			topology: &NVLinkTopology{
				HasNVSwitch:          false,
				Topology:             make(map[string]GPUTopology),
				NVSwitchPCIAddresses: []string{},
			},
		}
		if err := globalTopologyProvider.GatherTopology(); err != nil {
			slog.Error("Failed to gather topology on initialization", "error", err)
		}
	})

	return globalTopologyProvider
}

// GatherTopology gathers the topology by executing nvidia-smi directly
func (p *DynamicTopologyProvider) GatherTopology() error {
	// Get NVLink topology to check if NVLinks/NVSwitches exist
	cmd := exec.Command("nvidia-smi", "nvlink", "-R")

	nvlinkOutput, err := cmd.CombinedOutput()
	if err != nil {
		// Any error from nvidia-smi should be fatal (driver issues, permission problems, etc.)
		return fmt.Errorf("failed to execute nvidia-smi nvlink -R: %w (output: %s)", err, string(nvlinkOutput))
	}

	// Check if output is empty or contains no NVLink information
	outputStr := strings.TrimSpace(string(nvlinkOutput))

	if len(outputStr) == 0 || !strings.Contains(outputStr, "Link") {
		slog.Info("No NVLink information found, no NVSwitches on this node")

		p.topology = &NVLinkTopology{
			HasNVSwitch:          false,
			Topology:             make(map[string]GPUTopology),
			NVSwitchPCIAddresses: []string{},
		}

		return nil
	}

	// Parse the nvidia-smi output and extract unique PCI addresses
	topology, nvswitchPCIAddrs := p.parseNVLinkOutputWithPCIAddresses(outputStr)

	if len(nvswitchPCIAddrs) == 0 {
		slog.Info("No NVSwitch devices found in NVLink topology")

		p.topology = &NVLinkTopology{
			HasNVSwitch:          false,
			Topology:             topology.Topology,
			NVSwitchPCIAddresses: []string{},
		}

		return nil
	}

	topology.HasNVSwitch = true
	topology.Timestamp = time.Now().UTC().Format(time.RFC3339)
	topology.NVSwitchPCIAddresses = nvswitchPCIAddrs

	p.topology = topology
	p.buildReverseMaps()

	slog.Info("Successfully gathered NVLink topology",
		"hasNVSwitch", topology.HasNVSwitch,
		"gpuCount", len(topology.Topology),
		"nvSwitchCount", len(topology.NVSwitchPCIAddresses))

	// Log the complete topology details
	p.logFullTopology()

	return nil
}

// logFullTopology logs the complete topology details for debugging and monitoring
func (p *DynamicTopologyProvider) logFullTopology() {
	slog.Info("Topology",
		"timestamp", p.topology.Timestamp,
		"hasNVSwitch", p.topology.HasNVSwitch)

	p.logNVSwitchPCIAddresses()
	p.logGPUTopology()
}

// logNVSwitchPCIAddresses logs the NVSwitch PCI addresses if present
func (p *DynamicTopologyProvider) logNVSwitchPCIAddresses() {
	if !p.topology.HasNVSwitch || len(p.topology.NVSwitchPCIAddresses) == 0 {
		return
	}

	slog.Info("NVSwitch PCI addresses",
		"count", len(p.topology.NVSwitchPCIAddresses),
		"addresses", p.topology.NVSwitchPCIAddresses,
	)
}

// logGPUTopology logs the GPU topology with NVLink connections
func (p *DynamicTopologyProvider) logGPUTopology() {
	if len(p.topology.Topology) == 0 {
		slog.Info("No GPUs with NVLinks found")
		return
	}

	slog.Info("GPU topology", "gpuCount", len(p.topology.Topology))

	// Sort GPU IDs for consistent output
	gpuIDs := p.getSortedGPUIDs()
	for _, gpuID := range gpuIDs {
		p.logSingleGPU(gpuID)
	}
}

// getSortedGPUIDs returns a sorted list of GPU IDs
func (p *DynamicTopologyProvider) getSortedGPUIDs() []string {
	var gpuIDs []string
	for gpuID := range p.topology.Topology {
		gpuIDs = append(gpuIDs, gpuID)
	}

	sort.Strings(gpuIDs)

	return gpuIDs
}

// logSingleGPU logs the topology for a single GPU
func (p *DynamicTopologyProvider) logSingleGPU(gpuID string) {
	gpuTopo := p.topology.Topology[gpuID]
	slog.Info("GPU topology", "gpu", gpuID)

	if len(gpuTopo.Links) == 0 {
		slog.Info("No NVLinks for GPU", "gpu", gpuID)
		return
	}

	slog.Info("GPU links", "gpu", gpuID, "linkCount", len(gpuTopo.Links))

	// Sort link IDs for consistent output
	var linkIDs []string
	for linkID := range gpuTopo.Links {
		linkIDs = append(linkIDs, linkID)
	}

	sort.Strings(linkIDs)

	for _, linkID := range linkIDs {
		link := gpuTopo.Links[linkID]
		slog.Info("GPU link",
			"gpu", gpuID,
			"link", linkID,
			"remotePCI", link.RemotePCI,
			"remoteLink", link.RemoteLink)
	}
}

// parseNVLinkOutputWithPCIAddresses parses the nvidia-smi nvlink -R output and extracts unique PCI addresses
func (p *DynamicTopologyProvider) parseNVLinkOutputWithPCIAddresses(output string) (*NVLinkTopology, []string) {
	topology := &NVLinkTopology{
		Topology: make(map[string]GPUTopology),
	}

	lines := strings.Split(output, "\n")

	var currentGPU string

	var currentLinks map[string]TopologyLink

	pciAddressSet := make(map[string]bool)
	gpuPattern := regexp.MustCompile(`^GPU (\d+):`)
	linkPattern := regexp.MustCompile(`^Link (\d+):\s+Remote Device\s+([0-9a-fA-F:\.]+):\s+Link\s+(\d+)`)

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Check for GPU header
		if matches := gpuPattern.FindStringSubmatch(line); matches != nil {
			// Save previous GPU if exists
			if currentGPU != "" && len(currentLinks) > 0 {
				topology.Topology[currentGPU] = GPUTopology{Links: currentLinks}
			}

			currentGPU = matches[1]
			currentLinks = make(map[string]TopologyLink)

			continue
		}

		// Check for Link line
		if matches := linkPattern.FindStringSubmatch(line); matches != nil && currentGPU != "" {
			linkNum := matches[1]
			remotePCI := matches[2]
			remoteLink, _ := strconv.Atoi(matches[3])

			// Normalize PCI address
			remotePCI = normalizePCIAddress(remotePCI)

			// Collect unique PCI addresses (these are NVSwitch addresses)
			pciAddressSet[remotePCI] = true

			currentLinks[linkNum] = TopologyLink{
				RemotePCI:  remotePCI,
				RemoteLink: remoteLink,
			}
		}
	}

	// Save last GPU
	if currentGPU != "" && len(currentLinks) > 0 {
		topology.Topology[currentGPU] = GPUTopology{Links: currentLinks}
	}

	// Convert PCI address set to sorted slice
	pciAddresses := make([]string, 0, len(pciAddressSet))
	for pci := range pciAddressSet {
		pciAddresses = append(pciAddresses, pci)
	}

	// Sort for consistent ordering
	sort.Strings(pciAddresses)

	return topology, pciAddresses
}

// buildReverseMaps creates efficient lookup maps from the topology
func (p *DynamicTopologyProvider) buildReverseMaps() {
	p.nvswitchPCIToGPUMap = make(map[string]map[int]int)

	if !p.topology.HasNVSwitch {
		return
	}

	// Build pci_address -> nvlink -> gpu map
	for gpuIDStr, gpuTopo := range p.topology.Topology {
		gpuID, err := strconv.Atoi(gpuIDStr)
		if err != nil {
			slog.Warn("Invalid GPU ID in topology", "gpuID", gpuIDStr)
			continue
		}

		for _, link := range gpuTopo.Links {
			// Use normalized, lowercase PCI address directly
			pciAddr := link.RemotePCI
			normalizedPCI := normalizePCIAddress(pciAddr)

			// Initialize map if needed
			if p.nvswitchPCIToGPUMap[normalizedPCI] == nil {
				p.nvswitchPCIToGPUMap[normalizedPCI] = make(map[int]int)
			}

			// Map pci_address+nvlink to GPU
			p.nvswitchPCIToGPUMap[normalizedPCI][link.RemoteLink] = gpuID
		}
	}
}

// normalizePCIAddress normalizes a PCI address by removing leading zeros
func normalizePCIAddress(pci string) string {
	// Convert 00000000:CD:00.0 to 0000:cd:00.0 format
	parts := strings.Split(pci, ":")
	if len(parts) != 3 {
		return strings.ToLower(pci)
	}

	// Take last 4 chars of domain if longer
	domain := parts[0]
	if len(domain) > 4 {
		domain = domain[len(domain)-4:]
	}

	// Lowercase and reconstruct
	return fmt.Sprintf("%s:%s:%s",
		strings.ToLower(domain),
		strings.ToLower(parts[1]),
		strings.ToLower(parts[2]))
}

// GetGPUFromPCINVLink returns the GPU ID for a given PCI address and NVLink port
func (p *DynamicTopologyProvider) GetGPUFromPCINVLink(pciAddress string, nvlinkID int) (int, error) {
	if !p.topology.HasNVSwitch {
		return -1, fmt.Errorf("no NVSwitch present")
	}

	// Normalize PCI address for lookup (lowercase and 4-char domain)
	normalizedPCI := normalizePCIAddress(pciAddress)

	if nvswitchMap, ok := p.nvswitchPCIToGPUMap[normalizedPCI]; ok {
		if gpuID, ok := nvswitchMap[nvlinkID]; ok {
			return gpuID, nil
		}
	}

	return -1, fmt.Errorf("no GPU found for PCI %s, NVLink %d", pciAddress, nvlinkID)
}

// IsPCIAddressNVSwitch checks if a given PCI address is an NVSwitch device
func (p *DynamicTopologyProvider) IsPCIAddressNVSwitch(pciAddress string) bool {
	if !p.topology.HasNVSwitch {
		return false
	}

	// Normalize PCI address for comparison
	normalizedPCI := normalizePCIAddress(pciAddress)

	for _, addr := range p.topology.NVSwitchPCIAddresses {
		if normalizePCIAddress(addr) == normalizedPCI {
			return true
		}
	}

	return false
}

// HasNVSwitch returns true if the loaded topology indicates NVSwitches are present
func (p *DynamicTopologyProvider) HasNVSwitch() bool {
	return p.topology.HasNVSwitch
}

// GetNVSwitchPCIAddresses returns the list of NVSwitch PCI addresses from topology
func (p *DynamicTopologyProvider) GetNVSwitchPCIAddresses() []string {
	return p.topology.NVSwitchPCIAddresses
}

// GetTopology returns the loaded topology data
func (p *DynamicTopologyProvider) GetTopology() *NVLinkTopology {
	return p.topology
}
