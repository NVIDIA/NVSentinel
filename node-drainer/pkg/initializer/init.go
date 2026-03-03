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

// Package initializer bootstraps all node-drainer runtime dependencies
// (Kubernetes clients, datastore, informers, reconciler) and returns
// them as a single Components struct ready for use by main.
package initializer

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
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
)

// InitializationParams holds the configuration paths and flags needed to bootstrap the node-drainer.
type InitializationParams struct {
	DatabaseClientCertMountPath string
	KubeconfigPath              string
	TomlConfigPath              string
	MetricsPort                 string
	DryRun                      bool
}

// Components holds the initialized runtime dependencies returned by InitializeAll.
type Components struct {
	Informers      *informers.Informers
	EventWatcher   client.ChangeStreamWatcher
	QueueManager   queue.EventQueueManager
	Reconciler     *reconciler.Reconciler
	DatabaseClient client.DatabaseClient
	DataStore      datastore.DataStore
}

// InitializeAll creates all node-drainer runtime dependencies from the given params and returns them as Components.
func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting node drainer initialization")

	configs, err := loadConfigurations(params)
	if err != nil {
		return nil, fmt.Errorf("failed to load configurations: %w", err)
	}

	pipeline := config.NewQuarantinePipeline()

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	if configs.tomlCfg.PartialDrainEnabled {
		slog.Info("Running with partial drain enabled")
	} else {
		slog.Info("Running with partial drain disabled")
	}

	clientSet, restConfig, err := initializeKubernetesClient(params.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized kubernetes client")

	dynamicClient, restMapper, err := initializeDynamicClientAndMapper(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize dynamic client and mapper: %w", err)
	}

	informersInstance, err := initializeInformers(clientSet, &configs.tomlCfg.NotReadyTimeoutMinutes, params.DryRun)
	if err != nil {
		return nil, fmt.Errorf("error while initializing informers: %w", err)
	}

	stateManager := initializeStateManager(clientSet)

	// IMPORTANT: Preserves ClientName="node-drainer" for resume token lookups
	clientTokenConfig := client.TokenConfig{
		ClientName:      configs.tokenConfig.ClientName,
		TokenDatabase:   configs.tokenConfig.TokenDatabase,
		TokenCollection: configs.tokenConfig.TokenCollection,
	}

	reconcilerCfg := createReconcilerConfig(
		*configs.tomlCfg, configs.databaseConfig, clientTokenConfig, stateManager,
	)

	ds, err := datastore.NewDataStore(ctx, *configs.dsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create datastore: %w", err)
	}

	closeOnErr := true

	defer closeOnError(&closeOnErr, ds.Close, "datastore")

	slog.Debug("Created datastore", "provider", configs.dsConfig.Provider)

	dsComponents, err := initializeDatastoreComponents(ctx, ds, clientTokenConfig.ClientName, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize datastore components: %w", err)
	}

	defer closeOnError(&closeOnErr, dsComponents.databaseClient.Close, "database client")

	reconcilerInstance, err := initializeReconciler(
		reconcilerCfg, params.DryRun, clientSet, informersInstance,
		dsComponents.databaseClient, ds.HealthEventStore(), dynamicClient, restMapper,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize reconciler: %w", err)
	}

	queueManager := reconcilerInstance.GetQueueManager()

	slog.Info("Initialization completed successfully")

	closeOnErr = false

	return &Components{
		Informers:      informersInstance,
		EventWatcher:   dsComponents.eventWatcher,
		QueueManager:   queueManager,
		Reconciler:     reconcilerInstance,
		DatabaseClient: dsComponents.databaseClient,
		DataStore:      ds,
	}, nil
}

const cleanupTimeout = 5 * time.Second

func closeOnError(shouldClose *bool, closer func(context.Context) error, resource string) {
	if *shouldClose {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		if cerr := closer(cleanupCtx); cerr != nil {
			slog.Warn("Failed to close resource during error cleanup", "resource", resource, "error", cerr)
		}
	}
}

type initConfigs struct {
	tokenConfig    sdkconfig.TokenConfig
	dsConfig       *datastore.DataStoreConfig
	databaseConfig sdkconfig.DatabaseConfig
	tomlCfg        *config.TomlConfig
}

func loadConfigurations(params InitializationParams) (*initConfigs, error) {
	tokenConfig, err := sdkconfig.TokenConfigFromEnv("node-drainer")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	dsConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore config: %w", err)
	}

	// Convert to legacy DatabaseConfig interface for compatibility with existing factory.
	// Pass the certificate mount path to the adapter to handle path resolution at runtime.
	databaseConfig := adapter.ConvertDataStoreConfigToLegacyWithCertPath(dsConfig, params.DatabaseClientCertMountPath)

	tomlCfg, err := config.LoadTomlConfig(params.TomlConfigPath)
	if err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	return &initConfigs{
		tokenConfig:    tokenConfig,
		dsConfig:       dsConfig,
		databaseConfig: databaseConfig,
		tomlCfg:        tomlCfg,
	}, nil
}

func initializeDynamicClientAndMapper(
	restConfig *rest.Config,
) (dynamic.Interface, *restmapper.DeferredDiscoveryRESTMapper, error) {
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	return dynamicClient, restMapper, nil
}

type datastoreComponents struct {
	databaseClient client.DatabaseClient
	eventWatcher   client.ChangeStreamWatcher
}

func initializeDatastoreComponents(ctx context.Context, ds datastore.DataStore,
	clientName string, pipeline any) (*datastoreComponents, error) {
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

	changeStreamWatcher, err := datastoreAdapter.CreateChangeStreamWatcher(
		ctx, clientName, pipeline)
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

	return &datastoreComponents{
		databaseClient: databaseClient,
		eventWatcher:   unwrapable.Unwrap(),
	}, nil
}

func initializeKubernetesClient(kubeconfigPath string) (kubernetes.Interface, *rest.Config, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build config: %w", err)
	}

	restConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	return clientSet, restConfig, nil
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
	healthEventStore datastore.HealthEventStore,
	dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
) (*reconciler.Reconciler, error) {
	// Create adapter to convert client.DatabaseClient to queue.DataStore interface
	dbAdapter := &databaseClientAdapter{client: databaseClient}

	return reconciler.NewReconciler(cfg, dryRun, kubeClient, informersInstance, dbAdapter, healthEventStore,
		dynamicClient, restMapper)
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
