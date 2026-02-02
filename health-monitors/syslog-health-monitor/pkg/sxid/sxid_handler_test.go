// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sxid

import (
	"os"
	"path/filepath"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSXIDHandler(t *testing.T) {
	testCases := []struct {
		name           string
		nodeName       string
		agentName      string
		componentClass string
		checkName      string
	}{
		{
			name:           "Valid Handler Creation",
			nodeName:       "test-node",
			agentName:      "test-agent",
			componentClass: "NVSWITCH",
			checkName:      "sxid-check",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewSXIDHandler(tc.nodeName, tc.agentName, tc.componentClass, tc.checkName, "/tmp/metadata.json", pb.ProcessingStrategy_EXECUTE_REMEDIATION)

			require.NoError(t, err)
			require.NotNil(t, handler)
			assert.Equal(t, tc.nodeName, handler.nodeName)
			assert.Equal(t, tc.agentName, handler.defaultAgentName)
			assert.Equal(t, tc.componentClass, handler.defaultComponentClass)
			assert.Equal(t, tc.checkName, handler.checkName)
			require.NotNil(t, handler.metadataReader)
		})
	}
}

func TestExtractInfoFromNVSwitchErrorMsg(t *testing.T) {
	sxidHandler := &SXIDHandler{}

	testCases := []struct {
		name          string
		line          string
		expectedEvent *sxidErrorEvent
	}{
		{
			name: "Non-fatal error",
			line: "[123] nvidia-nvswitch3: SXid (PCI:0000:c1:00.0): 28006, Non-fatal, Link 46 MC TS crumbstore MCTO (First)",
			expectedEvent: &sxidErrorEvent{
				NVSwitch: 3,
				ErrorNum: 28006,
				PCI:      "0000:c1:00.0",
				Link:     46,
				Message:  "MC TS crumbstore MCTO (First)",
				IsFatal:  false,
			},
		},
		{
			name: "Another non-fatal error",
			line: "[73309.599396] nvidia-nvswitch0: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate",
			expectedEvent: &sxidErrorEvent{
				NVSwitch: 0,
				ErrorNum: 20009,
				PCI:      "0000:06:00.0",
				Link:     4,
				Message:  "RX Short Error Rate",
				IsFatal:  false,
			},
		},
		{
			name: "Fatal error",
			line: "[ 1108.858286] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 24007, Fatal, Link 28 sourcetrack timeout error (First)",
			expectedEvent: &sxidErrorEvent{
				NVSwitch: 0,
				ErrorNum: 24007,
				Link:     28,
				IsFatal:  true,
				Message:  "sourcetrack timeout error (First)",
				PCI:      "0000:c3:00.0",
			},
		},
		{
			name: "Invalid message 1: Link absent",
			line: "nvidia-nvswitch0: SXid (PCI:0004:00:00.0): 26008, SOE Watchdog error",
		},
		{
			name: "Invalid message 2: Link absent",
			line: "nvidia-nvswitch0: SXid (PCI:0004:00:00.0): 26006, SOE HALTED",
		},
		{
			name: "Invalid message 3: Link absent",
			line: "nvidia-nvswitch0: SXid (PCI:0004:00:00.0): 26007, SOE EXTERR",
		},
		{
			name: "Invalid message 4: Link absent",
			line: "nvidia-nvswitch0: SXid (PCI:0004:00:00.0): 26006, SOE HALT data[0] = 0x               0",
		},
		{
			name: "Invalid message 5: Truncated",
			line: "[38889.018130] nvidia-nvswitch1: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Li",
		},
		{
			name: "Invalid message 6: Link absent",
			line: "[38889.018130] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 12033, Severity 1 Engine instance 00 Sub-engine instance 00",
		},
		{
			name: "Invalid message 7: Link keyword but no link number",
			line: "[38889.018130] nvidia-nvswitch1: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sxiderrorEvent, err := sxidHandler.extractInfoFromNVSwitchErrorMsg(tc.line)
			assert.Nilf(t, err, "Error was expected, but got %v", err)

			assert.Equal(t, tc.expectedEvent, sxiderrorEvent)
		})
	}
}

