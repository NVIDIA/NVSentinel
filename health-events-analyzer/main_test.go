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

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

func TestCreatePipelineRequiresEnabledRecovery(t *testing.T) {
	builder := client.GetPipelineBuilder()

	for name, test := range map[string]struct {
		config *config.TomlConfig
		want   any
	}{
		"nil config": {
			want: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
		},
		"manual recovery": {
			config: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{{EvaluateRule: true}}},
			want:   builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
		},
		"disabled recovery rule": {
			config: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{{
				Recovery: &config.RecoveryMapping{},
			}}},
			want: builder.BuildProcessableNonFatalUnhealthyInsertsPipeline(),
		},
		"enabled recovery rule": {
			config: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{{
				EvaluateRule: true,
				Recovery:     &config.RecoveryMapping{},
			}}},
			want: builder.BuildAnalyzerHealthEventInsertsPipeline(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, test.want, createPipeline(test.config))
		})
	}
}
