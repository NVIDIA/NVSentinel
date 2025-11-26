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
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
)

func TestProviderRegistration(t *testing.T) {
	// Test that the PostgreSQL provider is registered
	factory := datastore.GetProvider(datastore.ProviderPostgreSQL)
	assert.NotNil(t, factory, "PostgreSQL provider should be registered")

	// Skip the actual datastore creation test as it requires a database
	t.Skip("Skipping datastore creation that requires PostgreSQL database - runs in integration tests")

	// Test creating a datastore instance with valid config
	config := datastore.DataStoreConfig{
		Provider: datastore.ProviderPostgreSQL,
		Connection: datastore.ConnectionConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "test",
			Username: "testuser",
			SSLMode:  "disable",
		},
	}

	ds, err := factory(context.Background(), config)
	assert.NoError(t, err)
	assert.NotNil(t, ds)
	assert.IsType(t, &PostgreSQLDataStore{}, ds)
}

func TestNewPostgreSQLStoreIntegration(t *testing.T) {
	t.Skip("Skipping integration test that requires PostgreSQL database - runs in integration tests")

	// Test the factory function directly
	config := datastore.DataStoreConfig{
		Provider: datastore.ProviderPostgreSQL,
		Connection: datastore.ConnectionConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "test",
			Username: "testuser",
			SSLMode:  "disable",
		},
	}

	ds, err := NewPostgreSQLStore(context.Background(), config)
	assert.NoError(t, err)
	assert.NotNil(t, ds)

	// Verify the datastore is of the correct type
	_, ok := ds.(*PostgreSQLDataStore)
	assert.True(t, ok, "datastore should be PostgreSQLDataStore type")

	// Test basic functionality
	assert.NotNil(t, ds.MaintenanceEventStore())
	assert.NotNil(t, ds.HealthEventStore())
}
