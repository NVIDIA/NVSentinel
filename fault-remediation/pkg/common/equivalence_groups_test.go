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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/drain"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

func TestGetGroupConfigForEvent(t *testing.T) {
	remediationActions := map[string]config.MaintenanceResource{
		"RESTART_BM": {
			ApiGroup:                     "janitor.dgxc.nvidia.com",
			Version:                      "v1alpha1",
			Kind:                         "RebootNode",
			TemplateFileName:             "rebootnode-template.yaml",
			CompleteConditionType:        "NodeReady",
			EquivalenceGroup:             "restart",
			ImpactedEntityScope:          "",
			SupersedingEquivalenceGroups: nil,
		},
		"COMPONENT_RESET": {
			ApiGroup:                     "janitor.dgxc.nvidia.com",
			Version:                      "v1alpha1",
			Kind:                         "RebootNode",
			TemplateFileName:             "rebootnode-template.yaml",
			CompleteConditionType:        "NodeReady",
			EquivalenceGroup:             "reset",
			ImpactedEntityScope:          "GPU_UUID",
			SupersedingEquivalenceGroups: []string{"restart"},
		},
	}

	tests := []struct {
		name                string
		healthEvent         *protos.HealthEvent
		expectError         bool
		expectedGroupConfig *EquivalenceGroupConfig
	}{
		{
			name: "Non-supported recommended action",
			healthEvent: &protos.HealthEvent{
				RecommendedAction: protos.RecommendedAction(1000),
			},
			expectError:         false,
			expectedGroupConfig: nil,
		},
		{
			name: "EquivalenceGroup without ImpactedEntityScope",
			healthEvent: &protos.HealthEvent{
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
			expectError: false,
			expectedGroupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup: "restart",
			},
		},
		{
			name: "EquivalenceGroup with valid ImpactedEntityScope",
			healthEvent: &protos.HealthEvent{
				RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				EntitiesImpacted: []*protos.Entity{
					{
						EntityType:  "GPU_UUID",
						EntityValue: "GPU-123",
					},
				},
			},
			expectError: false,
			expectedGroupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup:    "reset-GPU-123",
				ImpactedEntityScopeValue:     "GPU-123",
				SupersedingEquivalenceGroups: []string{"restart"},
			},
		},
		{
			name: "EquivalenceGroup where HealthEvent is missing ImpactedEntityScope",
			healthEvent: &protos.HealthEvent{
				RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
			},
			expectError:         true,
			expectedGroupConfig: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupConfig, err := GetGroupConfigForEvent(remediationActions, tt.healthEvent)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectedGroupConfig, groupConfig)
		})
	}
}

