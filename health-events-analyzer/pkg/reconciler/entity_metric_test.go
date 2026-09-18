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

package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

func TestCanonicalPCIValue(t *testing.T) {
	got, ok := canonicalPCIValue("0000:19:00.0")
	assert.True(t, ok)
	assert.Equal(t, "0000:19:00", got)

	_, ok = canonicalPCIValue("00010000:01:00")
	assert.False(t, ok)
}

func TestMetricSafeEntities(t *testing.T) {
	got := metricSafeEntities(&protos.HealthEvent{
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "PCI", EntityValue: "0000:19:00.0"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-abc"},
			{EntityType: "GPC", EntityValue: "1"},
			{EntityType: "SM", EntityValue: "0"},
			{EntityType: "REG0", EntityValue: "1010"},
		},
	})

	assert.Equal(t, []*protos.Entity{
		{EntityType: "PCI", EntityValue: "0000:19:00"},
		{EntityType: "GPC", EntityValue: "1"},
		{EntityType: "SM", EntityValue: "0"},
	}, got)
}

func TestRuleSelectsOnEntity(t *testing.T) {
	assert.True(t, ruleSelectsOnEntity(config.HealthEventsAnalyzerRule{
		Stage: []string{`"$healthevent.entitiesimpacted"`},
	}))
	assert.False(t, ruleSelectsOnEntity(config.HealthEventsAnalyzerRule{
		Stage: []string{`{"healthevent.nodename": "this.healthevent.nodename"}`},
	}))
}
