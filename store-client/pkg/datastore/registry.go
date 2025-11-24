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

package datastore

import (
	"context"
	"fmt"
	"sync"

	"log/slog"
)

// ProviderFactory is a function that creates a datastore instance
type ProviderFactory func(ctx context.Context, config DataStoreConfig) (DataStore, error)

// Global provider registry
var (
	providerRegistry = make(map[DataStoreProvider]ProviderFactory)
	registryMutex    sync.RWMutex
)

// RegisterProvider registers a datastore provider with the global registry
func RegisterProvider(provider DataStoreProvider, factory ProviderFactory) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	providerRegistry[provider] = factory

	slog.Info("Registered datastore provider", "provider", provider)
}

// GetProvider gets a provider factory from the global registry
func GetProvider(provider DataStoreProvider) ProviderFactory {
	registryMutex.RLock()
	defer registryMutex.RUnlock()

	return providerRegistry[provider]
}

// SupportedProviders returns a list of all registered provider types
func SupportedProviders() []DataStoreProvider {
	registryMutex.RLock()
	defer registryMutex.RUnlock()

	providers := make([]DataStoreProvider, 0, len(providerRegistry))
	for provider := range providerRegistry {
		providers = append(providers, provider)
	}

	return providers
}

// Factory implements DataStoreFactory interface
type Factory struct {
	providers map[DataStoreProvider]ProviderFactory
}

// NewFactory creates a new datastore factory
func NewFactory() DataStoreFactory {
	registryMutex.RLock()
	defer registryMutex.RUnlock()

	// Copy the global registry to this factory instance
	providers := make(map[DataStoreProvider]ProviderFactory)
	for provider, factory := range providerRegistry {
		providers[provider] = factory
	}

	slog.Info("Created datastore factory", "providers", len(providers))

	return &Factory{providers: providers}
}

// NewDataStore creates a datastore instance using the factory
func (f *Factory) NewDataStore(ctx context.Context, config DataStoreConfig) (DataStore, error) {
	slog.Info("Creating datastore", "provider", config.Provider)

	factory, exists := f.providers[config.Provider]
	if !exists {
		return nil, fmt.Errorf("unsupported datastore provider: %s. Supported providers: %v",
			config.Provider, f.SupportedProviders())
	}

	datastore, err := factory(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create datastore with provider %s: %w", config.Provider, err)
	}

	return datastore, nil
}

// SupportedProviders returns the list of providers supported by this factory
func (f *Factory) SupportedProviders() []DataStoreProvider {
	providers := make([]DataStoreProvider, 0, len(f.providers))
	for provider := range f.providers {
		providers = append(providers, provider)
	}

	return providers
}

// GetDefaultFactory returns the default global factory
func GetDefaultFactory() DataStoreFactory {
	return NewFactory()
}

// NewDataStore creates a datastore using the default global factory
func NewDataStore(ctx context.Context, config DataStoreConfig) (DataStore, error) {
	factory := GetDefaultFactory()
	return factory.NewDataStore(ctx, config)
}

// ValidateConfig validates datastore configuration
func ValidateConfig(config DataStoreConfig) error {
	if config.Provider == "" {
		return fmt.Errorf("datastore provider is required")
	}

	if config.Connection.Host == "" {
		return fmt.Errorf("connection host is required")
	}

	if config.Connection.Database == "" {
		return fmt.Errorf("database name is required")
	}

	// Check if provider is registered
	if GetProvider(config.Provider) == nil {
		return fmt.Errorf("provider %s is not registered. Supported providers: %v",
			config.Provider, SupportedProviders())
	}

	return nil
}
