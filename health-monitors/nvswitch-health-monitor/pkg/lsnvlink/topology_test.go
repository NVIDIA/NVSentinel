// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
)

func TestParseNVLinkOutputWithPCIAddresses(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test input mimicking actual nvidia-smi nvlink -R output
	testOutput := `GPU 0: NVIDIA H100 (UUID: GPU-12345678-1234-1234-1234-123456789abc)
         Link 0: Remote Device 00000000:CD:00.0: Link 25
         Link 1: Remote Device 00000000:CD:00.0: Link 24
         Link 2: Remote Device 00000000:CA:00.0: Link 30

GPU 1: NVIDIA H100 (UUID: GPU-87654321-4321-4321-4321-abc987654321)
         Link 0: Remote Device 00000000:CD:00.0: Link 27
         Link 1: Remote Device 00000000:CD:00.0: Link 26
         Link 2: Remote Device 00000000:CA:00.0: Link 10
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Verify topology was parsed correctly
	if len(topology.Topology) != 2 {
		t.Errorf("Expected 2 GPUs in topology, got %d", len(topology.Topology))
	}

	// Verify extracted PCI addresses
	if len(pciAddresses) != 2 {
		t.Errorf("Expected 2 unique PCI addresses, got %d", len(pciAddresses))
	}

	expectedPCIs := []string{"0000:ca:00.0", "0000:cd:00.0"} // Sorted order
	for i, expected := range expectedPCIs {
		if i < len(pciAddresses) && pciAddresses[i] != expected {
			t.Errorf("Expected PCI address %s at index %d, got %s", expected, i, pciAddresses[i])
		}
	}

	// Check GPU 0 links
	gpu0, ok := topology.Topology["0"]
	if !ok {
		t.Fatal("GPU 0 not found in topology")
	}

	if len(gpu0.Links) != 3 {
		t.Errorf("Expected 3 links for GPU 0, got %d", len(gpu0.Links))
	}

	// Check specific link
	link0, ok := gpu0.Links["0"]
	if !ok {
		t.Fatal("Link 0 not found for GPU 0")
	}

	expectedPCI := "0000:cd:00.0"
	if link0.RemotePCI != expectedPCI {
		t.Errorf("Expected PCI %s for GPU 0 Link 0, got %s", expectedPCI, link0.RemotePCI)
	}

	if link0.RemoteLink != 25 {
		t.Errorf("Expected remote link 25 for GPU 0 Link 0, got %d", link0.RemoteLink)
	}

	// Check GPU 1 links
	gpu1, ok := topology.Topology["1"]
	if !ok {
		t.Fatal("GPU 1 not found in topology")
	}

	if len(gpu1.Links) != 3 {
		t.Errorf("Expected 3 links for GPU 1, got %d", len(gpu1.Links))
	}
}

func TestParseNVLinkOutput_EmptyOutput(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test empty output (no NVSwitches/NVLinks)
	testOutput := ""

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Should return empty topology
	if len(topology.Topology) != 0 {
		t.Errorf("Expected 0 GPUs in topology for empty output, got %d", len(topology.Topology))
	}

	if len(pciAddresses) != 0 {
		t.Errorf("Expected 0 PCI addresses for empty output, got %d", len(pciAddresses))
	}
}

func TestParseNVLinkOutput_NoLinks(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test output with GPUs but no NVLinks
	testOutput := `GPU 0: NVIDIA V100-SXM2-32GB (UUID: GPU-12345678-1234-1234-1234-123456789abc)

GPU 1: NVIDIA V100-SXM2-32GB (UUID: GPU-87654321-4321-4321-4321-abc987654321)
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Should have GPUs but no links
	if len(topology.Topology) != 0 {
		t.Errorf("Expected 0 GPUs with links, got %d", len(topology.Topology))
	}

	if len(pciAddresses) != 0 {
		t.Errorf("Expected 0 PCI addresses when no links present, got %d", len(pciAddresses))
	}
}

