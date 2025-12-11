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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestNewProcessor(t *testing.T) {
	tests := []struct {
		name      string
		config    *Config
		expectErr bool
		errMsg    string
	}{
		{
			name: "empty-rules",
			config: &Config{
				Enabled: true,
				Rules:   []Rule{},
			},
			expectErr: true,
			errMsg:    "no rules defined",
		},
		{
			name: "missing-rule-name",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						When: "event.agent == 'test'",
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "name is required",
		},
		{
			name: "missing-when-expression",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "when expression is required",
		},
		{
			name: "empty-override",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name:     "test-rule",
						When:     "event.agent == 'test'",
						Override: Override{},
					},
				},
			},
			expectErr: true,
			errMsg:    "at least one override field required",
		},
		{
			name: "invalid-cel-syntax",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "invalid-cel",
						When: "event.agent ==",
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "CEL compilation failed",
		},
		{
			name: "non-boolean-expression",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "non-boolean",
						When: "event.agent",
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "must return boolean",
		},
		{
			name: "valid-config",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test"`,
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, err := NewProcessor(tt.config)
			if tt.expectErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
				require.NotNil(t, proc)
			}
		})
	}
}

func TestApplyOverrides(t *testing.T) {
	tests := []struct {
		name              string
		config            *Config
		event             *pb.HealthEvent
		expectedIsFatal   bool
		expectedIsHealthy bool
		expectedAction    pb.RecommendedAction
	}{
		{
			name: "disabled-processor-no-change",
			config: &Config{
				Enabled: false,
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal:   true,
			expectedIsHealthy: false,
			expectedAction:    pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			name: "rule-matches-applies-override",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							IsFatal:           boolPtr(false),
							RecommendedAction: stringPtr("NONE"),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal: false,
			expectedAction:  pb.RecommendedAction_NONE,
		},
		{
			name: "rule-no-match-unchanged",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "other-agent"`,
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal: true,
			expectedAction:  pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			name: "first-match-wins",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "first-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							RecommendedAction: stringPtr("NONE"),
						},
					},
					{
						Name: "second-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							RecommendedAction: stringPtr("CONTACT_SUPPORT"),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				RecommendedAction: pb.RecommendedAction_REPLACE_VM,
			},
			expectedAction: pb.RecommendedAction_NONE,
		},
		{
			name: "partial-override-works",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							IsFatal: boolPtr(false),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal:   false,
			expectedIsHealthy: false,
			expectedAction:    pb.RecommendedAction_CONTACT_SUPPORT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, err := NewProcessor(tt.config)
			require.NoError(t, err)

			err = proc.Transform(context.Background(), tt.event)
			require.NoError(t, err)

			assert.Equal(t, tt.expectedIsFatal, tt.event.IsFatal, "isFatal mismatch")
			assert.Equal(t, tt.expectedIsHealthy, tt.event.IsHealthy, "isHealthy mismatch")
			assert.Equal(t, tt.expectedAction, tt.event.RecommendedAction, "recommendedAction mismatch")
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}
