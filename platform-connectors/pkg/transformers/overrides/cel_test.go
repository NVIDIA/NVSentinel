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

package overrides

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestCompiledRuleEvaluate(t *testing.T) {
	tests := []struct {
		name        string
		expression  string
		event       *pb.HealthEvent
		expectMatch bool
	}{
		{
			name:       "simple-equality-matches",
			expression: `event.agent == "test-agent"`,
			event: &pb.HealthEvent{
				Agent: "test-agent",
			},
			expectMatch: true,
		},
		{
			name:       "simple-equality-no-match",
			expression: `event.agent == "other-agent"`,
			event: &pb.HealthEvent{
				Agent: "test-agent",
			},
			expectMatch: false,
		},
		{
			name:       "component-class-check",
			expression: `event.componentClass == "GPU"`,
			event: &pb.HealthEvent{
				ComponentClass: "GPU",
			},
			expectMatch: true,
		},
		{
			name:       "boolean-field-check-true",
			expression: `event.isFatal == true`,
			event: &pb.HealthEvent{
				IsFatal: true,
			},
			expectMatch: true,
		},
		{
			name:       "boolean-field-check-false",
			expression: `event.isFatal == false`,
			event: &pb.HealthEvent{
				IsFatal: true,
			},
			expectMatch: false,
		},
		{
			name:       "error-code-array-contains",
			expression: `"79" in event.errorCode`,
			event: &pb.HealthEvent{
				ErrorCode: []string{"79", "80"},
			},
			expectMatch: true,
		},
		{
			name:       "error-code-array-no-match",
			expression: `"100" in event.errorCode`,
			event: &pb.HealthEvent{
				ErrorCode: []string{"79", "80"},
			},
			expectMatch: false,
		},
		{
			name:       "logical-and-both-true",
			expression: `event.agent == "test" && event.isFatal == true`,
			event: &pb.HealthEvent{
				Agent:   "test",
				IsFatal: true,
			},
			expectMatch: true,
		},
		{
			name:       "logical-and-one-false",
			expression: `event.agent == "test" && event.isFatal == true`,
			event: &pb.HealthEvent{
				Agent:   "test",
				IsFatal: false,
			},
			expectMatch: false,
		},
		{
			name:       "logical-or-one-true",
			expression: `event.agent == "test" || event.isFatal == true`,
			event: &pb.HealthEvent{
				Agent:   "other",
				IsFatal: true,
			},
			expectMatch: true,
		},
		{
			name:       "logical-not",
			expression: `!(event.isFatal == true)`,
			event: &pb.HealthEvent{
				IsFatal: false,
			},
			expectMatch: true,
		},
		{
			name:       "metadata-access",
			expression: `event.metadata["gpu_id"] == "GPU-123"`,
			event: &pb.HealthEvent{
				Metadata: map[string]string{
					"gpu_id": "GPU-123",
				},
			},
			expectMatch: true,
		},
		{
			name:       "metadata-missing-key",
			expression: `has(event.metadata.gpu_id) && event.metadata["gpu_id"] == "GPU-123"`,
			event: &pb.HealthEvent{
				Metadata: map[string]string{},
			},
			expectMatch: false,
		},
		{
			name:       "complex-expression-with-parentheses",
			expression: `(event.agent == "syslog" || event.agent == "nvml") && event.isFatal == true`,
			event: &pb.HealthEvent{
				Agent:   "syslog",
				IsFatal: true,
			},
			expectMatch: true,
		},
		{
			name:       "message-not-empty",
			expression: `event.message != ""`,
			event: &pb.HealthEvent{
				Message: "GPU error detected",
			},
			expectMatch: true,
		},
		{
			name:       "message-empty",
			expression: `event.message != ""`,
			event: &pb.HealthEvent{
				Message: "",
			},
			expectMatch: false,
		},
		{
			name:       "node-name-check",
			expression: `event.nodeName == "worker-node-1"`,
			event: &pb.HealthEvent{
				NodeName: "worker-node-1",
			},
			expectMatch: true,
		},
		{
			name:       "check-name-starts-with",
			expression: `event.checkName.startsWith("GPU-")`,
			event: &pb.HealthEvent{
				CheckName: "GPU-Health-Check",
			},
			expectMatch: true,
		},
		{
			name:       "message-contains",
			expression: `event.message.contains("error")`,
			event: &pb.HealthEvent{
				Message: "GPU error detected",
			},
			expectMatch: true,
		},
		{
			name:       "multiple-metadata-checks",
			expression: `event.metadata["zone"] == "us-west1-a" && event.metadata["region"] == "us-west1"`,
			event: &pb.HealthEvent{
				Metadata: map[string]string{
					"zone":   "us-west1-a",
					"region": "us-west1",
				},
			},
			expectMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: tt.expression,
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			}

			compiled, err := compileRules(config)
			require.NoError(t, err)
			require.Len(t, compiled, 1)

			match, err := compiled[0].evaluate(tt.event)
			require.NoError(t, err)
			assert.Equal(t, tt.expectMatch, match, "match result mismatch")
		})
	}
}
