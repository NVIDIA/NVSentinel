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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

const ruleMarkerField = "rulemarker"

// ruleFakeDB answers each rule's query according to that rule's scripted outcome. It finds
// the rule through a marker stage, counts queries in flight, and can hold every query on a
// gate so a test can observe how many run at once.
type ruleFakeDB struct {
	mockDatabaseClient

	outcomes    map[string]string // rule name -> "match", "nomatch" or "error"
	gate        chan struct{}
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	calls       atomic.Int64

	mu      sync.Mutex
	queried []string
}

// Aggregate records the query, holds it on the gate when one is set, and then answers with the
// scripted outcome of the rule that the marker stage names.
func (f *ruleFakeDB) Aggregate(ctx context.Context, pipeline any) (client.Cursor, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)

	for {
		highest := f.maxInFlight.Load()
		if n <= highest || f.maxInFlight.CompareAndSwap(highest, n) {
			break
		}
	}

	f.calls.Add(1)

	name := ruleMarkerIn(pipeline)

	f.mu.Lock()
	f.queried = append(f.queried, name)
	f.mu.Unlock()

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	switch f.outcomes[name] {
	case "match":
		return createMockCursor([]map[string]any{{"ruleMatched": true}})
	case "error":
		return nil, fmt.Errorf("query for %s failed", name)
	default:
		return createMockCursor(nil)
	}
}

// ruleMarkerIn returns the rule name from the marker stage of pipeline, or "" if it has none.
func ruleMarkerIn(pipeline any) string {
	stages, _ := pipeline.([]map[string]any)
	for _, stage := range stages {
		if match, ok := stage["$match"].(map[string]any); ok {
			if name, ok := match[ruleMarkerField].(string); ok {
				return name
			}
		}
	}

	return ""
}

// recordingConnector captures the check name of every published event, in publish order.
type recordingConnector struct {
	mu        sync.Mutex
	published []string
}

// HealthEventOccurredV1 records the check name of each event in the batch.
func (c *recordingConnector) HealthEventOccurredV1(_ context.Context, in *protos.HealthEvents,
	_ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, e := range in.GetEvents() {
		c.published = append(c.published, e.GetCheckName())
	}

	return &emptypb.Empty{}, nil
}

// markerRule builds a rule whose only stage is the marker that ruleFakeDB uses to identify it.
func markerRule(name string, enabled bool) config.HealthEventsAnalyzerRule {
	return config.HealthEventsAnalyzerRule{
		Name:              name,
		Stage:             []string{fmt.Sprintf(`{"$match": {"%s": "%s"}}`, ruleMarkerField, name)},
		RecommendedAction: "CONTACT_SUPPORT",
		EvaluateRule:      enabled,
	}
}

// newConcurrencyReconciler builds a reconciler over the fakes with the given rules and rule
// concurrency.
func newConcurrencyReconciler(db *ruleFakeDB, connector *recordingConnector,
	rules []config.HealthEventsAnalyzerRule, ruleConcurrency int) *Reconciler {
	return &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
			Publisher:                 publisher.NewPublisher(connector, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
			RuleConcurrency:           ruleConcurrency,
		},
		databaseClient: db,
	}
}

