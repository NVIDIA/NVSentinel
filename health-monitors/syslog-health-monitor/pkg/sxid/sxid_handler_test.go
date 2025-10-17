// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"

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
			handler, err := NewSXIDHandler(tc.nodeName, tc.agentName, tc.componentClass, tc.checkName)

			require.NoError(t, err)
			require.NotNil(t, handler)
			assert.Equal(t, tc.nodeName, handler.nodeName)
			assert.Equal(t, tc.agentName, handler.defaultAgentName)
			assert.Equal(t, tc.componentClass, handler.defaultComponentClass)
			assert.Equal(t, tc.checkName, handler.checkName)
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
