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
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/mongodb"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type InitializationParams struct {
	MongoClientCertMountPath string
	KubeconfigPath           string
	TomlConfigPath           string
	MetricsPort              string
	DryRun                   bool
}

type Components struct {
	Informers    *informers.Informers
	EventWatcher *mongodb.EventWatcher
	QueueManager queue.EventQueueManager
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting node drainer initialization")

	envConfig, err := config.LoadEnvConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load environment configuration: %w", err)
	}

	mongoConfig := config.NewMongoConfig(envConfig, params.MongoClientCertMountPath)
	tokenConfig := config.NewTokenConfig(envConfig)
	pipeline := config.NewMongoPipeline()

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
	reconcilerCfg := createReconcilerConfig(*tomlCfg, mongoConfig, tokenConfig, pipeline, stateManager)

	// Reconciler creates its own queue manager
	reconciler := initializeReconciler(reconcilerCfg, params.DryRun, clientSet, informersInstance)
	queueManager := reconciler.GetQueueManager()

	collection, err := initializeMongoCollection(ctx, mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error while initializing mongo collection: %w", err)
	}

	eventWatcher := mongodb.NewEventWatcher(
		mongoConfig,
		tokenConfig,
		pipeline,
		queueManager,
		collection,
	)

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
	mongoConfig storewatcher.MongoDBConfig,
	tokenConfig storewatcher.TokenConfig,
	pipeline mongo.Pipeline,
	stateManager statemanager.StateManager,
) config.ReconcilerConfig {
	return config.ReconcilerConfig{
		TomlConfig:    tomlCfg,
		MongoConfig:   mongoConfig,
		TokenConfig:   tokenConfig,
		MongoPipeline: pipeline,
		StateManager:  stateManager,
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

func initializeMongoCollection(
	ctx context.Context,
	mongoConfig storewatcher.MongoDBConfig,
) (queue.MongoCollectionAPI, error) {
	collection, err := storewatcher.GetCollectionClient(ctx, mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get MongoDB collection: %w", err)
	}

	return collection, nil
}
