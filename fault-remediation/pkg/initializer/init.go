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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/reconciler"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/remediation"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	storeconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
	ctrlruntimeClient "sigs.k8s.io/controller-runtime/pkg/client"
)

type InitializationParams struct {
	Config             *rest.Config
	TomlConfigPath     string
	DryRun             bool
	EnableLogCollector bool
}

type Components struct {
	FaultRemediationReconciler reconciler.FaultRemediationReconciler
}

func InitializeAll(
	ctx context.Context,
	params InitializationParams,
	ctrlruntimeClient ctrlruntimeClient.Client,
) (*Components, error) {
	slog.Info("Starting fault remediation module initialization")

	tokenConfig, tomlConfig, err := loadTokenAndTomlConfig(params)
	if err != nil {
		return nil, err
	}

	builder := client.GetPipelineBuilder()
	pipeline := builder.BuildQuarantinedAndDrainedNodesPipeline()

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	if params.EnableLogCollector {
		slog.Info("Log collector enabled")
	}

	remediationClient, stateManager, err := initRemediationAndStateManager(params.Config, ctrlruntimeClient,
		params.DryRun, tomlConfig)
	if err != nil {
		return nil, err
	}

	slog.Info("Successfully initialized client")

	ds, watcherInstance, healthEventStore, datastoreConfig, err := initDatastoreAndWatcher(ctx, pipeline)
	if err != nil {
		return nil, err
	}

	clientTokenConfig := client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}

	reconcilerCfg := reconciler.ReconcilerConfig{
		DataStoreConfig:    *datastoreConfig,
		TokenConfig:        clientTokenConfig,
		Pipeline:           pipeline,
		RemediationClient:  remediationClient,
		StateManager:       stateManager,
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

func loadTokenAndTomlConfig(params InitializationParams) (*storeconfig.TokenConfig, *config.TomlConfig, error) {
	tokenConfig, err := storeconfig.TokenConfigFromEnv("fault-remediation")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	var tomlConfig config.TomlConfig
	if err := configmanager.LoadTOMLConfig(params.TomlConfigPath, &tomlConfig); err != nil {
		return nil, nil, fmt.Errorf("error while loading the toml Config: %w", err)
	}

	if err := tomlConfig.Validate(); err != nil {
		return nil, nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &tokenConfig, &tomlConfig, nil
}

func initRemediationAndStateManager(
	restConfig *rest.Config,
	ctrlruntimeClient ctrlruntimeClient.Client,
	dryRun bool,
	tomlConfig *config.TomlConfig,
) (*remediation.FaultRemediationClient, statemanager.StateManager, error) {
	remediationClient, err := remediation.NewRemediationClient(ctrlruntimeClient, dryRun, *tomlConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("error while initializing remediation client: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("error init kube client for state manager: %w", err)
	}

	stateManager := statemanager.NewStateManager(kubeClient)

	return remediationClient, stateManager, nil
}

func initDatastoreAndWatcher(
	ctx context.Context,
	pipeline datastore.Pipeline,
) (datastore.DataStore, datastore.ChangeStreamWatcher, datastore.HealthEventStore, *datastore.DataStoreConfig, error) {
	datastoreConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to load datastore configuration: %w", err)
	}

	ds, err := datastore.NewDataStore(ctx, *datastoreConfig)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error initializing datastore: %w", err)
	}

	watcherConfig := watcher.WatcherConfig{
		Pipeline:       pipeline,
		CollectionName: "HealthEvents",
	}

	watcherInstance, err := watcher.CreateChangeStreamWatcher(ctx, ds, watcherConfig)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error initializing change stream watcher: %w", err)
	}

	healthEventStore := ds.HealthEventStore()

	return ds, watcherInstance, healthEventStore, datastoreConfig, nil
}