func TestFilterEquivalenceGroupStates(t *testing.T) {
	remediationState := &annotation.RemediationStateAnnotation{
		EquivalenceGroups: map[string]annotation.EquivalenceGroupState{
			"restart": {
				MaintenanceCR: "reboot-node",
				ActionName:    protos.RecommendedAction_RESTART_VM.String(),
			},
		},
	}
	remediationStateWithMultipleGroups := &annotation.RemediationStateAnnotation{
		EquivalenceGroups: map[string]annotation.EquivalenceGroupState{
			"restart": {
				MaintenanceCR: "reboot-node",
				ActionName:    protos.RecommendedAction_RESTART_VM.String(),
			},
			"GPU-123-reset": {
				MaintenanceCR: "gpu-reset",
				ActionName:    protos.RecommendedAction_COMPONENT_RESET.String(),
			},
			"GPU-456-reset": {
				MaintenanceCR: "gpu-reset",
				ActionName:    protos.RecommendedAction_COMPONENT_RESET.String(),
			},
		},
	}
	tests := []struct {
		name               string
		remediationState   *annotation.RemediationStateAnnotation
		groupConfig        *EquivalenceGroupConfig
		expectedGroupState map[string]annotation.EquivalenceGroupState
	}{
		{
			name: "Primary equivalence group exists on annotation",
			groupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup: "restart",
			},
			remediationState: remediationState,
			expectedGroupState: map[string]annotation.EquivalenceGroupState{
				"restart": remediationState.EquivalenceGroups["restart"],
			},
		},
		{
			name: "Primary equivalence group is missing on annotation",
			groupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup: "stop",
			},
			remediationState:   remediationState,
			expectedGroupState: map[string]annotation.EquivalenceGroupState{},
		},
		{
			name: "Multiple groups on annotation: primary and superseding match",
			groupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup:    "GPU-123-reset",
				SupersedingEquivalenceGroups: []string{"restart"},
			},
			remediationState: remediationStateWithMultipleGroups,
			expectedGroupState: map[string]annotation.EquivalenceGroupState{
				"restart":       remediationStateWithMultipleGroups.EquivalenceGroups["restart"],
				"GPU-123-reset": remediationStateWithMultipleGroups.EquivalenceGroups["GPU-123-reset"],
			},
		},
		{
			name: "Multiple groups on annotation: superseding matches",
			groupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup:    "GPU-789-reset",
				SupersedingEquivalenceGroups: []string{"restart"},
			},
			remediationState: remediationStateWithMultipleGroups,
			expectedGroupState: map[string]annotation.EquivalenceGroupState{
				"restart": remediationStateWithMultipleGroups.EquivalenceGroups["restart"],
			},
		},
		{
			name: "Multiple groups on annotation: primary matches",
			groupConfig: &EquivalenceGroupConfig{
				EffectiveEquivalenceGroup:    "GPU-123-reset",
				SupersedingEquivalenceGroups: []string{"stop"},
			},
			remediationState: remediationStateWithMultipleGroups,
			expectedGroupState: map[string]annotation.EquivalenceGroupState{
				"GPU-123-reset": remediationStateWithMultipleGroups.EquivalenceGroups["GPU-123-reset"],
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupState := FilterEquivalenceGroupStates(tt.groupConfig, tt.remediationState)
			assert.Equal(t, tt.expectedGroupState, groupState)
		})
	}
}

// Both production callers must select the same GPU, including malformed entity lists.
func TestGetGroupConfigForEvent_PartialDrain_SelectsSameEntity(t *testing.T) {
	gpuA := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-aaaa"}
	gpuB := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-bbbb"}
	empty := &protos.Entity{EntityType: "GPU_UUID"}
	pci := &protos.Entity{EntityType: "PCI", EntityValue: "0000:01:00.0"}
	actions := map[string]config.MaintenanceResource{
		"COMPONENT_RESET": {
			EquivalenceGroup: "reset", ImpactedEntityScope: "GPU_UUID",
			SupersedingEquivalenceGroups: []string{"restart"},
		},
	}
	tests := []struct {
		name     string
		entities []*protos.Entity
		want     string
	}{
		{"two GPUs", []*protos.Entity{gpuA, gpuB}, "GPU-aaaa"},
		{"reversed GPUs", []*protos.Entity{gpuB, gpuA}, "GPU-bbbb"},
		{"empty then valid", []*protos.Entity{empty, gpuA}, "GPU-aaaa"},
		{"valid then empty", []*protos.Entity{gpuA, empty}, "GPU-aaaa"},
		{"PCI then GPUs", []*protos.Entity{pci, gpuA, gpuB}, "GPU-aaaa"},
		{"GPU then PCI", []*protos.Entity{gpuB, pci, gpuA}, "GPU-bbbb"},
		{"nil then valid", []*protos.Entity{nil, gpuA}, "GPU-aaaa"},
		{"no non-empty GPU", []*protos.Entity{pci, empty}, ""},
		{"no scope type", []*protos.Entity{pci}, ""},
		{"no entities", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &protos.HealthEvent{
				NodeName: "node-a", RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				EntitiesImpacted: tt.entities,
			}
			entity, drainErr := drain.PartialDrainEntity(event, true)
			group, remediationErr := GetGroupConfigForEvent(actions, event)
			if tt.want == "" {
				require.Error(t, drainErr)
				require.ErrorContains(t, remediationErr, "no non-empty impacted entity of type GPU_UUID")
				require.Nil(t, entity)
				require.Nil(t, group)
				return
			}

			require.NoError(t, drainErr)
			require.NoError(t, remediationErr)
			require.Equal(t, tt.want, entity.EntityValue)
			require.Equal(t, entity.EntityValue, group.ImpactedEntityScopeValue)
			require.Equal(t, "reset-"+tt.want, group.EffectiveEquivalenceGroup)
			require.Equal(t, []string{"restart"}, group.SupersedingEquivalenceGroups)
		})
	}
}
