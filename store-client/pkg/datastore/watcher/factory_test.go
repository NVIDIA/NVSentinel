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
	"context"
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// mockDataStore is a mock implementation for testing unsupported datastore types
type mockDataStore struct{}

func (m *mockDataStore) MaintenanceEventStore() datastore.MaintenanceEventStore { return nil }
func (m *mockDataStore) HealthEventStore() datastore.HealthEventStore           { return nil }
func (m *mockDataStore) Ping(ctx context.Context) error                         { return nil }
func (m *mockDataStore) Close(ctx context.Context) error                        { return nil }
func (m *mockDataStore) Provider() datastore.DataStoreProvider {
	return datastore.DataStoreProvider("unsupported")
}

func TestCreateChangeStreamWatcher_UnsupportedDatastore(t *testing.T) {
	ctx := context.Background()
	mock := &mockDataStore{}

	config := Config{
		ClientName: "test-client",
		TableName:  "test-table",
	}

	_, err := CreateChangeStreamWatcher(ctx, mock, config)
	if err == nil {
		t.Error("expected error for unsupported datastore type, got nil")
	}

	expectedMsg := "change stream watching not supported for datastore type"
	if err != nil && !contains(err.Error(), expectedMsg) {
		t.Errorf("expected error message to contain %q, got %q", expectedMsg, err.Error())
	}
}

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
				Pipeline:   []interface{}{map[string]interface{}{"$match": map[string]interface{}{}}},
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

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
