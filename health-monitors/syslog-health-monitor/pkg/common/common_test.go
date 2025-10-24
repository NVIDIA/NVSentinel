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
			expectedCode:  int(pb.RecommenedAction_NONE),
			expectMapping: true,
		},
		{
			name:          "Unknown Action",
			actionStr:     "UNKNOWN_ACTION",
			expectedCode:  int(pb.RecommenedAction_CONTACT_SUPPORT),
			expectMapping: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			action := MapActionStringToProto(tc.actionStr)
			assert.Equal(t, pb.RecommenedAction(tc.expectedCode), action)
		})
	}
}

func TestLoadErrorResolutionMap(t *testing.T) {
	errorMap, err := LoadErrorResolutionMap()
	assert.Nil(t, err)
	assert.NotEmpty(t, errorMap)

	if res, found := errorMap[46]; found {
		assert.Equal(t, pb.RecommenedAction_COMPONENT_RESET, res.RecommendedAction)
	}

	if res, found := errorMap[32]; found {
		assert.Equal(t, pb.RecommenedAction_NONE, res.RecommendedAction)
	}
}

func TestMapActionStringToProto(t *testing.T) {
	testcases := []struct {
		input          string
		expectedOutput pb.RecommenedAction
	}{
		{
			input:          "COMPONENT_RESET",
			expectedOutput: pb.RecommenedAction_COMPONENT_RESET,
		},
		{
			input:          "RESTART_APP",
			expectedOutput: pb.RecommenedAction_NONE,
		},
		{
			input:          "CONTACT_SUPPORT",
			expectedOutput: pb.RecommenedAction_CONTACT_SUPPORT,
		},
		{
			input:          "IGNORE",
			expectedOutput: pb.RecommenedAction_NONE,
		},
		{
			input:          "WORKFLOW_XID_48",
			expectedOutput: pb.RecommenedAction_COMPONENT_RESET,
		},
		{
			input:          "RESET_GPU",
			expectedOutput: pb.RecommenedAction_COMPONENT_RESET,
		},
		{
			input:          "RESET_FABRIC",
			expectedOutput: pb.RecommenedAction_COMPONENT_RESET,
		},
		{
			input:          "NONE",
			expectedOutput: pb.RecommenedAction_NONE,
		},
		{
			input:          "  CONTACT_SUPPORT  ",
			expectedOutput: pb.RecommenedAction_CONTACT_SUPPORT,
		},
		{
			input:          "SOME_UNKNOWN_ACTION",
			expectedOutput: pb.RecommenedAction_CONTACT_SUPPORT,
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
	rules, err := GetNVL5DecodingRules()
	assert.Nil(t, err)
	assert.NotEmpty(t, rules)
}
