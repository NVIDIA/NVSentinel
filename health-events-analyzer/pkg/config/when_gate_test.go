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

package config_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/parser"
)

// TestShippedRulesWhenMirrorsFirstStageGate checks the when expression of every shipped rule
// against the gate that the rule's stage [1] applies to the incoming event.
//
// The analyzer skips a rule's query when its when expression is false. If the expression were
// false for an event that the gate lets through, the analyzer would skip a query that can match
// and silently miss the pattern. If it were true where the gate is false, it would only waste a
// query. The test requires exact agreement over a grid of events, so it also fails when a rule's
// gate changes and its when expression is not updated with it.
func TestShippedRulesWhenMirrorsFirstStageGate(t *testing.T) {
	repoRoot := findRepoRoot(t)
	events := gateTestEvents()

	for _, source := range configSources(repoRoot) {
		t.Run(source.name, func(t *testing.T) {
			cfg := loadRulesFromValuesYAML(t, source.path)
			require.NotEmpty(t, cfg.Rules, "expected at least one rule in %s", source.name)

			gated := 0

			for _, rule := range cfg.Rules {
				if hasIncomingEventGate(t, rule) {
					gated++
				}
			}

			require.NotZero(t, gated, "expected rules with an incoming-event gate in %s", source.name)

			for _, rule := range cfg.Rules {
				t.Run(rule.Name, func(t *testing.T) {
					if !hasIncomingEventGate(t, rule) {
						assert.Empty(t, rule.When, "rule %q has no gate on the incoming event in stage [1], "+
							"so its when expression cannot be checked against one", rule.Name)

						return
					}

					require.NotEmpty(t, rule.When, "rule %q gates on the incoming event in stage [1]; "+
						"declare the same condition as its when expression so the query is skipped", rule.Name)

					for _, event := range events {
						want := evaluateGate(t, rule, event)

						got, err := rule.Applies(event.HealthEvent)
						require.NoError(t, err, "when expression of rule %q failed for %s", rule.Name, describe(event))
						require.Equal(t, want, got, "rule %q: stage [1] gate is %v but when is %v for %s",
							rule.Name, want, got, describe(event))
					}
				})
			}
		})
	}
}

// hasIncomingEventGate reports whether stage [1] of rule is a $match on an $expr that reads the
// incoming event ("this." values) and nothing from the stored documents. Such a stage passes
// every document or none, and stages run in sequence, so when it is false the rule cannot match.
func hasIncomingEventGate(t *testing.T, rule config.HealthEventsAnalyzerRule) bool {
	t.Helper()

	if len(rule.Stage) < 2 {
		return false
	}

	var stage map[string]map[string]any
	if err := json.Unmarshal([]byte(rule.Stage[1]), &stage); err != nil {
		return false
	}

	match, ok := stage["$match"]
	if !ok || len(match) != 1 {
		return false
	}

	expr, ok := match["$expr"]
	if !ok {
		return false
	}

	readsEvent, readsDocuments := false, false

	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			readsEvent = readsEvent || strings.HasPrefix(x, "this.")
			readsDocuments = readsDocuments || strings.HasPrefix(x, "$")
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(expr)

	return readsEvent && !readsDocuments
}

// evaluateGate resolves the "this." values in stage [1] with the analyzer's own parser, and
// evaluates the resulting constant $expr as MongoDB would.
func evaluateGate(t *testing.T, rule config.HealthEventsAnalyzerRule,
	event datamodels.HealthEventWithStatus) bool {
	t.Helper()

	parsed, err := parser.ParseSequenceStage(rule.Stage[1], event)
	require.NoError(t, err, "failed to parse stage [1] of rule %q", rule.Name)

	match, ok := parsed["$match"].(map[string]any)
	require.True(t, ok, "stage [1] of rule %q is not a $match", rule.Name)

	return mongoTruthy(evaluateConstant(t, match["$expr"]))
}

// evaluateConstant evaluates an aggregation expression whose operands are all constants. It
// covers the operators the shipped gates use and fails the test on any other, so a gate
// written with a new operator cannot pass unchecked.
func evaluateConstant(t *testing.T, expr any) any {
	t.Helper()

	operator, ok := expr.(map[string]any)
	if !ok {
		return expr
	}

	require.Len(t, operator, 1, "expected a single operator, got %v", operator)

	for name, raw := range operator {
		args, ok := raw.([]any)
		require.True(t, ok, "operator %s needs an argument array, got %v", name, raw)

		switch name {
		case "$and":
			for _, arg := range args {
				if !mongoTruthy(evaluateConstant(t, arg)) {
					return false
				}
			}

			return true
		case "$or":
			for _, arg := range args {
				if mongoTruthy(evaluateConstant(t, arg)) {
					return true
				}
			}

			return false
		case "$not":
			require.Len(t, args, 1)

			return !mongoTruthy(evaluateConstant(t, args[0]))
		case "$eq", "$ne":
			require.Len(t, args, 2)

			equal := reflect.DeepEqual(evaluateConstant(t, args[0]), evaluateConstant(t, args[1]))

			return equal == (name == "$eq")
		case "$in":
			require.Len(t, args, 2)

			needle := evaluateConstant(t, args[0])
			haystack, ok := evaluateConstant(t, args[1]).([]any)
			require.True(t, ok, "$in needs an array, got %v", args[1])

			for _, item := range haystack {
				if reflect.DeepEqual(needle, item) {
					return true
				}
			}

			return false
		default:
			t.Fatalf("stage [1] gate uses %s, which this test does not evaluate; add it to evaluateConstant", name)
		}
	}

	return nil
}

// mongoTruthy applies MongoDB's truthiness: null, false and zero are false.
func mongoTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	default:
		return true
	}
}

// gateTestEvents builds every combination of the incoming-event fields that the shipped gates
// read, including values each gate accepts, values it rejects, and an empty error code.
func gateTestEvents() []datamodels.HealthEventWithStatus {
	agents := []string{"syslog-health-monitor", "nic-health-monitor", "gpu-health-monitor", "other-agent"}
	checkNames := []string{
		"SysLogsXIDError", "SysLogsNICDriverError", "InfiniBandDegradationCheck",
		"EthernetDegradationCheck", "OtherCheck",
	}
	errorCodes := [][]string{
		nil, {}, {"74"}, {"13"}, {"31"}, {"45"}, {"48"}, {"13", "31"}, {"31", "74"}, {"74", "13"},
		{"netdev_watchdog"}, {"mlx5_rx_timeout_detected"}, {"unknown"},
	}
	flags := []bool{false, true}

	var events []datamodels.HealthEventWithStatus

	for _, agent := range agents {
		for _, checkName := range checkNames {
			for _, errorCode := range errorCodes {
				for _, isFatal := range flags {
					for _, isHealthy := range flags {
						events = append(events, datamodels.HealthEventWithStatus{
							HealthEvent: &protos.HealthEvent{
								Agent:     agent,
								CheckName: checkName,
								ErrorCode: errorCode,
								IsFatal:   isFatal,
								IsHealthy: isHealthy,
								NodeName:  "node-a",
							},
						})
					}
				}
			}
		}
	}

	return events
}

func describe(event datamodels.HealthEventWithStatus) string {
	e := event.HealthEvent

	return fmt.Sprintf("agent=%q checkName=%q errorCode=%q isFatal=%v isHealthy=%v",
		e.GetAgent(), e.GetCheckName(), e.GetErrorCode(), e.GetIsFatal(), e.GetIsHealthy())
}
