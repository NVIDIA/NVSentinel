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

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetPipelineStages_AlwaysIncludesAgentFilter verifies that the agent filter
// is ALWAYS the first stage in the pipeline to prevent the health-events-analyzer
// from matching its own generated events, which would cause infinite loops and
// incorrect rule evaluations.
//
// This is a regression test for the bug introduced in commit 7be1a34 where the
// agent filter was accidentally removed during refactoring.
func TestGetPipelineStages_AlwaysIncludesAgentFilter(t *testing.T) {
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{},
	}

	testCases := []struct {
		name        string
		rule        config.HealthEventsAnalyzerRule
		event       datamodels.HealthEventWithStatus
		description string
	}{
		{
			name: "rule with multiple stages",
			rule: config.HealthEventsAnalyzerRule{
				Name: "multi-stage-rule",
				Stage: []string{
					`{"$match": {"healthevent.nodename": "this.healthevent.nodename"}}`,
					`{"$count": "total"}`,
					`{"$match": {"total": {"$gte": 5}}}`,
				},
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
					Agent:    "gpu-health-monitor",
				},
			},
			description: "Multi-stage rule should have agent filter as first stage",
		},
		{
			name: "rule with single stage",
			rule: config.HealthEventsAnalyzerRule{
				Name: "single-stage-rule",
				Stage: []string{
					`{"$match": {"healthevent.isfatal": true}}`,
				},
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
					Agent:    "syslog-monitor",
				},
			},
			description: "Single-stage rule should have agent filter as first stage",
		},
		{
			name: "rule with empty stages",
			rule: config.HealthEventsAnalyzerRule{
				Name:  "empty-rule",
				Stage: []string{},
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
					Agent:    "gpu-health-monitor",
				},
			},
			description: "Even with empty stages, agent filter should be present",
		},
		{
			name: "rule with complex MongoDB operators",
			rule: config.HealthEventsAnalyzerRule{
				Name: "complex-operator-rule",
				Stage: []string{
					`{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": [{"$divide": [{"$toLong": "$$NOW"}, 1000]}, 180]}]}}}`,
					`{"$match": {"healthevent.nodename": "this.healthevent.nodename"}}`,
				},
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "operator-node",
					Agent:    "platform-connector",
				},
			},
			description: "Complex operator rule should have agent filter as first stage",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline, err := reconciler.getPipelineStages(tc.rule, tc.event)
			require.NoError(t, err, "getPipelineStages should not return an error")

			// Verify pipeline has at least the agent filter stage
			expectedMinLength := 1 + len(tc.rule.Stage)
			assert.GreaterOrEqual(t, len(pipeline), 1, tc.description)

			// CRITICAL CHECK: First stage must be the agent filter
			firstStage := pipeline[0]

			matchStage, ok := firstStage["$match"].(map[string]interface{})
			require.True(t, ok, "First stage should be a $match stage")

			// Verify the agent filter exists and excludes "health-events-analyzer"
			agentFilter, ok := matchStage["healthevent.agent"].(map[string]interface{})
			require.True(t, ok, "Agent filter must be present in first $match stage")

			excludeValue, ok := agentFilter["$ne"].(string)
			require.True(t, ok, "Agent filter must use $ne operator")
			assert.Equal(t, "health-events-analyzer", excludeValue,
				"Agent filter must exclude 'health-events-analyzer' to prevent infinite loops")

			// Verify the configured stages come after the agent filter
			if len(tc.rule.Stage) > 0 {
				assert.Equal(t, expectedMinLength, len(pipeline),
					"Pipeline should have agent filter + configured stages")
			}
		})
	}
}

// TestGetPipelineStages_AgentFilterPreventsInfiniteLoop tests that events
// generated by health-events-analyzer itself would be excluded by the filter.
func TestGetPipelineStages_AgentFilterPreventsInfiniteLoop(t *testing.T) {
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{},
	}

	rule := config.HealthEventsAnalyzerRule{
		Name: "test-rule",
		Stage: []string{
			`{"$match": {"healthevent.nodename": "this.healthevent.nodename"}}`,
		},
	}

	// Create an event that was generated BY health-events-analyzer
	// This simulates the case where the analyzer publishes an event that
	// would match its own rules if the agent filter is not present
	eventFromAnalyzer := datamodels.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName: "test-node",
			Agent:    "health-events-analyzer", // This is the critical field
			CheckName: "RepeatedXidError",
			IsFatal:   true,
		},
	}

	pipeline, err := reconciler.getPipelineStages(rule, eventFromAnalyzer)
	require.NoError(t, err)

	// Extract the agent filter from the first stage
	firstStage := pipeline[0]
	matchStage := firstStage["$match"].(map[string]interface{})
	agentFilter := matchStage["healthevent.agent"].(map[string]interface{})
	excludeValue := agentFilter["$ne"].(string)

	// CRITICAL: Verify that the event's agent matches what we're excluding
	assert.Equal(t, eventFromAnalyzer.HealthEvent.Agent, excludeValue,
		"The agent filter must exclude events from health-events-analyzer itself")

	// In a real MongoDB aggregation pipeline, this $match stage would filter out
	// any documents where healthevent.agent == "health-events-analyzer"
	// This prevents the analyzer from processing its own generated events
}

// TestGetPipelineStages_AgentFilterPosition verifies that the agent filter
// is always the FIRST stage, before any user-configured stages.
func TestGetPipelineStages_AgentFilterPosition(t *testing.T) {
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{},
	}

	rule := config.HealthEventsAnalyzerRule{
		Name: "positioned-rule",
		Stage: []string{
			`{"$match": {"healthevent.isfatal": true}}`,
			`{"$match": {"healthevent.nodename": "specific-node"}}`,
			`{"$count": "total"}`,
		},
	}

	event := datamodels.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName: "test-node",
			Agent:    "gpu-health-monitor",
		},
	}

	pipeline, err := reconciler.getPipelineStages(rule, event)
	require.NoError(t, err)

	// Pipeline should have: 1 agent filter + 3 configured stages = 4 total
	assert.Equal(t, 4, len(pipeline), "Pipeline should have agent filter + 3 configured stages")

	// Verify first stage is agent filter
	firstStage := pipeline[0]
	matchStage := firstStage["$match"].(map[string]interface{})
	_, hasAgentFilter := matchStage["healthevent.agent"]
	assert.True(t, hasAgentFilter, "First stage must be the agent filter")

	// Verify second stage is the first configured stage
	secondStage := pipeline[1]
	secondMatch := secondStage["$match"].(map[string]interface{})
	isFatal, hasIsFatal := secondMatch["healthevent.isfatal"]
	assert.True(t, hasIsFatal, "Second stage should be first configured stage")
	assert.Equal(t, true, isFatal, "Second stage should match isfatal: true")

	// Verify third stage is the second configured stage
	thirdStage := pipeline[2]
	thirdMatch := thirdStage["$match"].(map[string]interface{})
	nodeName, hasNodeName := thirdMatch["healthevent.nodename"]
	assert.True(t, hasNodeName, "Third stage should be second configured stage")
	assert.Equal(t, "specific-node", nodeName, "Third stage should match specific-node")

	// Verify fourth stage is the count stage
	fourthStage := pipeline[3]
	countField, hasCount := fourthStage["$count"]
	assert.True(t, hasCount, "Fourth stage should be the $count stage")
	assert.Equal(t, "total", countField, "Count field should be 'total'")
}
