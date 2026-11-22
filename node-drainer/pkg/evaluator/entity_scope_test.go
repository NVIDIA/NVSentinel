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
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
)

func TestEvaluateEventWithDatabase_NoValidScopedEntity_RetriesWithoutDraining(t *testing.T) {
	tests := []struct {
		name     string
		entities []*protos.Entity
	}{
		{"missing", nil},
		{"empty", []*protos.Entity{{EntityType: "GPU_UUID"}}},
		{"other type", []*protos.Entity{{EntityType: "PCI", EntityValue: "0000:01:00.0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator, err := NewNodeDrainEvaluator(config.TomlConfig{PartialDrainEnabled: true}, nil, nil)
			require.NoError(t, err)
			event := model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "node-a", RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
					EntitiesImpacted: tt.entities,
				},
				HealthEventStatus: &protos.HealthEventStatus{
					NodeQuarantined:        string(model.Quarantined),
					UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusInProgress)},
				},
			}
			for range 2 {
				// Nil collaborators ensure that no drain, datastore write, or decision recording starts.
				result, err := evaluator.EvaluateEventWithDatabase(context.Background(), event, nil, nil, nil)
				require.ErrorContains(t, err, "no supported entities for a partial drain found")
				require.Nil(t, result)
				require.Nil(t, event.HealthEventStatus.DrainDecision)
				require.Equal(t, string(model.StatusInProgress), event.HealthEventStatus.UserPodsEvictionStatus.Status)
			}
		})
	}
}

func TestEvaluateEventWithDatabase_HealthyEvent_DoesNotDrain(t *testing.T) {
	evaluator, err := NewNodeDrainEvaluator(config.TomlConfig{PartialDrainEnabled: true}, nil, nil)
	require.NoError(t, err)
	event := model.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName: "node-a", IsHealthy: true, RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
		},
		HealthEventStatus: &protos.HealthEventStatus{
			NodeQuarantined:        string(model.Quarantined),
			UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusInProgress)},
		},
	}
	result, err := evaluator.EvaluateEventWithDatabase(context.Background(), event, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, ActionMarkAlreadyDrained, result.Action)
	require.Nil(t, event.HealthEventStatus.DrainDecision)
}
