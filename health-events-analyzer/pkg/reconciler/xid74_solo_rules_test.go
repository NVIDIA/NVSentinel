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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	storeconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
)

// envTestMongoDBURI names a MongoDB replica set member for the tests that run the shipped rules
// against a real database, for example mongodb://localhost:27017/?directConnection=true. The tests
// skip when it is unset.
const envTestMongoDBURI = "NVSENTINEL_TEST_MONGODB_URI"

// ruleTestDatabase holds the HealthEvents collection the rule tests write to. Like the analyzer,
// the store-client only connects to a collection that exists, so create it once:
//
//	mongosh "$NVSENTINEL_TEST_MONGODB_URI" --eval 'db.getSiblingDB("hea_rule_tests").createCollection("HealthEvents")'
//
// Every case stores its events under a node name of its own, and the rules only read events of
// the incoming event's node, so earlier runs do not affect later ones.
const ruleTestDatabase = "hea_rule_tests"

const (
	xid74GPUA = "GPU-aaaaaaaa-0000-0000-0000-000000000000"
	xid74GPUB = "GPU-bbbbbbbb-0000-0000-0000-000000000000"
	regZero   = "00000000000000000000000000000000"
)

var xid74PCI = map[string]string{xid74GPUA: "0000:17:00", xid74GPUB: "0000:2a:00"}

// xidEventSpec describes one stored XID event of a scenario. age is how many seconds the event
// precedes the incoming event. The values-tilt.yaml copies of the rules look back 60 seconds, so
// every age stays well inside that window.
type xidEventSpec struct {
	gpu  string
	xid  string
	regs map[string]string
	age  int64
}

// TestXID74SoloRules_EventsOnTwoGPUs_DecideOnTheReportingGPU runs the four XID 74 solo rules, as
// shipped in the chart values.yaml and in values-tilt.yaml, against a real MongoDB through the
// reconciler's own query path. Each rule must decide from the registers of the incoming XID 74
// and from the other errors of the GPU that reported it. The derived event carries the incoming
// event's entities, so a rule that decides from another GPU's history names the wrong GPU.
func TestXID74SoloRules_EventsOnTwoGPUs_DecideOnTheReportingGPU(t *testing.T) {
	r := &Reconciler{databaseClient: newRuleTestMongoDBClient(t)}

	soloRules := []struct {
		name string
		regs map[string]string
	}{
		{name: "XID74Reg0SoloNVLinkError", regs: map[string]string{"REG0": "00000000000000000000000000000010"}},
		{name: "XID74Reg0SignalIntegrityError", regs: map[string]string{"REG0": "00000000001000000000000000000000"}},
		{name: "XID74Reg3UnexpectedError", regs: map[string]string{"REG3": "00000000000000010000000000000000"}},
		{name: "XID74Reg3Bit18Set", regs: map[string]string{"REG3": "00000000000001000000000000000000"}},
	}
	// Only REG1 bit 0 is set, which none of the four rules accepts.
	nonMatching := map[string]string{"REG1": "00000000000000000000000000000001"}

	for _, source := range shippedRuleSources(t) {
		t.Run(source.name, func(t *testing.T) {
			rules := loadShippedRules(t, source.path)

			for _, solo := range soloRules {
				rule, ok := rules[solo.name]
				require.True(t, ok, "rule %s not found in %s", solo.name, source.name)

				cases := []struct {
					name     string
					history  []xidEventSpec
					incoming xidEventSpec
					fires    bool
				}{
					{
						name:     "matching XID 74 fires",
						incoming: xidEventSpec{gpu: xid74GPUA, xid: "74", regs: solo.regs},
						fires:    true,
					},
					{
						name:     "non-matching XID 74 after a matching one on another GPU does not fire",
						history:  []xidEventSpec{{gpu: xid74GPUA, xid: "74", regs: solo.regs, age: 30}},
						incoming: xidEventSpec{gpu: xid74GPUB, xid: "74", regs: nonMatching},
						fires:    false,
					},
					{
						name: "matching XID 74 on a GPU with another active error does not fire",
						history: []xidEventSpec{
							{gpu: xid74GPUA, xid: "74", regs: solo.regs, age: 30},
							{gpu: xid74GPUB, xid: "48", age: 20},
						},
						incoming: xidEventSpec{gpu: xid74GPUB, xid: "74", regs: solo.regs},
						fires:    false,
					},
					{
						name:     "active error stored before the XID 74 on the same GPU blocks the rule",
						history:  []xidEventSpec{{gpu: xid74GPUB, xid: "48", age: 20}},
						incoming: xidEventSpec{gpu: xid74GPUB, xid: "74", regs: solo.regs},
						fires:    false,
					},
					{
						name:     "active error on another GPU does not block a matching XID 74",
						history:  []xidEventSpec{{gpu: xid74GPUB, xid: "48", age: 20}},
						incoming: xidEventSpec{gpu: xid74GPUA, xid: "74", regs: solo.regs},
						fires:    true,
					},
				}

				t.Run(solo.name, func(t *testing.T) {
					for _, tc := range cases {
						t.Run(tc.name, func(t *testing.T) {
							node := "xid74-" + strconv.FormatInt(time.Now().UnixNano(), 36)
							now := time.Now().Unix()

							docs := make([]any, 0, len(tc.history)+1)
							for _, spec := range tc.history {
								docs = append(docs, newXIDEvent(node, spec, now))
							}

							incoming := newXIDEvent(node, tc.incoming, now)
							docs = append(docs, incoming)

							_, err := r.databaseClient.InsertManyIdempotent(context.Background(), docs)
							require.NoError(t, err)

							fired, err := r.validateAllSequenceCriteria(context.Background(), rule, incoming)
							require.NoError(t, err)
							require.Equal(t, tc.fires, fired)
						})
					}
				})
			}
		})
	}
}

