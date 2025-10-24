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

package parser

import (
	"testing"

	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCSVParser_Parse(t *testing.T) {
	errorResolutionMap, err := common.LoadErrorResolutionMap()
	if err != nil {
		t.Fatalf("Failed to load embedded XID mapping: %v", err)
	}
	require.NotEmpty(t, errorResolutionMap, "XID error resolution map should not be empty")

	nvl5Rules, err := common.GetNVL5DecodingRules()
	if err != nil {
		t.Fatalf("Failed to load embedded XID mapping: %v", err)
	}
	require.NotEmpty(t, nvl5Rules, "XID error resolution map should not be empty")

	parser := NewCSVParser("test-node", errorResolutionMap, nvl5Rules)

	testCases := []struct {
		name              string
		message           string
		expectedSuccess   bool
		expectedXIDCode   int
		expectedPCIAddr   string
		expectedAction    pb.RecommenedAction
		expectedMnemonic  string
		expectedErrorCode string
	}{
		{
			name:              "NL5 XID",
			message:           "NVRM: Xid (PCI:0018:01:00): 149, NETIR_LINK_EVT  Fatal   XC0 i0 Link 09 (0x025525c6 0x00000000 0x00000000 0x00000000 0x00000000 0x00000000)",
			expectedSuccess:   true,
			expectedXIDCode:   149,
			expectedPCIAddr:   "0018:01:00",
			expectedAction:    pb.RecommenedAction_COMPONENT_RESET,
			expectedMnemonic:  "NETIR_LINK_EVT/NETIR_LINK_DOWN",
			expectedErrorCode: "149.NETIR_LINK_EVT",
		},
		{
			name:              "Complex XID format with all fields",
			message:           "NVRM: Xid (PCI:0000:66:00): 32, pid=2280636, name=train.3, Channel ID 0000000d intr0 00040000",
			expectedSuccess:   true,
			expectedXIDCode:   32,
			expectedPCIAddr:   "0000:66:00",
			expectedAction:    pb.RecommenedAction_NONE,
			expectedMnemonic:  "XID 32",
			expectedErrorCode: "32",
		},
		{
			name:              "Minimal XID format",
			message:           "NVRM: Xid (PCI:0000:3f:00): 46",
			expectedSuccess:   true,
			expectedXIDCode:   46,
			expectedPCIAddr:   "0000:3f:00",
			expectedAction:    pb.RecommenedAction_COMPONENT_RESET,
			expectedMnemonic:  "XID 46",
			expectedErrorCode: "46",
		},
		{
			name:              "Different PCI address format",
			message:           "NVRM: Xid (PCI:0001:ab:cd): 69, pid=1310987, name=train.3, Class Error: ChId 0008, Class 0000cbc0, Offset 00000184, Data ffffffff, ErrorCode 00000004",
			expectedSuccess:   true,
			expectedXIDCode:   69,
			expectedPCIAddr:   "0001:ab:cd",
			expectedAction:    pb.RecommenedAction_NONE,
			expectedMnemonic:  "XID 69",
			expectedErrorCode: "69",
		},
		{
			name:              "Different XID code",
			message:           "NVRM: Xid (PCI:0000:9b:00): 8, pid=5678, name=idle_timeout",
			expectedSuccess:   true,
			expectedXIDCode:   8,
			expectedPCIAddr:   "0000:9b:00",
			expectedAction:    pb.RecommenedAction_NONE,
			expectedMnemonic:  "XID 8",
			expectedErrorCode: "8",
		},
		{
			name:              "Unknown XID defaults to CONTACT_SUPPORT",
			message:           "NVRM: Xid (PCI:0000:66:00): 999, pid=1234, name=test",
			expectedSuccess:   true,
			expectedXIDCode:   999,
			expectedPCIAddr:   "0000:66:00",
			expectedAction:    pb.RecommenedAction_CONTACT_SUPPORT,
			expectedMnemonic:  "XID 999",
			expectedErrorCode: "999",
		},
		{
			name:            "Non-XID NVRM message",
			message:         "NVRM: GPU at PCI:0000:66:00: GPU-12345678-1234-5678-9abc-def012345678",
			expectedSuccess: false,
		},
		{
			name:            "Non-NVRM message",
			message:         "[Wed Dec 11 13:23:01 2024] Linux version 5.15.0-117-generic",
			expectedSuccess: false,
		},
		{
			name:            "Empty message",
			message:         "",
			expectedSuccess: false,
		},
		{
			name:            "Malformed XID - missing PCI",
			message:         "NVRM: Xid : 32, pid=2280636",
			expectedSuccess: false,
		},
		{
			name:            "Malformed XID - invalid number",
			message:         "NVRM: Xid (PCI:0000:66:00): abc, pid=2280636",
			expectedSuccess: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parser.Parse(tc.message)

			if !tc.expectedSuccess {
				assert.False(t, result.Success, "Expected Success to be false for unsuccessful case")
				return
			}

			require.NoError(t, err, "Parse should not return error for valid XID message")
			require.NotNil(t, result, "Result should not be nil for valid XID message")
			assert.True(t, result.Success, "Parse should succeed for valid XID message")

			assert.Equal(t, tc.expectedXIDCode, result.Result.Number, "XID code should match")
			assert.Equal(t, tc.expectedPCIAddr, result.Result.PCIE, "PCI address should match")
			assert.Equal(t, tc.expectedMnemonic, result.Result.Mnemonic, "Mnemonic should match")
			assert.Equal(t, tc.expectedErrorCode, result.Result.DecodedXIDStr, "Decoded XID string should match")
			assert.Equal(t, tc.expectedErrorCode, result.Result.Name, "Name should match")

			if tc.expectedXIDCode != 999 {
				assert.NotEqual(t, pb.RecommenedAction_CONTACT_SUPPORT.String(), result.Result.Resolution,
					"XID %d should have a specific mapping from real CSV, not default CONTACT_SUPPORT", tc.expectedXIDCode)
			} else {
				assert.Equal(t, pb.RecommenedAction_CONTACT_SUPPORT.String(), result.Result.Resolution,
					"Unknown XID should default to CONTACT_SUPPORT")
			}

			assert.Empty(t, result.Result.Driver, "Driver should be empty as requested")
			assert.Empty(t, result.Error, "Error field should be empty for successful parse")
		})
	}
}