func TestParseNVLinkOutput_P2PLinks(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test output with P2P NVLinks (GPU to GPU, no NVSwitch)
	testOutput := `GPU 0: NVIDIA V100-SXM2-32GB (UUID: GPU-12345678-1234-1234-1234-123456789abc)
         Link 0: Remote Device GPU 1: Link 0
         Link 1: Remote Device GPU 1: Link 1

GPU 1: NVIDIA V100-SXM2-32GB (UUID: GPU-87654321-4321-4321-4321-abc987654321)
         Link 0: Remote Device GPU 0: Link 0
         Link 1: Remote Device GPU 0: Link 1
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Should have no NVSwitch PCI addresses (P2P links don't have PCI addresses)
	if len(pciAddresses) != 0 {
		t.Errorf("Expected 0 PCI addresses for P2P links, got %d", len(pciAddresses))
	}

	// Topology should be empty as our regex expects PCI addresses
	if len(topology.Topology) != 0 {
		t.Errorf("Expected 0 GPUs in topology for P2P links, got %d", len(topology.Topology))
	}
}

func TestParseNVLinkOutput_LargeA100Topology(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test with actual A100 8-GPU system output (truncated for brevity)
	testOutput := `GPU 0: NVIDIA A100-SXM4-80GB (UUID: GPU-22e6cd4f-9e28-603b-8d78-a7fecfcade14)
         Link 0: Remote Device 00000000:CD:00.0: Link 25
         Link 1: Remote Device 00000000:CD:00.0: Link 24
         Link 2: Remote Device 00000000:CA:00.0: Link 30
         Link 3: Remote Device 00000000:CA:00.0: Link 31
         Link 4: Remote Device 00000000:CC:00.0: Link 26
         Link 5: Remote Device 00000000:CC:00.0: Link 27
         Link 6: Remote Device 00000000:CF:00.0: Link 31
         Link 7: Remote Device 00000000:CF:00.0: Link 30
         Link 8: Remote Device 00000000:CB:00.0: Link 27
         Link 9: Remote Device 00000000:CB:00.0: Link 26
         Link 10: Remote Device 00000000:CE:00.0: Link 10
         Link 11: Remote Device 00000000:CE:00.0: Link 11

GPU 1: NVIDIA A100-SXM4-80GB (UUID: GPU-dfb6a8bd-a010-6291-efc5-2c5e57501960)
         Link 0: Remote Device 00000000:CD:00.0: Link 27
         Link 1: Remote Device 00000000:CD:00.0: Link 26
         Link 2: Remote Device 00000000:CA:00.0: Link 10
         Link 3: Remote Device 00000000:CA:00.0: Link 11
         Link 4: Remote Device 00000000:CC:00.0: Link 28
         Link 5: Remote Device 00000000:CC:00.0: Link 29
         Link 6: Remote Device 00000000:CF:00.0: Link 29
         Link 7: Remote Device 00000000:CF:00.0: Link 28
         Link 8: Remote Device 00000000:CB:00.0: Link 11
         Link 9: Remote Device 00000000:CB:00.0: Link 10
         Link 10: Remote Device 00000000:CE:00.0: Link 27
         Link 11: Remote Device 00000000:CE:00.0: Link 26
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Verify topology was parsed correctly
	if len(topology.Topology) != 2 {
		t.Errorf("Expected 2 GPUs in topology, got %d", len(topology.Topology))
	}

	// Verify extracted PCI addresses (6 unique NVSwitches in A100 DGX)
	if len(pciAddresses) != 6 {
		t.Errorf("Expected 6 unique NVSwitch PCI addresses, got %d", len(pciAddresses))
	}

	// Check sorted order
	expectedPCIs := []string{"0000:ca:00.0", "0000:cb:00.0", "0000:cc:00.0", "0000:cd:00.0", "0000:ce:00.0", "0000:cf:00.0"}
	for i, expected := range expectedPCIs {
		if i < len(pciAddresses) && pciAddresses[i] != expected {
			t.Errorf("Expected PCI address %s at index %d, got %s", expected, i, pciAddresses[i])
		}
	}

	// Check GPU 0 has 12 links (A100 has 12 NVLinks per GPU)
	gpu0, ok := topology.Topology["0"]
	if !ok {
		t.Fatal("GPU 0 not found in topology")
	}

	if len(gpu0.Links) != 12 {
		t.Errorf("Expected 12 links for A100 GPU 0, got %d", len(gpu0.Links))
	}
}

