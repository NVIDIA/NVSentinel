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

package initializer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	storeconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

type InitializationParams struct {
	KubeconfigPath     string
	TomlConfigPath     string
	DryRun             bool
	EnableLogCollector bool
}

type Components struct {
	FaultRemediationReconciler reconciler.FaultRemediationReconciler
}

// nolint: cyclop // todo
func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting fault remediation module initialization")

	tokenConfig, err := storeconfig.TokenConfigFromEnv("fault-remediation")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	builder := client.GetPipelineBuilder()
	pipeline := builder.BuildQuarantinedAndDrainedNodesPipeline()

	var tomlConfig config.TomlConfig
	if err := configmanager.LoadTOMLConfig(params.TomlConfigPath, &tomlConfig); err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	// Validate the configuration for consistency
	if err := tomlConfig.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	if params.EnableLogCollector {
		slog.Info("Log collector enabled")
	}

	// Initialize fault remediation client with full config
	k8sClient, clientSet, err := reconciler.NewK8sClient(
		params.KubeconfigPath,
		params.DryRun,
		tomlConfig,
	)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized k8s client")

	// Load datastore configuration
	datastoreConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore configuration: %w", err)
	}

	// Convert store config types to the types expected by reconciler
	clientTokenConfig := client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}

	ds, err := datastore.NewDataStore(ctx, *datastoreConfig)
	if err != nil {
		return nil, fmt.Errorf("error initializing datastore: %w", err)
	}

	// Create watcher using the factory pattern
	watcherConfig := watcher.WatcherConfig{
		Pipeline:       pipeline,
		CollectionName: "HealthEvents",
	}

	watcherInstance, err := watcher.CreateChangeStreamWatcher(ctx, ds, watcherConfig)
	if err != nil {
		return nil, fmt.Errorf("error initializing change stream watcher: %w", err)
	}

	// Get the HealthEventStore for document operations
	healthEventStore := ds.HealthEventStore()

	reconcilerCfg := reconciler.ReconcilerConfig{
		DataStoreConfig:    *datastoreConfig,
		TokenConfig:        clientTokenConfig,
		Pipeline:           pipeline,
		RemediationClient:  k8sClient,
		StateManager:       statemanager.NewStateManager(clientSet),
		EnableLogCollector: params.EnableLogCollector,
		UpdateMaxRetries:   tomlConfig.UpdateRetry.MaxRetries,
		UpdateRetryDelay:   time.Duration(tomlConfig.UpdateRetry.RetryDelaySeconds) * time.Second,
	}

	slog.Info("Initialization completed successfully")

	return &Components{
		FaultRemediationReconciler: reconciler.NewFaultRemediationReconciler(
			ds, watcherInstance, healthEventStore, reconcilerCfg, params.DryRun),
	}, nil
}
