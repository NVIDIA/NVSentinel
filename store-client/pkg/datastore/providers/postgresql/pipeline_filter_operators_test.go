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

package postgresql

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestMatchesExists_FieldPresence_MatchesSQLFilter(t *testing.T) {
	tests := []struct {
		name        string
		actualValue any
		opValue     any
		expected    bool
	}{
		{name: "missing field, exists false", actualValue: nil, opValue: false, expected: true},
		{name: "missing field, exists true", actualValue: nil, opValue: true, expected: false},
		{name: "present field, exists false", actualValue: float64(1), opValue: false, expected: false},
		{name: "present field, exists true", actualValue: float64(1), opValue: true, expected: true},
		{name: "zero number counts as present", actualValue: float64(0), opValue: true, expected: true},
		{name: "false bool counts as present", actualValue: false, opValue: true, expected: true},
		{name: "empty string counts as present", actualValue: "", opValue: false, expected: false},
		{name: "non-boolean operand never matches", actualValue: nil, opValue: "false", expected: false},
		{name: "nil operand never matches", actualValue: float64(1), opValue: nil, expected: false},
	}

	f := &PipelineFilter{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, f.matchesExists(tt.actualValue, tt.opValue))
		})
	}
}

func TestMatchesOperators_NumericComparisons_CompareAsFloat(t *testing.T) {
	tests := []struct {
		name        string
		actualValue any
		operators   map[string]any
		expected    bool
	}{
		{name: "$gt above", actualValue: float64(5), operators: map[string]any{"$gt": 4}, expected: true},
		{name: "$gt equal", actualValue: float64(5), operators: map[string]any{"$gt": 5}, expected: false},
		{name: "$gte equal", actualValue: float64(5), operators: map[string]any{"$gte": int64(5)}, expected: true},
		{name: "$gte below", actualValue: float64(4), operators: map[string]any{"$gte": 5}, expected: false},
		{name: "$lt below", actualValue: int32(4), operators: map[string]any{"$lt": 5}, expected: true},
		{name: "$lt equal", actualValue: float64(5), operators: map[string]any{"$lt": 5}, expected: false},
		{name: "$lte equal", actualValue: float64(5), operators: map[string]any{"$lte": float32(5)}, expected: true},
		{name: "$lte above", actualValue: float64(6), operators: map[string]any{"$lte": 5}, expected: false},
		{name: "missing field never compares", actualValue: nil, operators: map[string]any{"$gte": 0}, expected: false},
		{name: "string never compares", actualValue: "5", operators: map[string]any{"$lte": 5}, expected: false},
	}

	f := &PipelineFilter{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, f.matchesOperators(tt.actualValue, tt.operators))
		})
	}
}

func TestMatchesEvent_BuilderPipelinesWithExists_FilterByProcessingStrategy(t *testing.T) {
	builder := client.NewPostgreSQLPipelineBuilder()

	tests := []struct {
		name     string
		pipeline datastore.Pipeline
		op       string
		event    map[string]any
		expected bool
	}{
		{
			name:     "fault-quarantine: missing strategy is processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(nil),
			expected: true,
		},
		{
			name:     "fault-quarantine: null strategy is processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(map[string]any{"processingStrategy": nil}),
			expected: true,
		},
		{
			name:     "fault-quarantine: EXECUTE_REMEDIATION is processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(strategy(protos.ProcessingStrategy_EXECUTE_REMEDIATION)),
			expected: true,
		},
		{
			name:     "fault-quarantine: STORE_ONLY is not processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(strategy(protos.ProcessingStrategy_STORE_ONLY)),
			expected: false,
		},
		{
			name:     "fault-quarantine: STORE_AND_ANALYSE is not processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(strategy(protos.ProcessingStrategy_STORE_AND_ANALYSE)),
			expected: false,
		},
		{
			name:     "fault-quarantine: update with missing strategy is not processed",
			pipeline: builder.BuildProcessableHealthEventInsertsPipeline(),
			op:       "update",
			event:    healthEventDoc(nil),
			expected: false,
		},
		{
			name:     "health-events-analyzer: missing strategy is processed",
			pipeline: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(nil),
			expected: true,
		},
		{
			name:     "health-events-analyzer: STORE_AND_ANALYSE is processed",
			pipeline: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
			op:       "update",
			event:    healthEventDoc(strategy(protos.ProcessingStrategy_STORE_AND_ANALYSE)),
			expected: true,
		},
		{
			name:     "health-events-analyzer: STORE_ONLY is not processed",
			pipeline: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(strategy(protos.ProcessingStrategy_STORE_ONLY)),
			expected: false,
		},
		{
			name:     "health-events-analyzer: healthy event with missing strategy is not processed",
			pipeline: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(map[string]any{"isHealthy": true}),
			expected: false,
		},
		{
			name:     "health-events-analyzer: own event with missing strategy is not processed",
			pipeline: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
			op:       "insert",
			event:    healthEventDoc(map[string]any{"agent": "health-events-analyzer"}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := NewPipelineFilter(tt.pipeline)
			require.NoError(t, err)
			require.NotNil(t, filter)

			event := datastore.EventWithToken{
				Event: map[string]any{
					"operationType": tt.op,
					"fullDocument":  map[string]any{"healthevent": tt.event},
				},
				ResumeToken: []byte("1"),
			}

			assert.Equal(t, tt.expected, filter.MatchesEvent(event))
		})
	}
}

func TestMatchesEvent_StoreOnlyEvent_DoesNotLogUnsupportedOperator(t *testing.T) {
	logs := &syncBuffer{}

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	filter, err := NewPipelineFilter(client.NewPostgreSQLPipelineBuilder().BuildProcessableHealthEventInsertsPipeline())
	require.NoError(t, err)

	event := datastore.EventWithToken{
		Event: map[string]any{
			"operationType": "insert",
			"fullDocument": map[string]any{
				"healthevent": healthEventDoc(strategy(protos.ProcessingStrategy_STORE_ONLY)),
			},
		},
		ResumeToken: []byte("1"),
	}

	assert.False(t, filter.MatchesEvent(event))
	assert.NotContains(t, logs.String(), "Unsupported operator")
}

// syncBuffer is a bytes.Buffer that is safe to read while another goroutine logs to the global slog default.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// strategy returns the processingStrategy field as json.Unmarshal decodes it from a changelog row.
func strategy(s protos.ProcessingStrategy) map[string]any {
	return map[string]any{"processingStrategy": float64(s)}
}

// healthEventDoc returns an unhealthy, non-fatal health event with the overrides applied.
// The document has no processingStrategy unless an override sets it.
func healthEventDoc(overrides map[string]any) map[string]any {
	doc := map[string]any{
		"agent":     "gpu-health-monitor",
		"checkName": "GpuXidError",
		"nodeName":  "node-1",
		"isFatal":   false,
		"isHealthy": false,
	}

	for k, v := range overrides {
		doc[k] = v
	}

	return doc
}