func TestParseNVLinkOutput_MalformedOutput(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test with malformed output
	testOutput := `Some random text
This is not valid nvidia-smi output
Link 0: Invalid format
GPU X: Not a valid GPU line
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Should handle gracefully and return empty
	if len(topology.Topology) != 0 {
		t.Errorf("Expected 0 GPUs in topology for malformed output, got %d", len(topology.Topology))
	}

	if len(pciAddresses) != 0 {
		t.Errorf("Expected 0 PCI addresses for malformed output, got %d", len(pciAddresses))
	}
}

func TestParseNVLinkOutput_MixedValidInvalid(t *testing.T) {
	provider := &DynamicTopologyProvider{}

	// Test with mixed valid and invalid lines
	testOutput := `GPU 0: NVIDIA H100 (UUID: GPU-12345678-1234-1234-1234-123456789abc)
         Link 0: Remote Device 00000000:CD:00.0: Link 25
         Invalid line here
         Link 1: Remote Device 00000000:CD:00.0: Link 24
         Link X: Invalid link line
         Link 2: Remote Device 00000000:CA:00.0: Link 30

Some garbage text

GPU 1: NVIDIA H100 (UUID: GPU-87654321-4321-4321-4321-abc987654321)
         Link 0: Remote Device 00000000:CD:00.0: Link 27
`

	topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(testOutput)

	// Should parse valid lines and ignore invalid ones
	if len(topology.Topology) != 2 {
		t.Errorf("Expected 2 GPUs in topology despite invalid lines, got %d", len(topology.Topology))
	}

	// Should have found 2 unique NVSwitch PCI addresses
	if len(pciAddresses) != 2 {
		t.Errorf("Expected 2 PCI addresses despite invalid lines, got %d", len(pciAddresses))
	}

	// GPU 0 should have 3 valid links
	gpu0, ok := topology.Topology["0"]
	if !ok {
		t.Fatal("GPU 0 not found in topology")
	}

	if len(gpu0.Links) != 3 {
		t.Errorf("Expected 3 valid links for GPU 0, got %d", len(gpu0.Links))
	}

	// GPU 1 should have 1 valid link
	gpu1, ok := topology.Topology["1"]
	if !ok {
		t.Fatal("GPU 1 not found in topology")
	}

	if len(gpu1.Links) != 1 {
		t.Errorf("Expected 1 valid link for GPU 1, got %d", len(gpu1.Links))
	}
}

func TestNormalizePCIAddress(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"00000000:CD:00.0", "0000:cd:00.0"},
		{"0000:AB:12.3", "0000:ab:12.3"},
		{"12345678:EF:00.0", "5678:ef:00.0"}, // Takes last 4 chars of domain
		{"invalid", "invalid"},               // Invalid format preserved
		{"0000:cd:00.0", "0000:cd:00.0"},     // Already normalized
		{"FFFF:FF:FF.F", "ffff:ff:ff.f"},     // All uppercase
		{"0:1:2.3", "0:1:2.3"},               // Short format preserved
	}

	for _, tc := range testCases {
		result := normalizePCIAddress(tc.input)
		if result != tc.expected {
			t.Errorf("normalizePCIAddress(%s) = %s, expected %s", tc.input, result, tc.expected)
		}
	}
}

func TestDynamicTopologyProvider_BuildReverseMaps(t *testing.T) {
	provider := &DynamicTopologyProvider{
		topology: &NVLinkTopology{
			HasNVSwitch: true,
			Topology: map[string]GPUTopology{
				"0": {
					Links: map[string]TopologyLink{
						"0": {RemotePCI: "0000:cd:00.0", RemoteLink: 25},
						"1": {RemotePCI: "0000:cd:00.0", RemoteLink: 24},
						"2": {RemotePCI: "0000:ca:00.0", RemoteLink: 30},
					},
				},
				"1": {
					Links: map[string]TopologyLink{
						"0": {RemotePCI: "0000:cd:00.0", RemoteLink: 27},
						"1": {RemotePCI: "0000:cd:00.0", RemoteLink: 26},
						"2": {RemotePCI: "0000:ca:00.0", RemoteLink: 10},
					},
				},
			},
			NVSwitchPCIAddresses: []string{"0000:cd:00.0", "0000:ca:00.0"},
		},
	}

	provider.buildReverseMaps()

	// Test GetGPUFromPCINVLink
	testCases := []struct {
		pciAddress  string
		nvlinkID    int
		expectedGPU int
		shouldError bool
	}{
		{"0000:cd:00.0", 25, 0, false},     // PCI 0000:cd:00.0, Link 25 -> GPU 0
		{"0000:cd:00.0", 24, 0, false},     // PCI 0000:cd:00.0, Link 24 -> GPU 0
		{"0000:cd:00.0", 27, 1, false},     // PCI 0000:cd:00.0, Link 27 -> GPU 1
		{"0000:cd:00.0", 26, 1, false},     // PCI 0000:cd:00.0, Link 26 -> GPU 1
		{"0000:ca:00.0", 30, 0, false},     // PCI 0000:ca:00.0, Link 30 -> GPU 0
		{"0000:ca:00.0", 10, 1, false},     // PCI 0000:ca:00.0, Link 10 -> GPU 1
		{"00000000:CD:00.0", 25, 0, false}, // Test with uppercase and longer format
		{"0000:cd:00.0", 99, -1, true},     // Invalid link
		{"0000:ff:00.0", 25, -1, true},     // Invalid PCI address
	}

	for _, tc := range testCases {
		gpuID, err := provider.GetGPUFromPCINVLink(tc.pciAddress, tc.nvlinkID)
		if tc.shouldError {
			if err == nil {
				t.Errorf("Expected error for PCI %s, Link %d", tc.pciAddress, tc.nvlinkID)
			}
		} else {
			if err != nil {
				t.Errorf("Unexpected error for PCI %s, Link %d: %v", tc.pciAddress, tc.nvlinkID, err)
			}
			if gpuID != tc.expectedGPU {
				t.Errorf("Expected GPU %d for PCI %s, Link %d, got %d",
					tc.expectedGPU, tc.pciAddress, tc.nvlinkID, gpuID)
			}
		}
	}

	// Test IsPCIAddressNVSwitch
	pciTests := []struct {
		pciAddress string
		expected   bool
	}{
		{"0000:cd:00.0", true},
		{"0000:ca:00.0", true},
		{"00000000:CD:00.0", true}, // Test normalization
		{"00000000:CA:00.0", true}, // Test normalization
		{"0000:ff:00.0", false},    // Invalid PCI
	}

	for _, tc := range pciTests {
		result := provider.IsPCIAddressNVSwitch(tc.pciAddress)
		if result != tc.expected {
			t.Errorf("Expected IsPCIAddressNVSwitch(%s) to return %v, got %v",
				tc.pciAddress, tc.expected, result)
		}
	}
}

func TestDynamicTopologyProvider_NoNVSwitch(t *testing.T) {
	// Test provider behavior when no NVSwitches are present
	provider := &DynamicTopologyProvider{
		topology: &NVLinkTopology{
			HasNVSwitch:          false,
			Topology:             make(map[string]GPUTopology),
			NVSwitchPCIAddresses: []string{},
		},
	}

	// Test HasNVSwitch
	if provider.HasNVSwitch() {
		t.Error("Expected HasNVSwitch to return false")
	}

	// Test GetGPUFromPCINVLink - should error
	_, err := provider.GetGPUFromPCINVLink("0000:cd:00.0", 25)
	if err == nil {
		t.Error("Expected error when no NVSwitch present")
	}

	// Test IsPCIAddressNVSwitch - should return false
	if provider.IsPCIAddressNVSwitch("0000:cd:00.0") {
		t.Error("Expected IsPCIAddressNVSwitch to return false when no NVSwitch present")
	}

	// Test GetNVSwitchPCIAddresses - should return empty
	addrs := provider.GetNVSwitchPCIAddresses()
	if len(addrs) != 0 {
		t.Errorf("Expected empty PCI addresses, got %d", len(addrs))
	}
}

func TestGatherTopology_EdgeCases(t *testing.T) {
	// Test that GatherTopology never returns an error
	// Since we can't easily mock exec.Command in this test,
	// we test the parseNVLinkOutputWithPCIAddresses function with various inputs
	// to ensure it handles all edge cases gracefully

	provider := &DynamicTopologyProvider{
		topology: &NVLinkTopology{
			HasNVSwitch:          false,
			Topology:             make(map[string]GPUTopology),
			NVSwitchPCIAddresses: []string{},
		},
	}

	testCases := []struct {
		name           string
		input          string
		expectError    bool
		expectNVSwitch bool
	}{
		{
			name:           "Empty output",
			input:          "",
			expectError:    false,
			expectNVSwitch: false,
		},
		{
			name:           "Only whitespace",
			input:          "   \n\t\n   ",
			expectError:    false,
			expectNVSwitch: false,
		},
		{
			name:           "No Link text",
			input:          "GPU 0: NVIDIA V100\nGPU 1: NVIDIA V100",
			expectError:    false,
			expectNVSwitch: false,
		},
		{
			name:           "Malformed output",
			input:          "Random text\nNot nvidia-smi output\n",
			expectError:    false,
			expectNVSwitch: false,
		},
		{
			name:           "Valid output with NVSwitches",
			input:          "GPU 0: NVIDIA H100\n         Link 0: Remote Device 00000000:CD:00.0: Link 25",
			expectError:    false,
			expectNVSwitch: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			topology, pciAddresses := provider.parseNVLinkOutputWithPCIAddresses(tc.input)

			// Should never error - always returns valid (possibly empty) results
			if topology == nil {
				t.Error("parseNVLinkOutputWithPCIAddresses should never return nil topology")
			}

			hasNVSwitch := len(pciAddresses) > 0
			if hasNVSwitch != tc.expectNVSwitch {
				t.Errorf("Expected hasNVSwitch=%v for %s, got %v", tc.expectNVSwitch, tc.name, hasNVSwitch)
			}
		})
	}
}
