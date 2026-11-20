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

package nodecache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPruningKeys_KeepsNothingUntilACallerAddsIt(t *testing.T) {
	t.Parallel()

	keys := NewPruningKeys()

	require.True(t, keys.PruneLabels())
	require.True(t, keys.PruneAnnotations())
	require.True(t, keys.PruneManagedFields())
	require.False(t, keys.HasNodeRule())
	require.Equal(t, "[]", keys.RetainedLabelNames())
	require.Nil(t, keys.RetainLabels(nil, nil))
	require.Empty(t, keys.RetainLabels(map[string]string{"scheduler": "slurm"}, nil))
}

func TestZeroKeys_DoesNotPrune(t *testing.T) {
	t.Parallel()

	var keys Keys

	keys.AddLabel("scheduler")
	keys.AddAnnotation("example.com/team")

	require.False(t, keys.PruneLabels())
	require.False(t, keys.PruneAnnotations())
	require.False(t, keys.PruneManagedFields())
	require.Equal(t, allRetained, keys.RetainedLabelNames())
	require.Equal(t, allRetained, keys.RetainedAnnotationNames())

	labels := map[string]string{"scheduler": "slurm", "other": "kept"}
	require.Equal(t, labels, keys.RetainLabels(labels, nil))
}

func TestAddNodeExpression_FieldShapes(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"scheduler":        "slurm",
		"example.com/pool": "inference",
		"large":            "metadata",
	}
	annotations := map[string]string{
		"example.com/team": "alpha",
		"unrelated":        "drop",
	}

	tests := []struct {
		name             string
		expression       string
		wantLabels       map[string]string
		wantAnnotations  map[string]string
		pruneLabels      bool
		pruneAnnotations bool
		keepAllLabels    bool
		keepAllAnns      bool
		readsStatus      bool
		incomplete       bool
		parseError       bool
	}{
		{
			name:             "literal label and its in guard",
			expression:       `"scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"`,
			wantLabels:       map[string]string{"scheduler": "slurm"},
			wantAnnotations:  map[string]string{},
			pruneLabels:      true,
			pruneAnnotations: true,
		},
		{
			name:             "literal annotation",
			expression:       `"example.com/team" in node.metadata.annotations`,
			wantLabels:       map[string]string{},
			wantAnnotations:  map[string]string{"example.com/team": "alpha"},
			pruneLabels:      true,
			pruneAnnotations: true,
		},
		{
			name:             "label prefix",
			expression:       `node.metadata.labels.exists(k, k.startsWith("example.com/"))`,
			wantLabels:       labels,
			wantAnnotations:  map[string]string{},
			pruneAnnotations: true,
			keepAllLabels:    true,
		},
		{
			name:             "size of the label map",
			expression:       `size(node.metadata.labels) > 0`,
			wantLabels:       labels,
			wantAnnotations:  map[string]string{},
			pruneAnnotations: true,
			keepAllLabels:    true,
		},
		{
			name:             "computed label key",
			expression:       `node.metadata.labels[node.metadata.name] == "node-a"`,
			wantLabels:       labels,
			wantAnnotations:  map[string]string{},
			pruneAnnotations: true,
			keepAllLabels:    true,
		},
		{
			name:             "spec field names no map key",
			expression:       `node.spec.unschedulable`,
			wantLabels:       map[string]string{},
			wantAnnotations:  map[string]string{},
			pruneLabels:      true,
			pruneAnnotations: true,
		},
		{
			name:             "status is recorded and not cached as a key",
			expression:       `node.status.phase == "Running"`,
			wantLabels:       map[string]string{},
			wantAnnotations:  map[string]string{},
			pruneLabels:      true,
			pruneAnnotations: true,
			readsStatus:      true,
		},
		{
			name:            "the whole node",
			expression:      `size(node) > 0`,
			wantLabels:      labels,
			wantAnnotations: annotations,
			keepAllLabels:   true,
			keepAllAnns:     true,
			readsStatus:     true,
			incomplete:      true,
		},
		{
			name:            "expression that does not parse",
			expression:      `node.metadata.labels[`,
			wantLabels:      labels,
			wantAnnotations: annotations,
			keepAllLabels:   true,
			keepAllAnns:     true,
			parseError:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			keys := NewPruningKeys()
			effect := keys.AddNodeExpression(tt.expression)

			require.True(t, keys.HasNodeRule())
			require.Equal(t, tt.pruneLabels, keys.PruneLabels())
			require.Equal(t, tt.pruneAnnotations, keys.PruneAnnotations())
			require.Equal(t, tt.wantLabels, keys.RetainLabels(labels, nil))
			require.Equal(t, tt.wantAnnotations, keys.RetainAnnotations(annotations, nil))
			require.Equal(t, tt.expression, effect.Expression)
			require.Equal(t, tt.keepAllLabels, effect.KeepAllLabels)
			require.Equal(t, tt.keepAllAnns, effect.KeepAllAnnotations)
			require.Equal(t, tt.readsStatus, effect.ReadsStatus)
			require.Equal(t, tt.incomplete, effect.Incomplete)
			require.Equal(t, tt.parseError, effect.ParseError != nil)
			require.Len(t, keys.Effects(), 1)
		})
	}
}

func TestAddNodeExpression_ManagedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		expression      string
		wantPruneFields bool
	}{
		{name: "label read", expression: `"scheduler" in node.metadata.labels`, wantPruneFields: true},
		{name: "plain metadata field", expression: `node.metadata.generation == 3`, wantPruneFields: true},
		{name: "managedFields read", expression: `size(node.metadata.managedFields) > 0`},
		{name: "whole metadata", expression: `has(node.metadata)`},
		{name: "whole node", expression: `size(node) > 0`},
		{name: "does not parse", expression: `node.metadata.labels[`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			keys := NewPruningKeys()
			keys.AddNodeExpression(tt.expression)

			require.Equal(t, tt.wantPruneFields, keys.PruneManagedFields())
		})
	}
}

func TestAddNodeExpression_OneWideRule_KeepsEveryLabel(t *testing.T) {
	t.Parallel()

	keys := NewPruningKeys()
	keys.AddNodeExpression(`"scheduler" in node.metadata.labels`)
	keys.AddNodeExpression(`size(node.metadata.labels) > 0`)

	require.False(t, keys.PruneLabels())
	require.Equal(t, allRetained, keys.RetainedLabelNames())

	labels := map[string]string{"kubernetes.io/arch": "amd64"}
	require.Equal(t, labels, keys.RetainLabels(labels, nil))
}

func TestAddLabel_KeepsOnlyTheNamedKey(t *testing.T) {
	t.Parallel()

	keys := NewPruningKeys()
	keys.AddLabel("scheduler")
	keys.AddLabel("")

	require.Equal(t, map[string]string{"scheduler": "slurm"}, keys.RetainLabels(map[string]string{
		"scheduler": "slurm",
		"other":     "drop",
	}, nil))
	require.Equal(t, "[scheduler]", keys.RetainedLabelNames())
}
