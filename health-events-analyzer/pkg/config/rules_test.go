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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/parser"
)

func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, err := os.Stat(filepath.Join(dir, ".versions.yaml")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root containing .versions.yaml")
		}

		dir = parent
	}
}

func loadRulesFromValuesYAML(t *testing.T, path string) *config.TomlConfig {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read %s", path)

	content := string(data)
	const configMarker = "config: |"
	idx := strings.Index(content, configMarker)
	require.NotEqual(t, -1, idx, "config: | not found in %s", path)

	tomlSection := content[idx+len(configMarker):]

	// If this is a multisection values file (like values-tilt.yaml), truncate at the
	// next top-level unindented key.
	var tomlLines []string
	for _, line := range strings.Split(tomlSection, "\n") {
		if len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "#") {
			break
		}

		tomlLines = append(tomlLines, line)
	}
	tomlSection = strings.Join(tomlLines, "\n")

	// Replace Helm template expressions like {{ .Values.foo }} with true so TOML parses cleanly.
	templateRegex := regexp.MustCompile(`\{\{[^}]*\}\}`)
	cleanTOML := templateRegex.ReplaceAllString(tomlSection, "true")

	tmpFile, err := os.CreateTemp("", "rules-*.toml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(cleanTOML)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	cfg, err := config.LoadTomlConfig(tmpFile.Name())
	require.NoError(t, err, "failed to load TOML config from %s", path)

	return cfg
}

func configSources(repoRoot string) []struct {
	name string
	path string
} {
	return []struct {
		name string
		path string
	}{
		{
			name: "chart values.yaml",
			path: filepath.Join(
				repoRoot, "distros", "kubernetes", "nvsentinel", "charts", "health-events-analyzer", "values.yaml",
			),
		},
		{
			name: "tilt values-tilt.yaml",
			path: filepath.Join(
				repoRoot, "distros", "kubernetes", "nvsentinel", "values-tilt.yaml",
			),
		},
	}
}

// TestRulesFirstStageBoundsGeneratedTimestamp verifies that every shipped rule in both the
// chart values.yaml and values-tilt.yaml opens with a stage bounding generatedtimestamp,
// preventing unbounded historical event scans (per Issue #1838).
func TestRulesFirstStageBoundsGeneratedTimestamp(t *testing.T) {
	repoRoot := findRepoRoot(t)

	for _, source := range configSources(repoRoot) {
		t.Run(source.name, func(t *testing.T) {
			cfg := loadRulesFromValuesYAML(t, source.path)
			require.NotEmpty(t, cfg.Rules, "expected at least one rule in %s", source.name)

			for _, rule := range cfg.Rules {
				t.Run(rule.Name, func(t *testing.T) {
					require.NotEmpty(t, rule.Stage, "rule %s must define at least one stage", rule.Name)

					firstStage := rule.Stage[0]
					assert.Contains(
						t,
						firstStage,
						"healthevent.generatedtimestamp.seconds",
						"Rule %q in %s must bound generatedtimestamp in its first stage to avoid unbounded scans",
						rule.Name,
						source.name,
					)
				})
			}
		})
	}
}