// newXIDEvent builds a stored syslog XID event with the entities the syslog health monitor
// reports: PCI and GPU_UUID, then NVLINK and REG0 to REG6 for an XID 74. Registers that spec does
// not set are zero.
func newXIDEvent(node string, spec xidEventSpec, now int64) datamodels.HealthEventWithStatus {
	entities := []*protos.Entity{
		{EntityType: "PCI", EntityValue: xid74PCI[spec.gpu]},
		{EntityType: "GPU_UUID", EntityValue: spec.gpu},
	}

	if spec.xid == "74" {
		entities = append(entities, &protos.Entity{EntityType: "NVLINK", EntityValue: "14"})

		for i := range 7 {
			register := "REG" + strconv.Itoa(i)

			value, ok := spec.regs[register]
			if !ok {
				value = regZero
			}

			entities = append(entities, &protos.Entity{EntityType: register, EntityValue: value})
		}
	}

	generated := now - spec.age

	return datamodels.HealthEventWithStatus{
		CreatedAt: time.Unix(generated, 0).UTC(),
		HealthEvent: &protos.HealthEvent{
			Version:            1,
			Agent:              "syslog-health-monitor",
			CheckName:          "SysLogsXIDError",
			ComponentClass:     "GPU",
			ErrorCode:          []string{spec.xid},
			EntitiesImpacted:   entities,
			IsFatal:            true,
			NodeName:           node,
			ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
			GeneratedTimestamp: &timestamppb.Timestamp{Seconds: generated},
		},
		HealthEventStatus: &protos.HealthEventStatus{},
	}
}

// newRuleTestMongoDBClient returns the store-client MongoDB client that the analyzer queries in
// production, connected to the HealthEvents collection of ruleTestDatabase. The shipped rules
// $lookup that collection by name.
func newRuleTestMongoDBClient(t *testing.T) client.DatabaseClient {
	t.Helper()

	uri := os.Getenv(envTestMongoDBURI)
	if uri == "" {
		t.Skipf("Skipping test that requires a MongoDB replica set - set %s to run it", envTestMongoDBURI)
	}

	t.Setenv("DATASTORE_PROVIDER", "")
	t.Setenv(storeconfig.EnvMongoDBURI, uri)
	t.Setenv(storeconfig.EnvMongoDBDatabaseName, ruleTestDatabase)
	t.Setenv(storeconfig.EnvMongoDBCollectionName, "HealthEvents")
	t.Setenv(storeconfig.EnvMongoDBPingTimeoutTotalSeconds, "30")
	t.Setenv(storeconfig.EnvMongoDBPingIntervalSeconds, "1")

	dbConfig, err := storeconfig.NewDatabaseConfigFromEnvWithDefaults("")
	require.NoError(t, err)

	dbClient, err := client.NewMongoDBClient(context.Background(), dbConfig)
	require.NoError(t, err, "create %s.HealthEvents first, as described at ruleTestDatabase", ruleTestDatabase)

	t.Cleanup(func() { require.NoError(t, dbClient.Close(context.Background())) })

	return dbClient
}

type ruleSource struct {
	name string
	path string
}

// shippedRuleSources returns the two files that carry the shipped rules, the same two that the
// config package tests read.
func shippedRuleSources(t *testing.T) []ruleSource {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, err := os.Stat(filepath.Join(dir, ".versions.yaml")); err == nil {
			break
		}

		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find the repo root containing .versions.yaml")

		dir = parent
	}

	chart := filepath.Join(dir, "distros", "kubernetes", "nvsentinel")

	return []ruleSource{
		{name: "values.yaml", path: filepath.Join(chart, "charts", "health-events-analyzer", "values.yaml")},
		{name: "values-tilt.yaml", path: filepath.Join(chart, "values-tilt.yaml")},
	}
}

// loadShippedRules reads the rules from the config block of a values file, with every Helm
// expression replaced by true so that every rule is enabled, and returns them by name.
func loadShippedRules(t *testing.T, path string) map[string]config.HealthEventsAnalyzerRule {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	const configMarker = "config: |"

	content := string(data)
	idx := strings.Index(content, configMarker)
	require.NotEqual(t, -1, idx, "config: | not found in %s", path)

	var tomlLines []string

	// values-tilt.yaml holds other sections after the config block, so stop at the next
	// unindented key.
	for _, line := range strings.Split(content[idx+len(configMarker):], "\n") {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") &&
			!strings.HasPrefix(line, "#") {
			break
		}

		tomlLines = append(tomlLines, line)
	}

	toml := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(strings.Join(tomlLines, "\n"), "true")

	tomlPath := filepath.Join(t.TempDir(), "rules.toml")
	require.NoError(t, os.WriteFile(tomlPath, []byte(toml), 0o600))

	cfg, err := config.LoadTomlConfig(tomlPath)
	require.NoError(t, err)

	rules := make(map[string]config.HealthEventsAnalyzerRule, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		rules[rule.Name] = rule
	}

	return rules
}