// TestHandleEvent_ConcurrentRules_PublishSameEventsInRuleOrder checks that concurrent evaluation
// is observably identical to evaluating the rules one at a time: the same events published, in
// the same order, with the same errors reported.
func TestHandleEvent_ConcurrentRules_PublishSameEventsInRuleOrder(t *testing.T) {
	rules := []config.HealthEventsAnalyzerRule{
		markerRule("rule-a", true),
		markerRule("rule-b", true),
		markerRule("rule-c", true),
		markerRule("rule-d", true),
		markerRule("rule-e", false), // disabled: must never be queried or published
		markerRule("rule-f", true),
	}
	outcomes := map[string]string{
		"rule-a": "match", "rule-b": "nomatch", "rule-c": "error",
		"rule-d": "match", "rule-e": "match", "rule-f": "match",
	}

	tests := []struct {
		name            string
		ruleConcurrency int
	}{
		{name: "sequential (default)", ruleConcurrency: 1},
		{name: "two at a time", ruleConcurrency: 2},
		{name: "four at a time", ruleConcurrency: 4},
		{name: "all at once", ruleConcurrency: 22},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &ruleFakeDB{outcomes: outcomes}
			connector := &recordingConnector{}
			r := newConcurrencyReconciler(db, connector, rules, tc.ruleConcurrency)

			event := healthEvent_13
			published, err := r.handleEvent(context.Background(), &event)

			require.True(t, published)
			require.Error(t, err, "rule-c's query failure must still be reported")
			require.Contains(t, err.Error(), "query for rule-c failed")
			require.Equal(t, []string{"rule-a", "rule-d", "rule-f"}, connector.published,
				"matches must be published in rule order")
			require.EqualValues(t, 5, db.calls.Load(), "every enabled rule is queried exactly once")
			require.NotContains(t, db.queried, "rule-e", "a disabled rule must not be queried")
		})
	}
}

// TestHandleEvent_ConcurrentRules_NeverExceedConcurrencyLimit holds every query and checks that
// exactly the limit run at the same time, and never more.
func TestHandleEvent_ConcurrentRules_NeverExceedConcurrencyLimit(t *testing.T) {
	const (
		ruleCount = 8
		limit     = 3
	)

	rules := make([]config.HealthEventsAnalyzerRule, 0, ruleCount)
	for i := range ruleCount {
		rules = append(rules, markerRule(fmt.Sprintf("rule-%d", i), true))
	}

	db := &ruleFakeDB{outcomes: map[string]string{}, gate: make(chan struct{})}
	connector := &recordingConnector{}
	r := newConcurrencyReconciler(db, connector, rules, limit)

	done := make(chan struct{})

	go func() {
		defer close(done)

		event := healthEvent_13
		_, _ = r.handleEvent(context.Background(), &event)
	}()

	require.Eventually(t, func() bool { return db.inFlight.Load() == limit },
		5*time.Second, 5*time.Millisecond, "the limit should be reached while queries are held")
	require.Never(t, func() bool { return db.inFlight.Load() > limit },
		200*time.Millisecond, 5*time.Millisecond, "no more than the limit may run at once")

	close(db.gate)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleEvent did not return after the queries were released")
	}

	require.EqualValues(t, limit, db.maxInFlight.Load())
	require.EqualValues(t, ruleCount, db.calls.Load())
	require.Empty(t, connector.published)
}

// TestHandleEvent_ConcurrentRules_CancelledContextStopsWithoutPublishing cancels the context
// while queries are held, and checks that the cancellation reaches the caller and that nothing
// is published.
func TestHandleEvent_ConcurrentRules_CancelledContextStopsWithoutPublishing(t *testing.T) {
	rules := make([]config.HealthEventsAnalyzerRule, 0, 6)
	outcomes := map[string]string{}

	for i := range 6 {
		name := fmt.Sprintf("rule-%d", i)
		rules = append(rules, markerRule(name, true))
		outcomes[name] = "match"
	}

	db := &ruleFakeDB{outcomes: outcomes, gate: make(chan struct{})} // gate never opens
	connector := &recordingConnector{}
	r := newConcurrencyReconciler(db, connector, rules, 2)

	ctx, cancel := context.WithCancel(context.Background())

	var (
		published bool
		err       error
	)

	done := make(chan struct{})

	go func() {
		defer close(done)

		event := healthEvent_13
		published, err = r.handleEvent(ctx, &event)
	}()

	require.Eventually(t, func() bool { return db.inFlight.Load() == 2 },
		5*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleEvent did not return after the context was cancelled")
	}

	require.False(t, published)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "cancellation must surface to the caller: %v", err)
	require.Empty(t, connector.published, "nothing may be published once the context is cancelled")
}
