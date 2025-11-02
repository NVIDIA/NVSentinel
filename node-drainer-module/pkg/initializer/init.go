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

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/adapter"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	sdkconfig "github.com/nvidia/nvsentinel/store-client-sdk/pkg/config"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/factory"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type InitializationParams struct {
	DatabaseClientCertMountPath string
	KubeconfigPath              string
	TomlConfigPath              string
	MetricsPort                 string
	DryRun                      bool
}

type Components struct {
	Informers    *informers.Informers
	EventWatcher client.ChangeStreamWatcher
	QueueManager queue.EventQueueManager
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting node drainer initialization")

	envConfig, err := config.LoadEnvConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load environment configuration: %w", err)
	}

	// Load datastore configuration using the new unified system
	dsConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Convert to legacy DatabaseConfig interface for compatibility with existing factory
	// Pass the certificate mount path to the adapter to handle path resolution at runtime
	databaseConfig := adapter.ConvertDataStoreConfigToLegacyWithCertPath(dsConfig, params.DatabaseClientCertMountPath)
	tokenConfig := config.NewTokenConfig(envConfig)
	pipeline := config.NewQuarantinePipeline()

	tomlCfg, err := config.LoadTomlConfig(params.TomlConfigPath)
	if err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	clientSet, err := initializeKubernetesClient(params.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized kubernetes client")

	informersInstance, err := initializeInformers(clientSet, &tomlCfg.NotReadyTimeoutMinutes, params.DryRun)
	if err != nil {
		return nil, fmt.Errorf("error while initializing informers: %w", err)
	}

	stateManager := initializeStateManager(clientSet)
	reconcilerCfg := createReconcilerConfig(*tomlCfg, databaseConfig, tokenConfig, pipeline, stateManager)

	// Reconciler creates its own queue manager
	reconciler := initializeReconciler(reconcilerCfg, params.DryRun, clientSet, informersInstance)
	queueManager := reconciler.GetQueueManager()

	// Create client factory and database client from databaseConfig
	clientFactory := factory.NewClientFactory(databaseConfig)

	// Database client removed as functionality moved to store-client-sdk

	changeStreamWatcher, err := clientFactory.CreateChangeStreamWatcher(ctx, "node-drainer-module", pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	// Use the change stream watcher directly
	eventWatcher := changeStreamWatcher

	slog.Info("Initialization completed successfully")

	return &Components{
		Informers:    informersInstance,
		EventWatcher: eventWatcher,
		QueueManager: queueManager,
	}, nil
}

func initializeKubernetesClient(kubeconfigPath string) (kubernetes.Interface, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build config: %w", err)
	}

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	return clientSet, nil
}

func initializeInformers(clientset kubernetes.Interface,
	notReadyTimeoutMinutes *int, dryRun bool) (*informers.Informers, error) {
	return informers.NewInformers(clientset, time.Hour, notReadyTimeoutMinutes, dryRun)
}

func initializeStateManager(clientSet kubernetes.Interface) statemanager.StateManager {
	return statemanager.NewStateManager(clientSet)
}

func createReconcilerConfig(
	tomlCfg config.TomlConfig,
	databaseConfig sdkconfig.DatabaseConfig,
	tokenConfig client.TokenConfig,
	pipeline interface{}, // Still passed for potential future use, but not stored in config
	stateManager statemanager.StateManager,
) config.ReconcilerConfig {
	return config.ReconcilerConfig{
		TomlConfig:     tomlCfg,
		DatabaseConfig: databaseConfig,
		TokenConfig:    tokenConfig,
		StateManager:   stateManager,
	}
}

func initializeReconciler(
	cfg config.ReconcilerConfig,
	dryRun bool,
	kubeClient kubernetes.Interface,
	informersInstance *informers.Informers,
) *reconciler.Reconciler {
	return reconciler.NewReconciler(cfg, dryRun, kubeClient, informersInstance)
}