// TestXID74Reg2Bit13SetRuleStructure verifies that XID74Reg2Bit13Set has both the 24h
// time bound in stage 0 and the terminal $limit 1 stage in both values.yaml and values-tilt.yaml,
// and that all stages parse cleanly.
func TestXID74Reg2Bit13SetRuleStructure(t *testing.T) {
	repoRoot := findRepoRoot(t)

	sampleEvent := datamodels.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:  "test-gpu-node",
			Agent:     "gpu-health-monitor",
			ErrorCode: []string{"74"},
			GeneratedTimestamp: &timestamppb.Timestamp{
				Seconds: 1700000000,
			},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "REG0", EntityValue: "00000000000000000000000000000000"},
				{EntityType: "REG1", EntityValue: "00000000000000000000000000000000"},
				{EntityType: "REG2", EntityValue: "00000000000000000010000000000000"},
				{EntityType: "REG3", EntityValue: "00000000000000000000000000000000"},
				{EntityType: "REG4", EntityValue: "00000000000000000000000000000000"},
				{EntityType: "REG5", EntityValue: "00000000000000000000000000000000"},
				{EntityType: "REG6", EntityValue: "00000000000000000000000000000000"},
			},
		},
	}

	for _, source := range configSources(repoRoot) {
		t.Run(source.name, func(t *testing.T) {
			cfg := loadRulesFromValuesYAML(t, source.path)

			var targetRule *config.HealthEventsAnalyzerRule
			for i := range cfg.Rules {
				if cfg.Rules[i].Name == "XID74Reg2Bit13Set" {
					targetRule = &cfg.Rules[i]
					break
				}
			}

			require.NotNil(t, targetRule, "XID74Reg2Bit13Set rule not found in %s", source.name)

			// Must have 5 stages: time bound, errorcode 74 match, registers addFields, bit-13 match, limit 1.
			require.Len(t, targetRule.Stage, 5)

			// Stage 0: 24h time-bounding window
			assert.Contains(t, targetRule.Stage[0], "healthevent.generatedtimestamp.seconds")
			assert.Contains(t, targetRule.Stage[0], "86400")

			// Terminal stage: $limit 1
			lastStage := targetRule.Stage[len(targetRule.Stage)-1]
			assert.Contains(t, lastStage, `"$limit"`)

			// Verify all stages parse cleanly with a representative health event
			for i, stageStr := range targetRule.Stage {
				parsed, err := parser.ParseSequenceStage(stageStr, sampleEvent)
				require.NoError(t, err, "failed to parse stage %d in %s: %s", i, source.name, stageStr)
				require.NotEmpty(t, parsed, "stage %d in %s parsed to empty map", i, source.name)
			}

			// Validate parsed terminal limit stage specifically
			limitParsed, err := parser.ParseSequenceStage(lastStage, sampleEvent)
			require.NoError(t, err)
			limitVal, ok := limitParsed["$limit"]
			require.True(t, ok, "expected $limit key in parsed stage in %s", source.name)
			assert.Equal(t, float64(1), limitVal)
		})
	}
}

// TestLookupRulesGroupByGPUBeforeJoin guards the shape of the XID 74 "solo" rules.
//
// Their $lookup is a correlated self-join whose sub-pipeline reads only the node
// name, which the reconciler's mandatory first stage already pins to one node,
// and the GPU UUID. Run once per surviving document it re-reads the node's whole
// lookback window each time, so cost grows with the square of the node's event
// rate: measured at 1.2 s for a single rule on a node holding 4.5k events in its
// 24 hour window, against 15 ms for the same rule's siblings.
//
// Grouping by GPU first caps the sub-queries at the number of GPUs on the node.
// This test fails if a $lookup is ever reintroduced without that guard, or if the
// group key stops matching the GPU UUID the join correlates on.
func TestLookupRulesGroupByGPUBeforeJoin(t *testing.T) {
	repoRoot := findRepoRoot(t)

	for _, source := range configSources(repoRoot) {
		t.Run(source.name, func(t *testing.T) {
			cfg := loadRulesFromValuesYAML(t, source.path)
			require.NotEmpty(t, cfg.Rules, "expected at least one rule in %s", source.name)

			lookupRules := 0

			for _, rule := range cfg.Rules {
				lookupAt := -1

				for i, stage := range rule.Stage {
					if strings.Contains(stage, `"$lookup"`) {
						lookupAt = i
						break
					}
				}

				if lookupAt < 0 {
					continue
				}

				lookupRules++

				t.Run(rule.Name, func(t *testing.T) {
					require.GreaterOrEqual(t, lookupAt, 2,
						"rule %q must reduce to one document per GPU before its $lookup", rule.Name)

					group, replaceRoot := rule.Stage[lookupAt-2], rule.Stage[lookupAt-1]

					assert.Contains(t, group, `"$group"`,
						"rule %q must $group immediately before its $lookup so the join runs "+
							"once per GPU, not once per event", rule.Name)
					assert.Contains(t, group, "GPU_UUID",
						"rule %q must group on the GPU UUID the $lookup correlates on", rule.Name)
					assert.Contains(t, replaceRoot, `"$replaceRoot"`,
						"rule %q must restore the document shape the $lookup expects", rule.Name)

					// Parse rather than string-match, so a widened bound such as
					// {"$limit": 2} cannot satisfy the assertion. The terminal stage
					// carries no "this." reference, so an empty event resolves it.
					lastStage := rule.Stage[len(rule.Stage)-1]

					limitParsed, err := parser.ParseSequenceStage(lastStage, datamodels.HealthEventWithStatus{})
					require.NoError(t, err, "failed to parse terminal stage of rule %q", rule.Name)

					limitVal, ok := limitParsed["$limit"]
					require.True(t, ok, "rule %q must end in $limit, got %s", rule.Name, lastStage)

					assert.Equal(t, float64(1), limitVal,
						"rule %q must stop at the first match; the reconciler only tests "+
							"whether the result is non-empty", rule.Name)
				})
			}

			assert.NotZero(t, lookupRules, "expected at least one $lookup rule in %s", source.name)
		})
	}
}
