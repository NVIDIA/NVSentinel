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
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

type EquivalenceGroupConfig struct {
	EffectiveEquivalenceGroup    string
	ImpactedEntityScopeValue     string
	SupersedingEquivalenceGroups []string
}

/*
This functions returns the EffectiveEquivalenceGroup, ImpactedEntityScopeValue and SupersedingEquivalenceGroups
for the given HealthEvent. The EquivalenceGroup depends on the recommended action and set of impacted entities included
in the event. The recommended action maps to a given MaintenanceResource defined in the TomlConfig. Example:

		RESTART_VM:
		  apiGroup: "janitor.dgxc.nvidia.com"
		  version: "v1alpha1"
		  kind: "RebootNode"
		  equivalenceGroup: "restart"

		COMPONENT_RESET:
		  apiGroup: "janitor.dgxc.nvidia.com"
		  version: "v1alpha1"
		  kind: "GPUReset"
		  equivalenceGroup: "reset"
		  supersedingEquivalenceGroups: ["restart"]
	      impactedEntityScope: "GPU_UUID"

For a HealthEvent which includes the RESTART_VM recommended action, we will set EffectiveEquivalenceGroup=restart.
For a HealthEvent which includes the COMPONENT_RESET recommended action that includes an impacted entity as
GPU_UUID=GPU-123, we will define the EffectiveEquivalenceGroup=reset-GPU-123, ImpactedEntityScopeValue=GPU-123, and
SupersedingEquivalenceGroups=["restart"]. Note that we will fail the remediation if a COMPONENT_RESET event is missing
an impacted entity for GPU_UUID. Additionally, we only allow a TomlConfig to define an ImpactedEntityScope if this
entity has been enabled for partial draining (defined in pod_device_annotation.go).

In the second example for COMPONENT_RESET, we will consider the HealthEvent as being a member in both the reset-GPU-123
and restart EquivalenceGroups. Additionally, the ImpactedEntityScope will be passed to the corresponding maintenance
custom resource template.
*/
func GetGroupConfigForEvent(remediationActions map[string]config.MaintenanceResource,
	healthEvent *protos.HealthEvent) (*EquivalenceGroupConfig, error) {
	actionName := healthEvent.RecommendedAction.String()

	actionConfig, exists := remediationActions[actionName]
	if !exists {
		slog.Warn("Action not found in remediation configuration",
			"action", actionName,
			"node", healthEvent.NodeName)

		return nil, nil
	}

	var equivalenceGroupName string

	var impactedEntityScopeValue string

	if len(actionConfig.ImpactedEntityScope) != 0 {
		for _, entity := range healthEvent.GetEntitiesImpacted() {
			if entity.EntityType == actionConfig.ImpactedEntityScope {
				equivalenceGroupName = fmt.Sprintf("%s-%s", actionConfig.EquivalenceGroup, entity.EntityValue)
				impactedEntityScopeValue = entity.EntityValue
			}
		}

		if len(equivalenceGroupName) == 0 {
			return nil, fmt.Errorf("HealthEvent is missing impacted entity for %s required by action %s",
				actionConfig.ImpactedEntityScope, actionName)
		}
	} else {
		equivalenceGroupName = actionConfig.EquivalenceGroup
	}

	return &EquivalenceGroupConfig{
		EffectiveEquivalenceGroup:    equivalenceGroupName,
		ImpactedEntityScopeValue:     impactedEntityScopeValue,
		SupersedingEquivalenceGroups: actionConfig.SupersedingEquivalenceGroups,
	}, nil
}

/*
This function will filter the RemediationStateAnnotation to only include the EquivalenceGroups which match the current
event. Specifically, we will only return the EquivalenceGroupState on the annotation for groups which match the
EffectiveEquivalenceGroup or any SupersedingEquivalenceGroups for the given HealthEvent. For example, if the current
HealthEvent has EffectiveEquivalenceGroup=reset-GPU-123 and SupersedingEquivalenceGroups=["restart"] and then
RemediationStateAnnotation state annotation on the node is

	"restart": {
	  "maintenanceCR": "maintenance-123",
	  "createdAt": "2025-11-13T17:18:32.163469826Z"
	  "actionName", "RESTART_VM",
	},
	"reset-GPU-123": {
	  "maintenanceCR": "maintenance-456",
	  "createdAt": "2025-11-13T17:18:32.163469826Z"
	  "actionName", "COMPONENT_RESET",
	},
	"reset-GPU-456": {
	  "maintenanceCR": "maintenance-789",
	  "createdAt": "2025-11-13T17:18:32.163469826Z"
	  "actionName", "COMPONENT_RESET",
	}

We will return the following EquivalenceGroupStates which match our current event:

	"restart": {
	  "maintenanceCR": "maintenance-123",
	  "createdAt": "2025-11-13T17:18:32.163469826Z"
	  "actionName", "RESTART_VM",
	},
	"reset-GPU-123": {
	  "maintenanceCR": "maintenance-456",
	  "createdAt": "2025-11-13T17:18:32.163469826Z"
	  "actionName", "COMPONENT_RESET",
	}

We will validate that both maintenance-123 and maintenance-456 are not in-progress prior to creating a new maintenance
custom resource for the EffectiveEquivalenceGroup reset-GPU-123.
*/
func FilterEquivalenceGroupStates(groupConfig *EquivalenceGroupConfig,
	remediationState *annotation.RemediationStateAnnotation) map[string]annotation.EquivalenceGroupState {
	allGroups := []string{groupConfig.EffectiveEquivalenceGroup}
	if len(groupConfig.SupersedingEquivalenceGroups) != 0 {
		allGroups = append(allGroups, groupConfig.SupersedingEquivalenceGroups...)
	}

	matchingGroupStates := make(map[string]annotation.EquivalenceGroupState)

	for _, currentGroup := range allGroups {
		if equivalenceGroupState, ok := remediationState.EquivalenceGroups[currentGroup]; ok {
			matchingGroupStates[currentGroup] = equivalenceGroupState
		}
	}

	return matchingGroupStates
}
