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
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/adapter"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	sdkconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"

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
	Informers      *informers.Informers
	EventWatcher   client.ChangeStreamWatcher
	QueueManager   queue.EventQueueManager
	Reconciler     *reconciler.Reconciler
	DatabaseClient client.DatabaseClient
	DataStore      datastore.DataStore
}

//nolint:cyclop // Complexity slightly over limit (11 vs 10) but function is clear and linear
func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting node drainer initialization")

	// Load token configuration - preserves ClientName="node-drainer" for resume token lookups
	tokenConfig, err := sdkconfig.TokenConfigFromEnv("node-drainer")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	// Load datastore configuration using the new unified system
	dsConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Convert to legacy DatabaseConfig interface for compatibility with existing factory
	// Pass the certificate mount path to the adapter to handle path resolution at runtime
	databaseConfig := adapter.ConvertDataStoreConfigToLegacyWithCertPath(dsConfig, params.DatabaseClientCertMountPath)
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

	// Convert store-client TokenConfig to client.TokenConfig type
	// IMPORTANT: Preserves ClientName="node-drainer" for resume token lookups
	clientTokenConfig := client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}

	reconcilerCfg := createReconcilerConfig(*tomlCfg, databaseConfig, clientTokenConfig, pipeline, stateManager)

	// Create NEW database-agnostic datastore
	ds, err := datastore.NewDataStore(ctx, *dsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create datastore: %w", err)
	}

	slog.Debug("Created datastore", "provider", dsConfig.Provider)

	// Get database client and change stream watcher from datastore
	datastoreAdapter, ok := ds.(interface {
		GetDatabaseClient() client.DatabaseClient
		CreateChangeStreamWatcher(
			ctx context.Context, clientName string, pipeline interface{},
		) (datastore.ChangeStreamWatcher, error)
	})
	if !ok {
		return nil, fmt.Errorf("datastore does not support required operations")
	}

	databaseClient := datastoreAdapter.GetDatabaseClient()

	// Reconciler creates its own queue manager and needs the database client
	reconciler := initializeReconciler(reconcilerCfg, params.DryRun, clientSet, informersInstance, databaseClient)
	queueManager := reconciler.GetQueueManager()

	changeStreamWatcher, err := datastoreAdapter.CreateChangeStreamWatcher(
		ctx, "node-drainer", pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	// Unwrap for EventWatcher compatibility
	type unwrapper interface {
		Unwrap() client.ChangeStreamWatcher
	}

	unwrapable, ok := changeStreamWatcher.(unwrapper)
	if !ok {
		return nil, fmt.Errorf("watcher does not support unwrapping to client.ChangeStreamWatcher")
	}

	eventWatcher := unwrapable.Unwrap()

	slog.Info("Initialization completed successfully")

	return &Components{
		Informers:      informersInstance,
		EventWatcher:   eventWatcher,
		QueueManager:   queueManager,
		Reconciler:     reconciler,
		DatabaseClient: databaseClient,
		DataStore:      ds,
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
	databaseClient client.DatabaseClient,
) *reconciler.Reconciler {
	// Create adapter to convert client.DatabaseClient to queue.DataStore interface
	dbAdapter := &databaseClientAdapter{client: databaseClient}
	return reconciler.NewReconciler(cfg, dryRun, kubeClient, informersInstance, dbAdapter)
}

// databaseClientAdapter adapts client.DatabaseClient to queue.DataStore interface
type databaseClientAdapter struct {
	client client.DatabaseClient
}

func (a *databaseClientAdapter) UpdateDocument(
	ctx context.Context, filter, update interface{},
) (*client.UpdateResult, error) {
	return a.client.UpdateDocument(ctx, filter, update)
}

func (a *databaseClientAdapter) FindDocument(
	ctx context.Context, filter interface{}, options *client.FindOneOptions,
) (client.SingleResult, error) {
	return a.client.FindOne(ctx, filter, options)
}

func (a *databaseClientAdapter) FindDocuments(
	ctx context.Context, filter interface{}, options *client.FindOptions,
) (client.Cursor, error) {
	return a.client.Find(ctx, filter, options)
}