func TestProcessLineWithValidTopologyFatalSXID(t *testing.T) {
	// Create temp metadata file with valid NVSwitch topology
	metadataJSON := `{
		"version": "1.0",
		"timestamp": "2025-01-01T00:00:00Z",
		"node_name": "test-node",
		"gpus": [
			{
				"gpu_id": 1,
				"uuid": "GPU-aaaabbbb-cccc-dddd-eeee-ffffffffffff",
				"pci_address": "0000:18:00.0",
				"serial_number": "GPU-SN-002",
				"device_name": "NVIDIA H100",
				"nvlinks": [
					{
						"link_id": 2,
						"remote_pci_address": "0000:c3:00.0",
						"remote_link_id": 28
					}
				]
			}
		],
		"nvswitches": ["0000:c3:00.0"]
	}`

	tmpDir := t.TempDir()
	metadataPath := filepath.Join(tmpDir, "gpu_metadata.json")
	err := os.WriteFile(metadataPath, []byte(metadataJSON), 0o644)
	require.NoError(t, err)

	handler, err := NewSXIDHandler("test-node", "test-agent", "NVSWITCH", "sxid-check", metadataPath, pb.ProcessingStrategy_STORE_ONLY)
	require.NoError(t, err)

	message := "[ 1108.858286] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 24007, Fatal, Link 28 sourcetrack timeout error (First)"
	events, err := handler.ProcessLine(message)

	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events, 1)

	event := events.Events[0]
	assert.Equal(t, message, event.Message)
	assert.Equal(t, []string{"24007"}, event.ErrorCode)
	assert.True(t, event.IsFatal)
	assert.False(t, event.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, event.RecommendedAction)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy)

	// Verify GPU entity
	assert.Equal(t, "GPU", event.EntitiesImpacted[3].EntityType)
	assert.Equal(t, "1", event.EntitiesImpacted[3].EntityValue)
	assert.Equal(t, "GPU_UUID", event.EntitiesImpacted[4].EntityType)
	assert.Equal(t, "GPU-aaaabbbb-cccc-dddd-eeee-ffffffffffff", event.EntitiesImpacted[4].EntityValue)
}

func TestProcessLine(t *testing.T) {
	testCases := []struct {
		name        string
		message     string
		expectEvent bool
		expectError bool
	}{
		{
			name:        "Invalid SXID - no Link field",
			message:     "nvidia-nvswitch0: SXid (PCI:0004:00:00.0): 26008, SOE Watchdog error",
			expectEvent: false,
			expectError: false,
		},
		{
			name:        "Non-SXID message",
			message:     "Some random log message",
			expectEvent: false,
			expectError: false,
		},
		{
			name:        "Valid SXID but topology unavailable",
			message:     "[123] nvidia-nvswitch3: SXid (PCI:0000:c1:00.0): 28006, Non-fatal, Link 46 MC TS crumbstore MCTO (First)",
			expectEvent: false,
			expectError: true, // getGPUID will fail without topology
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewSXIDHandler("test-node", "test-agent", "NVSWITCH", "sxid-check", "/tmp/metadata.json", pb.ProcessingStrategy_EXECUTE_REMEDIATION)
			require.NoError(t, err)

			events, err := handler.ProcessLine(tc.message)

			if tc.expectError {
				assert.Error(t, err)
				assert.Nil(t, events)
			} else {
				assert.NoError(t, err)
				if tc.expectEvent {
					require.NotNil(t, events)
					require.Len(t, events.Events, 1)
					event := events.Events[0]
					assert.Equal(t, tc.message, event.Message)
					assert.NotEmpty(t, event.Metadata)
					assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)
				} else {
					assert.Nil(t, events)
				}
			}
		})
	}
}
