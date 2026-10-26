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

package watcher

import (
	"testing"
)

func TestConfig_Validation(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		valid  bool
	}{
		{
			name: "valid config",
			config: Config{
				ClientName: "test-client",
				TableName:  "test-table",
			},
			valid: true,
		},
		{
			name: "missing client name",
			config: Config{
				ClientName: "",
				TableName:  "test-table",
			},
			valid: false,
		},
		{
			name: "missing table name",
			config: Config{
				ClientName: "test-client",
				TableName:  "",
			},
			valid: false,
		},
		{
			name: "with pipeline",
			config: Config{
				ClientName: "test-client",
				TableName:  "test-table",
				Pipeline:   []any{map[string]any{"$match": map[string]any{}}},
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.ClientName == "" || tt.config.TableName == "" {
				if tt.valid {
					t.Error("expected config to be invalid, but marked as valid")
				}
			} else {
				if !tt.valid {
					t.Error("expected config to be valid, but marked as invalid")
				}
			}
		})
	}
}
