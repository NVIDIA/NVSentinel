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

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestActionMapping(t *testing.T) {
	// Test MapActionStringToProto without requiring temp files
	testCases := []struct {
		name          string
		actionStr     string
		expectedCode  int
		expectMapping bool
	}{
		{
			name:          "Valid Action",
			actionStr:     "RESTART_APP",
			expectedCode:  int(pb.RecommendedAction_NONE),
			expectMapping: true,
		},
		{
			name:          "Unknown Action",
			actionStr:     "UNKNOWN_ACTION",
			expectedCode:  int(pb.RecommendedAction_CONTACT_SUPPORT),
			expectMapping: false,
		},
		{
			name:          "XID 154",
			actionStr:     "XID_154_EVAL",
			expectedCode:  int(pb.RecommendedAction_NONE),
			expectMapping: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			action := MapActionStringToProto(tc.actionStr)
			assert.Equal(t, pb.RecommendedAction(tc.expectedCode), action)
		})
	}
}

func TestLoadErrorResolutionMap(t *testing.T) {
	errorMap, err := LoadErrorResolutionMap()
	assert.Nil(t, err)
	assert.NotEmpty(t, errorMap)

	if res, found := errorMap[46]; found {
		assert.Equal(t, pb.RecommendedAction_COMPONENT_RESET, res.RecommendedAction)
	}

	if res, found := errorMap[32]; found {
		assert.Equal(t, pb.RecommendedAction_NONE, res.RecommendedAction)
	}
}

func TestMapActionStringToProto(t *testing.T) {
	testcases := []struct {
		input          string
		expectedOutput pb.RecommendedAction
	}{
		{
			input:          "COMPONENT_RESET",
			expectedOutput: pb.RecommendedAction_COMPONENT_RESET,
		},
		{
			input:          "RESTART_APP",
			expectedOutput: pb.RecommendedAction_NONE,
		},
		{
			input:          "CONTACT_SUPPORT",
			expectedOutput: pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			input:          "IGNORE",
			expectedOutput: pb.RecommendedAction_NONE,
		},
		{
			input:          "WORKFLOW_XID_48",
			expectedOutput: pb.RecommendedAction_COMPONENT_RESET,
		},
		{
			input:          "RESET_GPU",
			expectedOutput: pb.RecommendedAction_COMPONENT_RESET,
		},
		{
			input:          "RECOVER_FEATURE_RESET_GPU",
			expectedOutput: pb.RecommendedAction_COMPONENT_RESET,
		},
		{
			input:          "WORKFLOW_XID_168",
			expectedOutput: pb.RecommendedAction_RESTART_VM,
		},
		{
			input:          "RESET_FABRIC",
			expectedOutput: pb.RecommendedAction_RESTART_VM,
		},
		{
			input:          "NONE",
			expectedOutput: pb.RecommendedAction_NONE,
		},
		{
			input:          "  CONTACT_SUPPORT  ",
			expectedOutput: pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			input:          "SOME_UNKNOWN_ACTION",
			expectedOutput: pb.RecommendedAction_CONTACT_SUPPORT,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.input, func(t *testing.T) {
			output := MapActionStringToProto(tc.input)
			assert.Equal(t, tc.expectedOutput, output)
		})
	}
}

func TestGetNVL5DecodingRules(t *testing.T) {
	// Rules carry both V1 and V2 IntrInfo patterns; selection happens per message.
	rules, err := GetNVL5DecodingRules()
	assert.NoError(t, err)

	// Verify all NVL5 XIDs (144-150) are loaded
	for xid := 144; xid <= 150; xid++ {
		assert.Contains(t, rules, xid)
	}

	// Test concrete values from xlsx for XID 145
	assert.Len(t, rules[145], 33, "XID 145 should have 33 rules")

	// Verify first rule (RLW_CTRL) - concrete values from xlsx
	rule0 := rules[145][0]
	assert.Equal(t, 145, rule0.XIDNumber)
	assert.Equal(t, "RLW_CTRL", rule0.Mnemonic)
	assert.Equal(t, "Non-fatal", rule0.Severity)
	assert.Equal(t, "IGNORE", rule0.Resolution)
	assert.Equal(t, "------000000----------0001100010", rule0.IntrInfoBinaryV1) // V1 pattern (Column C)
	assert.Equal(t, "------000000-------------0000011", rule0.IntrInfoBinaryV2)  // V2 pattern (Column D)
	assert.Equal(t, []string{"0x80000000"}, rule0.ErrorStatusHex)

	// Verify second rule (RLW_REMAP with XID_154_EVAL)
	rule1 := rules[145][1]
	assert.Equal(t, "RLW_REMAP", rule1.Mnemonic)
	assert.Equal(t, "XID_154_EVAL", rule1.Resolution)
	assert.Equal(t, "------000000----------0010000010", rule1.IntrInfoBinaryV1) // V1
	assert.Equal(t, "------000000-------------0000100", rule1.IntrInfoBinaryV2)  // V2
}

func TestIsDriverVersionR575OrNewer(t *testing.T) {
	tests := []struct {
		version  string
		expected bool
	}{
		// Pre-R575
		{"570.148.08", false},
		{"574.99.99", false},
		{"535.104.05", false},
		// R575+
		{"575.0.0", true},
		{"575.51.02", true},
		{"580.0.0", true},
		{"590.100.05", true},
		// Edge cases
		{"", false},
		{"invalid", false},
		{"580", true},
	}

	for _, tc := range tests {
		result := IsDriverVersionR575OrNewer(tc.version)
		assert.Equal(t, tc.expected, result, "IsDriverVersionR575OrNewer(%q)", tc.version)
	}
}
