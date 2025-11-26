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

package datastore_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/postgresql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataStoreInterfaceCompliance verifies that all provider implementations
// properly implement the DataStore interface. This test protects the contract
// between store-client and all consuming modules.
func TestDataStoreInterfaceCompliance(t *testing.T) {
	// Compile-time interface compliance checks
	// These will fail to compile if the interfaces are not properly implemented
	var _ datastore.DataStore = (*mongodb.AdaptedMongoStore)(nil)
	var _ datastore.DataStore = (*postgresql.PostgreSQLDataStore)(nil)

	t.Run("MongoDB implements DataStore", func(t *testing.T) {
		// Verify MongoDB Store implements all DataStore methods
		storeType := reflect.TypeOf((*mongodb.AdaptedMongoStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.DataStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "MongoDB AdaptedMongoStore")
	})

	t.Run("PostgreSQL implements DataStore", func(t *testing.T) {
		// Verify PostgreSQL Store implements all DataStore methods
		storeType := reflect.TypeOf((*postgresql.PostgreSQLDataStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.DataStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "PostgreSQL DataStore")
	})
}

// TestMaintenanceEventStoreInterfaceCompliance verifies that all provider
// maintenance event store implementations properly implement the interface.
func TestMaintenanceEventStoreInterfaceCompliance(t *testing.T) {
	// Compile-time interface compliance checks
	var _ datastore.MaintenanceEventStore = (*mongodb.MongoMaintenanceEventStore)(nil)
	var _ datastore.MaintenanceEventStore = (*postgresql.PostgreSQLMaintenanceEventStore)(nil)

	t.Run("MongoDB implements MaintenanceEventStore", func(t *testing.T) {
		storeType := reflect.TypeOf((*mongodb.MongoMaintenanceEventStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.MaintenanceEventStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "MongoDB MaintenanceEventStore")
	})

	t.Run("PostgreSQL implements MaintenanceEventStore", func(t *testing.T) {
		storeType := reflect.TypeOf((*postgresql.PostgreSQLMaintenanceEventStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.MaintenanceEventStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "PostgreSQL MaintenanceEventStore")
	})

	t.Run("MaintenanceEventStore methods exist", func(t *testing.T) {
		// Verify all expected methods exist on the interface
		interfaceType := reflect.TypeOf((*datastore.MaintenanceEventStore)(nil)).Elem()

		expectedMethods := []string{
			"UpsertMaintenanceEvent",
			"FindEventsToTriggerQuarantine",
			"FindEventsToTriggerHealthy",
			"UpdateEventStatus",
			"GetLastProcessedEventTimestampByCSP",
			"FindLatestActiveEventByNodeAndType",
			"FindLatestOngoingEventByNode",
			"FindActiveEventsByStatuses",
		}

		for _, methodName := range expectedMethods {
			method, found := interfaceType.MethodByName(methodName)
			assert.True(t, found, "MaintenanceEventStore should have method: %s", methodName)
			if found {
				// Verify method has correct signature (takes context.Context as first param)
				numIn := method.Type.NumIn()
				assert.GreaterOrEqual(t, numIn, 1, "Method %s should have at least 1 input parameter", methodName)
				if numIn >= 1 {
					// For interface methods, there's no receiver, so first param is at index 0
					firstParam := method.Type.In(0)
					ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
					assert.True(t, firstParam.Implements(ctxType) || firstParam == ctxType,
						"Method %s first parameter should be context.Context, got %s",
						methodName, firstParam)
				}
			}
		}
	})
}

// TestHealthEventStoreInterfaceCompliance verifies that all provider
// health event store implementations properly implement the interface.
func TestHealthEventStoreInterfaceCompliance(t *testing.T) {
	// Compile-time interface compliance checks
	var _ datastore.HealthEventStore = (*mongodb.MongoHealthEventStore)(nil)
	var _ datastore.HealthEventStore = (*postgresql.PostgreSQLHealthEventStore)(nil)

	t.Run("MongoDB implements HealthEventStore", func(t *testing.T) {
		storeType := reflect.TypeOf((*mongodb.MongoHealthEventStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.HealthEventStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "MongoDB HealthEventStore")
	})

	t.Run("PostgreSQL implements HealthEventStore", func(t *testing.T) {
		storeType := reflect.TypeOf((*postgresql.PostgreSQLHealthEventStore)(nil))
		interfaceType := reflect.TypeOf((*datastore.HealthEventStore)(nil)).Elem()

		assertImplementsInterface(t, storeType, interfaceType, "PostgreSQL HealthEventStore")
	})

	t.Run("HealthEventStore methods exist", func(t *testing.T) {
		// Verify all expected methods exist on the interface
		interfaceType := reflect.TypeOf((*datastore.HealthEventStore)(nil)).Elem()

		expectedMethods := []string{
			"UpdateHealthEventStatus",
			"UpdateHealthEventStatusByNode",
			"FindHealthEventsByNode",
			"FindHealthEventsByFilter",
			"FindHealthEventsByStatus",
			"UpdateNodeQuarantineStatus",
			"UpdatePodEvictionStatus",
			"UpdateRemediationStatus",
			"CheckIfNodeAlreadyDrained",
			"FindLatestEventForNode",
		}

		for _, methodName := range expectedMethods {
			method, found := interfaceType.MethodByName(methodName)
			assert.True(t, found, "HealthEventStore should have method: %s", methodName)
			if found {
				// Verify method has correct signature (takes context.Context as first param)
				numIn := method.Type.NumIn()
				assert.GreaterOrEqual(t, numIn, 1, "Method %s should have at least 1 input parameter", methodName)
				if numIn >= 1 {
					// For interface methods, there's no receiver, so first param is at index 0
					firstParam := method.Type.In(0)
					ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
					assert.True(t, firstParam.Implements(ctxType) || firstParam == ctxType,
						"Method %s first parameter should be context.Context, got %s",
						methodName, firstParam)
				}
			}
		}
	})
}

// TestChangeStreamWatcherInterfaceCompliance verifies that change stream
// watcher implementations properly implement the interface.
func TestChangeStreamWatcherInterfaceCompliance(t *testing.T) {
	// Compile-time interface compliance checks
	// Note: MongoDB uses an adapted watcher that conforms through the AdaptedChangeStreamWatcher wrapper
	var _ datastore.ChangeStreamWatcher = (*postgresql.PostgreSQLChangeStreamWatcher)(nil)

	// MongoDB watcher is wrapped by AdaptedChangeStreamWatcher in the adapter layer
	t.Run("MongoDB has ChangeStreamWatcher wrapper", func(t *testing.T) {
		// The AdaptedChangeStreamWatcher in the adapter.go wraps the MongoDB watcher
		watcherType := reflect.TypeOf((*mongodb.AdaptedChangeStreamWatcher)(nil))
		interfaceType := reflect.TypeOf((*datastore.ChangeStreamWatcher)(nil)).Elem()

		assertImplementsInterface(t, watcherType, interfaceType, "MongoDB AdaptedChangeStreamWatcher")
	})

	t.Run("PostgreSQL implements ChangeStreamWatcher", func(t *testing.T) {
		watcherType := reflect.TypeOf((*postgresql.PostgreSQLChangeStreamWatcher)(nil))
		interfaceType := reflect.TypeOf((*datastore.ChangeStreamWatcher)(nil)).Elem()

		assertImplementsInterface(t, watcherType, interfaceType, "PostgreSQL ChangeStreamWatcher")
	})

	t.Run("ChangeStreamWatcher methods exist", func(t *testing.T) {
		// Verify all expected methods exist on the interface
		interfaceType := reflect.TypeOf((*datastore.ChangeStreamWatcher)(nil)).Elem()

		expectedMethods := []string{
			"Events",
			"Start",
			"Close",
			"MarkProcessed",
		}

		for _, methodName := range expectedMethods {
			_, found := interfaceType.MethodByName(methodName)
			assert.True(t, found, "ChangeStreamWatcher should have method: %s", methodName)
		}
	})
}

// TestProviderFactoryRegistration verifies that both providers are properly
// registered with the factory and can be instantiated.
func TestProviderFactoryRegistration(t *testing.T) {
	t.Run("MongoDB provider is registered", func(t *testing.T) {
		providers := datastore.SupportedProviders()
		assert.Contains(t, providers, datastore.ProviderMongoDB,
			"MongoDB provider should be registered")

		factory := datastore.GetProvider(datastore.ProviderMongoDB)
		assert.NotNil(t, factory, "MongoDB provider factory should be available")
	})

	t.Run("PostgreSQL provider is registered", func(t *testing.T) {
		providers := datastore.SupportedProviders()
		assert.Contains(t, providers, datastore.ProviderPostgreSQL,
			"PostgreSQL provider should be registered")

		factory := datastore.GetProvider(datastore.ProviderPostgreSQL)
		assert.NotNil(t, factory, "PostgreSQL provider factory should be available")
	})
}

// TestDataStoreReturnsCorrectInterfaces verifies that DataStore.MaintenanceEventStore()
// and DataStore.HealthEventStore() return implementations that satisfy their interfaces.
func TestDataStoreReturnsCorrectInterfaces(t *testing.T) {
	t.Run("MongoDB DataStore method return types", func(t *testing.T) {
		// Use reflection to check return types without calling methods on nil pointers
		storeType := reflect.TypeOf((*mongodb.AdaptedMongoStore)(nil))

		maintenanceMethod, found := storeType.MethodByName("MaintenanceEventStore")
		require.True(t, found, "MongoDB should have MaintenanceEventStore method")
		require.Equal(t, 1, maintenanceMethod.Type.NumOut(), "MaintenanceEventStore should return 1 value")

		healthMethod, found := storeType.MethodByName("HealthEventStore")
		require.True(t, found, "MongoDB should have HealthEventStore method")
		require.Equal(t, 1, healthMethod.Type.NumOut(), "HealthEventStore should return 1 value")

		// Verify return types implement the interfaces
		maintenanceReturnType := maintenanceMethod.Type.Out(0)
		healthReturnType := healthMethod.Type.Out(0)

		interfaceMaintenanceType := reflect.TypeOf((*datastore.MaintenanceEventStore)(nil)).Elem()
		interfaceHealthType := reflect.TypeOf((*datastore.HealthEventStore)(nil)).Elem()

		assert.True(t, maintenanceReturnType.Implements(interfaceMaintenanceType),
			"MongoDB MaintenanceEventStore return type should implement MaintenanceEventStore interface")
		assert.True(t, healthReturnType.Implements(interfaceHealthType),
			"MongoDB HealthEventStore return type should implement HealthEventStore interface")
	})

	t.Run("PostgreSQL DataStore method return types", func(t *testing.T) {
		// Use reflection to check return types without calling methods on nil pointers
		storeType := reflect.TypeOf((*postgresql.PostgreSQLDataStore)(nil))

		maintenanceMethod, found := storeType.MethodByName("MaintenanceEventStore")
		require.True(t, found, "PostgreSQL should have MaintenanceEventStore method")
		require.Equal(t, 1, maintenanceMethod.Type.NumOut(), "MaintenanceEventStore should return 1 value")

		healthMethod, found := storeType.MethodByName("HealthEventStore")
		require.True(t, found, "PostgreSQL should have HealthEventStore method")
		require.Equal(t, 1, healthMethod.Type.NumOut(), "HealthEventStore should return 1 value")

		// Verify return types implement the interfaces
		maintenanceReturnType := maintenanceMethod.Type.Out(0)
		healthReturnType := healthMethod.Type.Out(0)

		interfaceMaintenanceType := reflect.TypeOf((*datastore.MaintenanceEventStore)(nil)).Elem()
		interfaceHealthType := reflect.TypeOf((*datastore.HealthEventStore)(nil)).Elem()

		assert.True(t, maintenanceReturnType.Implements(interfaceMaintenanceType),
			"PostgreSQL MaintenanceEventStore return type should implement MaintenanceEventStore interface")
		assert.True(t, healthReturnType.Implements(interfaceHealthType),
			"PostgreSQL HealthEventStore return type should implement HealthEventStore interface")
	})
}

// TestCommonMethodSignatures verifies that common methods across providers
// have identical signatures to ensure consistent behavior.
func TestCommonMethodSignatures(t *testing.T) {
	t.Run("Ping has consistent signature", func(t *testing.T) {
		mongoType := reflect.TypeOf((*mongodb.AdaptedMongoStore)(nil))
		pgType := reflect.TypeOf((*postgresql.PostgreSQLDataStore)(nil))

		mongoMethod, mongoFound := mongoType.MethodByName("Ping")
		pgMethod, pgFound := pgType.MethodByName("Ping")

		require.True(t, mongoFound, "MongoDB should have Ping method")
		require.True(t, pgFound, "PostgreSQL should have Ping method")

		assert.Equal(t, mongoMethod.Type.NumIn(), pgMethod.Type.NumIn(),
			"Ping should have same number of input parameters")
		assert.Equal(t, mongoMethod.Type.NumOut(), pgMethod.Type.NumOut(),
			"Ping should have same number of output parameters")
	})

	t.Run("Close has consistent signature", func(t *testing.T) {
		mongoType := reflect.TypeOf((*mongodb.AdaptedMongoStore)(nil))
		pgType := reflect.TypeOf((*postgresql.PostgreSQLDataStore)(nil))

		mongoMethod, mongoFound := mongoType.MethodByName("Close")
		pgMethod, pgFound := pgType.MethodByName("Close")

		require.True(t, mongoFound, "MongoDB should have Close method")
		require.True(t, pgFound, "PostgreSQL should have Close method")

		assert.Equal(t, mongoMethod.Type.NumIn(), pgMethod.Type.NumIn(),
			"Close should have same number of input parameters")
		assert.Equal(t, mongoMethod.Type.NumOut(), pgMethod.Type.NumOut(),
			"Close should have same number of output parameters")
	})

	t.Run("Provider has consistent signature", func(t *testing.T) {
		mongoType := reflect.TypeOf((*mongodb.AdaptedMongoStore)(nil))
		pgType := reflect.TypeOf((*postgresql.PostgreSQLDataStore)(nil))

		mongoMethod, mongoFound := mongoType.MethodByName("Provider")
		pgMethod, pgFound := pgType.MethodByName("Provider")

		require.True(t, mongoFound, "MongoDB should have Provider method")
		require.True(t, pgFound, "PostgreSQL should have Provider method")

		assert.Equal(t, mongoMethod.Type.NumIn(), pgMethod.Type.NumIn(),
			"Provider should have same number of input parameters")
		assert.Equal(t, mongoMethod.Type.NumOut(), pgMethod.Type.NumOut(),
			"Provider should have same number of output parameters")
	})
}

// assertImplementsInterface is a helper function that verifies a type
// implements all methods of an interface.
func assertImplementsInterface(t *testing.T, implType, interfaceType reflect.Type, implName string) {
	t.Helper()

	// Get the number of methods in the interface
	numMethods := interfaceType.NumMethod()
	assert.Greater(t, numMethods, 0, "%s: interface should have methods", implName)

	// Check each method in the interface
	for i := 0; i < numMethods; i++ {
		method := interfaceType.Method(i)

		// Check if the implementation has this method
		implMethod, found := implType.MethodByName(method.Name)
		assert.True(t, found, "%s: should implement method %s", implName, method.Name)

		if found {
			// Verify the method signature matches
			// Note: For concrete types, NumIn() includes the receiver (first param),
			// but for interface methods, it doesn't. So we need to add 1 to the
			// interface method's NumIn() to compare, or subtract 1 from the impl method's NumIn().
			// We'll do the latter to be more explicit.
			implNumIn := implMethod.Type.NumIn() - 1 // Subtract receiver

			assert.Equal(t, method.Type.NumIn(), implNumIn,
				"%s: method %s should have %d input parameters (excluding receiver)",
				implName, method.Name, method.Type.NumIn())
			assert.Equal(t, method.Type.NumOut(), implMethod.Type.NumOut(),
				"%s: method %s should have %d output parameters",
				implName, method.Name, method.Type.NumOut())
		}
	}
}
