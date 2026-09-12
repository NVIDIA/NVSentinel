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
	"testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestAnalyzerPipelineAdmitsRecoveryEvents(t *testing.T) {
	filter, err := NewPipelineFilter(
		client.WithExtendedFilters(
			client.NewPostgreSQLPipelineBuilder().BuildAnalyzerHealthEventInsertsPipeline(),
		),
	)
	if err != nil {
		t.Fatalf("NewPipelineFilter() error = %v", err)
	}

	tests := []struct {
		name      string
		operation string
		agent     string
		strategy  any
		isHealthy bool
		want      bool
	}{
		{name: "healthy insert", operation: "insert", agent: "syslog-health-monitor",
			strategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION), isHealthy: true, want: true},
		{name: "unhealthy insert", operation: "insert", agent: "syslog-health-monitor",
			strategy: int32(protos.ProcessingStrategy_STORE_AND_ANALYSE), want: true},
		{name: "healthy update", operation: "update", agent: "syslog-health-monitor",
			strategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION), isHealthy: true, want: true},
		{name: "legacy event without strategy", operation: "insert", agent: "custom-monitor",
			strategy: nil, isHealthy: true, want: true},
		{name: "explicit unspecified strategy", operation: "insert", agent: "custom-monitor",
			strategy: int32(protos.ProcessingStrategy_UNSPECIFIED), isHealthy: true, want: true},
		{name: "legacy event without agent", operation: "insert",
			strategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION), isHealthy: true, want: true},
		{name: "analyzer output", operation: "insert", agent: "health-events-analyzer",
			strategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION), isHealthy: true, want: false},
		{name: "store only", operation: "insert", agent: "syslog-health-monitor",
			strategy: int32(protos.ProcessingStrategy_STORE_ONLY), isHealthy: true, want: false},
		{name: "delete", operation: "delete", agent: "syslog-health-monitor",
			strategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION), isHealthy: true, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			healthEvent := map[string]any{
				"ishealthy": test.isHealthy,
			}
			if test.agent != "" {
				healthEvent["agent"] = test.agent
			}
			if test.strategy != nil {
				healthEvent["processingstrategy"] = test.strategy
			}

			event := datastore.EventWithToken{Event: datastore.Event{
				"operationType": test.operation,
				"fullDocument": map[string]any{
					"healthevent": healthEvent,
				},
			}}

			if got := filter.MatchesEvent(event); got != test.want {
				t.Fatalf("MatchesEvent() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestExtendedFiltersDoNotChangeFaultQuarantineAdmission(t *testing.T) {
	pipeline := client.NewPostgreSQLPipelineBuilder().BuildProcessableHealthEventInsertsPipeline()
	legacyFilter, err := NewPipelineFilter(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	extendedFilter, err := NewPipelineFilter(client.WithExtendedFilters(pipeline))
	if err != nil {
		t.Fatal(err)
	}

	event := datastore.EventWithToken{Event: datastore.Event{
		"operationType": "insert",
		"fullDocument": map[string]any{
			"healthevent": map[string]any{},
		},
	}}
	if legacyFilter.MatchesEvent(event) {
		t.Fatal("unscoped pipeline unexpectedly changed fault-quarantine admission")
	}
	if !extendedFilter.MatchesEvent(event) {
		t.Fatal("extended pipeline did not admit a legacy event")
	}
}
