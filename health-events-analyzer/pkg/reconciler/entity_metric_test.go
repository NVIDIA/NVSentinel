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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

func TestRuleSelectsOnEntity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule config.HealthEventsAnalyzerRule
		want bool
	}{
		{
			name: "node scoped rule does not select on an entity",
			rule: config.HealthEventsAnalyzerRule{
				Name: "MultipleRemediations",
				Stage: []string{
					`{"$match": {"healtheventstatus.faultremediated.value": true, "healthevent.nodename": "this.healthevent.nodename"}}`,
					`{"$count": "count"}`,
				},
			},
			want: false,
		},
		{
			name: "GPC TPC rule selects on entitiesimpacted",
			rule: config.HealthEventsAnalyzerRule{
				Name: "RepeatedXID13OnSameGPCAndTPC",
				Stage: []string{
					`{"$match": {"$expr": {"$eq": ["$$this.entitytype", "GPC"]}}}`,
					`{"input": {"$ifNull": ["$healthevent.entitiesimpacted", []]}}`,
				},
			},
			want: true,
		},
		{
			name: "GPU UUID intersection still counts as entity keyed",
			rule: config.HealthEventsAnalyzerRule{
				Name: "RepeatedXIDErrorOnSameGPU",
				Stage: []string{
					`{"$filter": {"input": "$healthevent.entitiesimpacted", "cond": {"$eq": ["$$this.entitytype", "GPU_UUID"]}}}`,
				},
			},
			want: true,
		},
		{
			name: "empty stages do not select on an entity",
			rule: config.HealthEventsAnalyzerRule{Name: "Empty"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ruleSelectsOnEntity(tt.rule))
		})
	}
}

func TestMetricSafeEntities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		event    *protos.HealthEvent
		wantType []string
		wantVal  []string
	}{
		{
			name:  "nil event",
			event: nil,
		},
		{
			name: "drops GPU UUID and SM, keeps PCI GPC TPC",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "PCI", EntityValue: "0009:01:00"},
					{EntityType: "GPU_UUID", EntityValue: "GPU-abc123"},
					{EntityType: "GPC", EntityValue: "0"},
					{EntityType: "TPC", EntityValue: "3"},
					{EntityType: "SM", EntityValue: "0"},
				},
			},
			wantType: []string{"PCI", "GPC", "TPC"},
			wantVal:  []string{"0009:01:00", "0", "3"},
		},
		{
			name: "keeps GPU index, drops UUID",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "GPU", EntityValue: "3"},
					{EntityType: "GPU_UUID", EntityValue: "GPU-abc123"},
				},
			},
			wantType: []string{"GPU"},
			wantVal:  []string{"3"},
		},
		{
			name: "drops empty type or value",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "GPC", EntityValue: ""},
					{EntityType: "", EntityValue: "1"},
					nil,
					{EntityType: "TPC", EntityValue: "2"},
				},
			},
			wantType: []string{"TPC"},
			wantVal:  []string{"2"},
		},
		{
			name: "deduplicates the same type and value",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "PCI", EntityValue: "0009:01:00"},
					{EntityType: "PCI", EntityValue: "0009:01:00"},
				},
			},
			wantType: []string{"PCI"},
			wantVal:  []string{"0009:01:00"},
		},
		{
			name: "keeps NIC and NICPort",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "NIC", EntityValue: "mlx5_0"},
					{EntityType: "NICPort", EntityValue: "1"},
				},
			},
			wantType: []string{"NIC", "NICPort"},
			wantVal:  []string{"mlx5_0", "1"},
		},
		{
			name: "drops register values",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "NVLINK", EntityValue: "2"},
					{EntityType: "REG0", EntityValue: "0x10"},
				},
			},
			wantType: []string{"NVLINK"},
			wantVal:  []string{"2"},
		},
		{
			name: "drops UUID even with different casing",
			event: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "gpu_uuid", EntityValue: "GPU-abc123"},
					{EntityType: "pci", EntityValue: "0009:01:00"},
				},
			},
			wantType: []string{"pci"},
			wantVal:  []string{"0009:01:00"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := metricSafeEntities(tt.event)
			require.Len(t, got, len(tt.wantType))

			for i := range tt.wantType {
				assert.Equal(t, tt.wantType[i], got[i].GetEntityType())
				assert.Equal(t, tt.wantVal[i], got[i].GetEntityValue())
			}
		})
	}
}

func TestRecordRuleMatchedEntity_NoopWhenDisabled(t *testing.T) {
	if ruleMatchedEntityTotal != nil {
		t.Skip("entity metric already registered by another test")
	}

	require.NotPanics(t, func() {
		recordRuleMatchedEntity("RepeatedXID13OnSameGPCAndTPC", "node-a", "GPC", "0")
		recordMatchedEntityMetric("RepeatedXID13OnSameGPCAndTPC", "node-a", &protos.HealthEvent{
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPC", EntityValue: "0"},
			},
		})
	})
}

