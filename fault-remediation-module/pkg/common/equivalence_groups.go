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
	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// RemediationEquivalenceGroups defines groups of remediation actions that are considered
// to have the same operational effect. This is used to prevent multiple, similar remediations
// (like various forms of reboots) from occurring in rapid succession.
// TODO: Fix this multiple mappings as part of https://jirasw.nvidia.com/browse/KACE-1736
var RemediationEquivalenceGroups = map[string][]platformconnector.RecommenedAction{
	"restart": {
		platformconnector.RecommenedAction_COMPONENT_RESET,
		platformconnector.RecommenedAction_RESTART_VM,
		platformconnector.RecommenedAction_RESTART_BM,
	},
}

// GetRemediationGroupForAction returns the equivalence group key for a given action.
// If the action is not part of any group, it returns an empty string.
func GetRemediationGroupForAction(action platformconnector.RecommenedAction) string {
	for group, actions := range RemediationEquivalenceGroups {
		for _, a := range actions {
			if a == action {
				return group
			}
		}
	}

	return ""
}

// GetActionsForGroup returns all actions that belong to a given equivalence group.
func GetActionsForGroup(group string) []platformconnector.RecommenedAction {
	if actions, ok := RemediationEquivalenceGroups[group]; ok {
		return actions
	}

	return nil
}

// IsActionInGroup checks if an action belongs to a specific equivalence group
func IsActionInGroup(action platformconnector.RecommenedAction, group string) bool {
	actions, ok := RemediationEquivalenceGroups[group]
	if !ok {
		return false
	}

	for _, a := range actions {
		if a == action {
			return true
		}
	}

	return false
}
