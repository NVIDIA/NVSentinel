/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pkg

import (
	"testing"
)

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
	}{
		{
			name: "ValidConfig_IsHealthyTrue",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         true,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "ValidConfig_IsHealthyFalse",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "ValidConfig_DefaultValues",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "default",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "MissingNodeName",
			config: &Config{
				NodeName:          "",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: true,
		},
		{
			name: "MissingErrorCode",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: true,
		},
		{
			name: "MissingReason",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: true,
		},
		{
			name: "MissingCreatorID",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: true,
		},
		{
			name: "DefaultCreatorID",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "default",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "RecommendedActionNegativeOne",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: -1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "RecommendedActionOne",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: 1,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
		{
			name: "RecommendedActionNegative",
			config: &Config{
				NodeName:          "test-node",
				ErrorCode:         "TEST_ERROR",
				Reason:            "Test reason",
				IsHealthy:         false,
				RecommendedAction: -5,
				CreatorID:         "test-creator",
				Force:             false,
				SkipQuarantine:    false,
				SkipDrain:         false,
				SocketPath:        "/tmp/test.sock",
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.config)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				if err.Error() == "" {
					t.Error("Expected validation error message, got empty string")
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
			}
		})
	}
}
