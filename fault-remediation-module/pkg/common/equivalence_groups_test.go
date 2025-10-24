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

package common

import (
	"testing"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
)

func TestGetRemediationGroupForAction(t *testing.T) {
	tests := []struct {
		name          string
		action        platformconnector.RecommenedAction
		expectedGroup string
	}{
		{
			name:          "COMPONENT_RESET returns restart group",
			action:        platformconnector.RecommenedAction_COMPONENT_RESET,
			expectedGroup: "restart",
		},
		{
			name:          "RESTART_VM returns restart group",
			action:        platformconnector.RecommenedAction_RESTART_VM,
			expectedGroup: "restart",
		},
		{
			name:          "RESTART_BM returns restart group",
			action:        platformconnector.RecommenedAction_RESTART_BM,
			expectedGroup: "restart",
		},
		{
			name:          "CONTACT_SUPPORT returns empty string (not in any group)",
			action:        platformconnector.RecommenedAction_CONTACT_SUPPORT,
			expectedGroup: "",
		},
		{
			name:          "NONE returns empty string (not in any group)",
			action:        platformconnector.RecommenedAction_NONE,
			expectedGroup: "",
		},
		{
			name:          "UNKNOWN returns empty string (not in any group)",
			action:        platformconnector.RecommenedAction_UNKNOWN,
			expectedGroup: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := GetRemediationGroupForAction(tt.action)
			assert.Equal(t, tt.expectedGroup, group)
		})
	}
}

func TestGetActionsForGroup(t *testing.T) {
	tests := []struct {
		name            string
		group           string
		expectedActions []platformconnector.RecommenedAction
	}{
		{
			name:  "restart group returns all restart-related actions",
			group: "restart",
			expectedActions: []platformconnector.RecommenedAction{
				platformconnector.RecommenedAction_COMPONENT_RESET,
				platformconnector.RecommenedAction_RESTART_VM,
				platformconnector.RecommenedAction_RESTART_BM,
			},
		},
		{
			name:            "non-existent group returns nil",
			group:           "non-existent",
			expectedActions: nil,
		},
		{
			name:            "empty string returns nil",
			group:           "",
			expectedActions: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := GetActionsForGroup(tt.group)
			if tt.expectedActions == nil {
				assert.Nil(t, actions)
			} else {
				assert.ElementsMatch(t, tt.expectedActions, actions)
			}
		})
	}
}

func TestIsActionInGroup(t *testing.T) {
	tests := []struct {
		name     string
		action   platformconnector.RecommenedAction
		group    string
		expected bool
	}{
		{
			name:     "COMPONENT_RESET is in restart group",
			action:   platformconnector.RecommenedAction_COMPONENT_RESET,
			group:    "restart",
			expected: true,
		},
		{
			name:     "RESTART_VM is in restart group",
			action:   platformconnector.RecommenedAction_RESTART_VM,
			group:    "restart",
			expected: true,
		},
		{
			name:     "RESTART_BM is in restart group",
			action:   platformconnector.RecommenedAction_RESTART_BM,
			group:    "restart",
			expected: true,
		},
		{
			name:     "CONTACT_SUPPORT is not in restart group",
			action:   platformconnector.RecommenedAction_CONTACT_SUPPORT,
			group:    "restart",
			expected: false,
		},
		{
			name:     "NONE is not in restart group",
			action:   platformconnector.RecommenedAction_NONE,
			group:    "restart",
			expected: false,
		},
		{
			name:     "RESTART_VM is not in non-existent group",
			action:   platformconnector.RecommenedAction_RESTART_VM,
			group:    "non-existent",
			expected: false,
		},
		{
			name:     "any action in empty group returns false",
			action:   platformconnector.RecommenedAction_RESTART_VM,
			group:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsActionInGroup(tt.action, tt.group)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestEquivalenceGroupsConsistency(t *testing.T) {
	restartActions := GetActionsForGroup("restart")
	assert.NotNil(t, restartActions, "restart group should exist")
	assert.NotEmpty(t, restartActions, "restart group should not be empty")

	for _, action := range restartActions {
		group := GetRemediationGroupForAction(action)
		assert.Equal(t, "restart", group, "action %v should map to restart group", action)

		inGroup := IsActionInGroup(action, "restart")
		assert.True(t, inGroup, "action %v should be in restart group", action)
	}
}

func TestRemediationEquivalenceGroupsStructure(t *testing.T) {
	assert.NotEmpty(t, RemediationEquivalenceGroups, "RemediationEquivalenceGroups should not be empty")

	restartGroup, exists := RemediationEquivalenceGroups["restart"]
	assert.True(t, exists, "restart group should exist in RemediationEquivalenceGroups")
	assert.NotEmpty(t, restartGroup, "restart group should contain actions")

	for groupName, actions := range RemediationEquivalenceGroups {
		seen := make(map[platformconnector.RecommenedAction]bool)
		for _, action := range actions {
			assert.False(t, seen[action], "duplicate action %v found in group %s", action, groupName)
			seen[action] = true
		}
	}
}
