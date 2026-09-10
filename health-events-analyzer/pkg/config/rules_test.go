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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadTomlConfig_RuleMatchedEntityMetricEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "flag defaults to false when omitted",
			content: `
[[rules]]
name = "MultipleRemediations"
evaluate_rule = true
stage = ['{"$count": "count"}']
`,
			want: false,
		},
		{
			name: "flag can be enabled next to rules",
			content: `
ruleMatchedEntityMetricEnabled = true

[[rules]]
name = "RepeatedXID13OnSameGPCAndTPC"
evaluate_rule = true
stage = ['{"$match": {"healthevent.entitiesimpacted": []}}']
`,
			want: true,
		},
		{
			name: "flag can be explicitly disabled",
			content: `
ruleMatchedEntityMetricEnabled = false

[[rules]]
name = "RepeatedXIDErrorOnSameGPU"
evaluate_rule = true
stage = ['{"$count": "count"}']
`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			cfg, err := LoadTomlConfig(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.RuleMatchedEntityMetricEnabled)
			require.Len(t, cfg.Rules, 1)
		})
	}
}
