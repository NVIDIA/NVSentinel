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

package informer

import (
	"context"
	"encoding/json"
	"testing"

	v1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeAppliedLabelsAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		existing []config.AppliedLabel
		incoming []config.AppliedLabel
		expected []config.AppliedLabel
	}{
		{
			name:     "unions different keys",
			existing: []config.AppliedLabel{{Key: "fault-a", Value: "active", Priority: 10, Order: 0}},
			incoming: []config.AppliedLabel{{Key: "fault-b", Value: "active", Priority: 5, Order: 1}},
			expected: []config.AppliedLabel{
				{Key: "fault-a", Value: "active", Priority: 10, Order: 0},
				{Key: "fault-b", Value: "active", Priority: 5, Order: 1},
			},
		},
		{
			name:     "keeps higher existing priority for the same key",
			existing: []config.AppliedLabel{{Key: "gpu-fault", Value: "critical", Priority: 10, Order: 0}},
			incoming: []config.AppliedLabel{{Key: "gpu-fault", Value: "degraded", Priority: 5, Order: 1}},
			expected: []config.AppliedLabel{{Key: "gpu-fault", Value: "critical", Priority: 10, Order: 0}},
		},
		{
			name:     "accepts higher incoming priority for the same key",
			existing: []config.AppliedLabel{{Key: "gpu-fault", Value: "degraded", Priority: 5, Order: 1}},
			incoming: []config.AppliedLabel{{Key: "gpu-fault", Value: "critical", Priority: 10, Order: 0}},
			expected: []config.AppliedLabel{{Key: "gpu-fault", Value: "critical", Priority: 10, Order: 0}},
		},
		{
			name:     "uses later configuration order at equal priority",
			existing: []config.AppliedLabel{{Key: "gpu-fault", Value: "first", Priority: 10, Order: 0}},
			incoming: []config.AppliedLabel{{Key: "gpu-fault", Value: "later", Priority: 10, Order: 2}},
			expected: []config.AppliedLabel{{Key: "gpu-fault", Value: "later", Priority: 10, Order: 2}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existingJSON, err := json.Marshal(tt.existing)
			require.NoError(t, err)
			incomingJSON, err := json.Marshal(tt.incoming)
			require.NoError(t, err)

			mergedJSON, err := mergeAppliedLabelsAnnotation(string(existingJSON), string(incomingJSON))
			require.NoError(t, err)

			var merged []config.AppliedLabel
			require.NoError(t, json.Unmarshal([]byte(mergedJSON), &merged))
			assert.ElementsMatch(t, tt.expected, merged)
		})
	}
}

func TestMergeAppliedLabelsAnnotationAcceptsLegacyEntries(t *testing.T) {
	existing := `[{"key":"gpu-fault","value":"legacy"}]`
	incoming := `[{"key":"gpu-fault","value":"prioritized","priority":1,"order":0}]`

	mergedJSON, err := mergeAppliedLabelsAnnotation(existing, incoming)
	require.NoError(t, err)

	var merged []config.AppliedLabel
	require.NoError(t, json.Unmarshal([]byte(mergedJSON), &merged))
	require.Len(t, merged, 1)
	assert.Equal(t, config.AppliedLabel{
		Key: "gpu-fault", Value: "prioritized", Priority: 1, Order: 0,
	}, merged[0])
}

func TestDryRunDoesNotMutateLabels(t *testing.T) {
	ctx := context.Background()
	client := &FaultQuarantineClient{DryRunMode: true}
	node := &v1.Node{}
	node.Labels = map[string]string{
		"existing":  "keep",
		"gpu-fault": "original",
	}

	client.applyLabels(ctx, node, map[string]string{
		"gpu-fault": "critical",
		"new-label": "new-value",
	}, "node-1")
	client.removeLabels(ctx, node, []string{"existing", "gpu-fault"}, "node-1")

	assert.Equal(t, map[string]string{
		"existing":  "keep",
		"gpu-fault": "original",
	}, node.Labels)
}
