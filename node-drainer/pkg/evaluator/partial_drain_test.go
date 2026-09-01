// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package evaluator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
)

func healthEvent(action protos.RecommendedAction, entities ...*protos.Entity) *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName:          "node-1",
		RecommendedAction: action,
		EntitiesImpacted:  entities,
	}
}

func gpuEntity(value string) *protos.Entity {
	return &protos.Entity{EntityType: "GPU_UUID", EntityValue: value}
}

func TestPartialDrainEntity_VaryingEventShapes_ReturnsEntityAndMatchingScope(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		event      *protos.HealthEvent
		wantEntity string
		wantScope  string
	}{
		{
			name:      "partial drain disabled",
			enabled:   false,
			event:     healthEvent(protos.RecommendedAction_COMPONENT_RESET, gpuEntity("GPU-abc")),
			wantScope: DrainScopeFull,
		},
		{
			name:      "enabled but action is not COMPONENT_RESET",
			enabled:   true,
			event:     healthEvent(protos.RecommendedAction_RESTART_VM, gpuEntity("GPU-abc")),
			wantScope: DrainScopeFull,
		},
		{
			name:       "enabled with a usable GPU_UUID entity",
			enabled:    true,
			event:      healthEvent(protos.RecommendedAction_COMPONENT_RESET, gpuEntity("GPU-abc")),
			wantEntity: "GPU-abc",
			wantScope:  DrainScopePartial,
		},
		{
			name:    "unsupported entity type is not usable",
			enabled: true,
			event: healthEvent(protos.RecommendedAction_COMPONENT_RESET,
				&protos.Entity{EntityType: "NIC", EntityValue: "eth0"}),
			wantScope: DrainScopeFull,
		},
		{
			name:      "supported type with an empty value is not usable",
			enabled:   true,
			event:     healthEvent(protos.RecommendedAction_COMPONENT_RESET, gpuEntity("")),
			wantScope: DrainScopeFull,
		},
		{
			name:      "no entities at all",
			enabled:   true,
			event:     healthEvent(protos.RecommendedAction_COMPONENT_RESET),
			wantScope: DrainScopeFull,
		},
		{
			name:    "first usable entity wins when mixed",
			enabled: true,
			event: healthEvent(protos.RecommendedAction_COMPONENT_RESET,
				&protos.Entity{EntityType: "NIC", EntityValue: "eth0"},
				gpuEntity("GPU-def")),
			wantEntity: "GPU-def",
			wantScope:  DrainScopePartial,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entity := PartialDrainEntity(tc.event, tc.enabled)

			if tc.wantEntity == "" {
				assert.Nil(t, entity)
			} else {
				require.NotNil(t, entity)
				assert.Equal(t, tc.wantEntity, entity.GetEntityValue())
			}

			assert.Equal(t, tc.wantScope, DrainScope(tc.event, tc.enabled))
		})
	}
}

// The evaluator still treats "eligible but no usable entity" as an error, so a misconfigured
// COMPONENT_RESET event fails loudly rather than silently draining the whole node.
func TestShouldExecutePartialDrain_NoUsableEntity_ReturnsError(t *testing.T) {
	e := &NodeDrainEvaluator{config: config.TomlConfig{PartialDrainEnabled: true}}

	entity, err := e.shouldExecutePartialDrain(
		healthEvent(protos.RecommendedAction_COMPONENT_RESET,
			&protos.Entity{EntityType: "NIC", EntityValue: "eth0"}))

	require.Error(t, err)
	assert.Nil(t, entity)
}

func TestShouldExecutePartialDrain_UsableGPUEntity_ReturnsEntity(t *testing.T) {
	e := &NodeDrainEvaluator{config: config.TomlConfig{PartialDrainEnabled: true}}

	entity, err := e.shouldExecutePartialDrain(
		healthEvent(protos.RecommendedAction_COMPONENT_RESET, gpuEntity("GPU-abc")))

	require.NoError(t, err)
	require.NotNil(t, entity)
	assert.Equal(t, "GPU-abc", entity.GetEntityValue())
}

// A non-candidate event returns no entity and no error, so full drain proceeds normally.
func TestShouldExecutePartialDrain_PartialDrainDisabled_ReturnsNilWithoutError(t *testing.T) {
	e := &NodeDrainEvaluator{config: config.TomlConfig{PartialDrainEnabled: false}}

	entity, err := e.shouldExecutePartialDrain(
		healthEvent(protos.RecommendedAction_COMPONENT_RESET, gpuEntity("GPU-abc")))

	require.NoError(t, err)
	assert.Nil(t, entity)
}