func TestRecordRuleMatchedEntity_IncrementsWhenEnabled(t *testing.T) {
	EnableRuleMatchedEntityMetric()
	require.NotNil(t, ruleMatchedEntityTotal)

	ruleName := "RepeatedXID13OnSameGPCAndTPC"
	nodeName := "node-metric-test"
	entityType := "GPC"
	entityValue := "7"

	before := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, entityType, entityValue))
	recordRuleMatchedEntity(ruleName, nodeName, entityType, entityValue)
	after := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, entityType, entityValue))

	assert.Equal(t, before+1, after)
}

func TestRecordMatchedEntityMetric_SkipsUUIDAndNodeScopedNoise(t *testing.T) {
	EnableRuleMatchedEntityMetric()
	require.NotNil(t, ruleMatchedEntityTotal)

	ruleName := "RepeatedXID13OnSameGPCAndTPC"
	nodeName := "node-xid13-entities"

	event := &protos.HealthEvent{
		NodeName: nodeName,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "PCI", EntityValue: "0009:01:00"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-should-not-appear"},
			{EntityType: "GPC", EntityValue: "1"},
			{EntityType: "TPC", EntityValue: "3"},
			{EntityType: "SM", EntityValue: "0"},
		},
	}

	pciBefore := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "PCI", "0009:01:00"))
	gpcBefore := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "GPC", "1"))
	tpcBefore := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "TPC", "3"))

	recordMatchedEntityMetric(ruleName, nodeName, event)

	assert.Equal(t, pciBefore+1, testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "PCI", "0009:01:00")))
	assert.Equal(t, gpcBefore+1, testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "GPC", "1")))
	assert.Equal(t, tpcBefore+1, testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, "TPC", "3")))

	metrics, err := ruleMatchedEntityTotal.GetMetricWithLabelValues(ruleName, nodeName, "GPU_UUID", "GPU-should-not-appear")
	require.NoError(t, err)
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics))

	smMetric, err := ruleMatchedEntityTotal.GetMetricWithLabelValues(ruleName, nodeName, "SM", "0")
	require.NoError(t, err)
	assert.Equal(t, 0.0, testutil.ToFloat64(smMetric))
}

func TestRecordMatchedEntityMetricForRule_NodeScopedDoesNotExport(t *testing.T) {
	EnableRuleMatchedEntityMetric()
	require.NotNil(t, ruleMatchedEntityTotal)

	rule := config.HealthEventsAnalyzerRule{
		Name: "MultipleRemediations",
		Stage: []string{
			`{"$match": {"healtheventstatus.faultremediated.value": true}}`,
			`{"$count": "count"}`,
		},
	}
	nodeName := "node-remediation-scoped"
	event := &protos.HealthEvent{
		NodeName: nodeName,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "PCI", EntityValue: "0009:01:00"},
			{EntityType: "GPC", EntityValue: "0"},
			{EntityType: "TPC", EntityValue: "3"},
		},
	}

	pciBefore := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(rule.Name, nodeName, "PCI", "0009:01:00"))
	recordMatchedEntityMetricForRule(rule, event)
	pciAfter := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(rule.Name, nodeName, "PCI", "0009:01:00"))

	assert.Equal(t, pciBefore, pciAfter)
}

func TestRecordMatchedEntityMetricForRule_EntityKeyedExportsSafeLabels(t *testing.T) {
	EnableRuleMatchedEntityMetric()
	require.NotNil(t, ruleMatchedEntityTotal)

	rule := config.HealthEventsAnalyzerRule{
		Name: "RepeatedXID13OnSameGPCAndTPC",
		Stage: []string{
			`{"input": {"$ifNull": ["$healthevent.entitiesimpacted", []]}}`,
		},
	}
	nodeName := "node-entity-keyed"
	event := &protos.HealthEvent{
		NodeName: nodeName,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "PCI", EntityValue: "0009:01:00"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-hidden"},
			{EntityType: "GPC", EntityValue: "4"},
		},
	}

	gpcBefore := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(rule.Name, nodeName, "GPC", "4"))
	recordMatchedEntityMetricForRule(rule, event)
	gpcAfter := testutil.ToFloat64(ruleMatchedEntityTotal.WithLabelValues(rule.Name, nodeName, "GPC", "4"))

	assert.Equal(t, gpcBefore+1, gpcAfter)

	uuidMetric, err := ruleMatchedEntityTotal.GetMetricWithLabelValues(rule.Name, nodeName, "GPU_UUID", "GPU-hidden")
	require.NoError(t, err)
	assert.Equal(t, 0.0, testutil.ToFloat64(uuidMetric))
}
