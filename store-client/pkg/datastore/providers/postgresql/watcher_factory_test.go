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

package postgresql

import (
	"context"
	"database/sql"
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

func TestPostgreSQLWatcherFactory_SupportedProvider(t *testing.T) {
	factory := NewPostgreSQLWatcherFactory()

	if factory.SupportedProvider() != datastore.ProviderPostgreSQL {
		t.Errorf("expected provider PostgreSQL, got %v", factory.SupportedProvider())
	}
}

func TestPostgreSQLWatcherFactory_CreateChangeStreamWatcher_WrongDatastoreType(t *testing.T) {
	ctx := context.Background()
	factory := NewPostgreSQLWatcherFactory()

	// Create a mock datastore that's not PostgreSQL
	mockDS := &mockDataStore{}

	config := watcher.WatcherConfig{
		CollectionName: "test_table",
	}

	_, err := factory.CreateChangeStreamWatcher(ctx, mockDS, config)
	if err == nil {
		t.Error("expected error when passing non-PostgreSQL datastore, got nil")
	}

	expectedMsg := "expected PostgreSQL datastore"
	if err != nil && !contains(err.Error(), expectedMsg) {
		t.Errorf("expected error to contain %q, got %q", expectedMsg, err.Error())
	}
}

func TestPostgreSQLWatcherFactory_CreateChangeStreamWatcher_MissingTableName(t *testing.T) {
	ctx := context.Background()
	factory := NewPostgreSQLWatcherFactory()

	// Create a minimal PostgreSQL datastore for testing
	pgStore := &PostgreSQLDataStore{
		db: &sql.DB{}, // Note: This is a nil DB, but sufficient for this test
	}

	config := watcher.WatcherConfig{
		CollectionName: "", // Missing table name
	}

	_, err := factory.CreateChangeStreamWatcher(ctx, pgStore, config)
	if err == nil {
		t.Error("expected error when CollectionName is empty, got nil")
	}

	expectedMsg := "CollectionName (table name) is required"
	if err != nil && !contains(err.Error(), expectedMsg) {
		t.Errorf("expected error to contain %q, got %q", expectedMsg, err.Error())
	}
}

func TestPostgreSQLWatcherFactory_CreateChangeStreamWatcher_ValidConfig(t *testing.T) {
	// Skip this test as it requires a real database connection
	// Integration tests will cover the full flow
	t.Skip("Skipping test that requires database connection")
}

func TestPostgreSQLWatcherFactory_CreateChangeStreamWatcher_WithClientName(t *testing.T) {
	// This test verifies that ClientName can be extracted from Options
	// We can't fully test without a database, but we can verify the logic path
	t.Skip("Skipping test that requires database connection")
}

func TestPostgreSQLWatcherFactory_CreateChangeStreamWatcher_PipelineWarning(t *testing.T) {
	// This test verifies that a warning is logged when Pipeline is provided
	// PostgreSQL doesn't support MongoDB-style pipelines
	t.Skip("Skipping test that requires database connection and log capture")
}

// mockDataStore is a mock implementation for testing wrong datastore type
type mockDataStore struct{}

func (m *mockDataStore) MaintenanceEventStore() datastore.MaintenanceEventStore { return nil }
func (m *mockDataStore) HealthEventStore() datastore.HealthEventStore           { return nil }
func (m *mockDataStore) Ping(ctx context.Context) error                         { return nil }
func (m *mockDataStore) Close(ctx context.Context) error                        { return nil }
func (m *mockDataStore) Provider() datastore.DataStoreProvider {
	return datastore.DataStoreProvider("mock")
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
