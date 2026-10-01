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

package reconciler

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

const whenMarkerField = "whenmarker"

// whenFakeDB answers each rule's query with that rule's scripted outcome, and records which
// rules were queried, in order. It finds the rule through a marker stage.
type whenFakeDB struct {
	mockDatabaseClient

	outcomes map[string]bool // rule name -> the query matches
	queried  []string
}

// Aggregate records the rule named by the marker stage and answers with its scripted outcome.
func (f *whenFakeDB) Aggregate(_ context.Context, pipeline any) (client.Cursor, error) {
	name := ""

	stages, _ := pipeline.([]map[string]any)
	for _, stage := range stages {
		if match, ok := stage["$match"].(map[string]any); ok {
			if marker, ok := match[whenMarkerField].(string); ok {
				name = marker
			}
		}
	}

	f.queried = append(f.queried, name)

	if f.outcomes[name] {
		return createMockCursor([]map[string]any{{"ruleMatched": true}})
	}

	return createMockCursor(nil)
}

// whenRecordingConnector records the check name of every published event, in publish order.
type whenRecordingConnector struct {
	published []string
}

// HealthEventOccurredV1 records the check name of each event in the batch.
func (c *whenRecordingConnector) HealthEventOccurredV1(_ context.Context, in *protos.HealthEvents,
	_ ...grpc.CallOption) (*emptypb.Empty, error) {
	for _, e := range in.GetEvents() {
		c.published = append(c.published, e.GetCheckName())
	}

	return &emptypb.Empty{}, nil
}

// whenRule builds an enabled rule whose only stage is the marker that whenFakeDB reads.
func whenRule(name, when string) config.HealthEventsAnalyzerRule {
	return config.HealthEventsAnalyzerRule{
		Name:              name,
		Stage:             []string{fmt.Sprintf(`{"$match": {"%s": "%s"}}`, whenMarkerField, name)},
		RecommendedAction: "CONTACT_SUPPORT",
		EvaluateRule:      true,
		When:              when,
	}
}

// counterValue reads the current value of a counter.
func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()

	var metric dto.Metric
	require.NoError(t, counter.Write(&metric))

	return metric.GetCounter().GetValue()
}

// TestHandleEvent_When_SkipsOnlyRulesThatCannotApply checks that a rule whose when expression is
// false runs no query and publishes nothing, even though its query would match; that every other
// rule is still queried; that the query result still decides a match; and that a when expression
// that fails falls back to running the query.
func TestHandleEvent_When_SkipsOnlyRulesThatCannotApply(t *testing.T) {
	cfg := &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{
		whenRule("when-false", "size(event.errorCode) > 0 && event.errorCode[0] == '74'"),
		whenRule("when-true", "size(event.errorCode) > 0 && event.errorCode[0] == '13'"),
		whenRule("no-when", ""),
		whenRule("when-true-no-match", "size(event.errorCode) > 0 && event.errorCode[0] == '13'"),
		whenRule("when-fails", "event.metadata['no-such-key'] == 'x'"),
	}}
	require.NoError(t, cfg.Compile())

	db := &whenFakeDB{outcomes: map[string]bool{
		"when-false": true, "when-true": true, "no-when": true, "when-true-no-match": false, "when-fails": true,
	}}
	connector := &whenRecordingConnector{}

	r := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: cfg,
			Publisher:                 publisher.NewPublisher(connector, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
		},
		databaseClient: db,
	}

	skippedBefore := counterValue(t, ruleSkippedTotal.WithLabelValues("when-false"))
	notSkippedBefore := counterValue(t, ruleSkippedTotal.WithLabelValues("when-true"))
	errorsBefore := counterValue(t, ruleWhenErrorsTotal.WithLabelValues("when-fails"))

	event := healthEvent_13
	published, err := r.handleEvent(context.Background(), &event)

	require.NoError(t, err)
	assert.True(t, published)
	assert.Equal(t, []string{"when-true", "no-when", "when-true-no-match", "when-fails"}, db.queried,
		"only the rule whose when expression is false may skip its query")
	assert.Equal(t, []string{"when-true", "no-when", "when-fails"}, connector.published,
		"the query result, not the when expression, must decide a match")

	assert.Equal(t, 1.0, counterValue(t, ruleSkippedTotal.WithLabelValues("when-false"))-skippedBefore)
	assert.Equal(t, 0.0, counterValue(t, ruleSkippedTotal.WithLabelValues("when-true"))-notSkippedBefore)
	assert.Equal(t, 1.0, counterValue(t, ruleWhenErrorsTotal.WithLabelValues("when-fails"))-errorsBefore)
}
