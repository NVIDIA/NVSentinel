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
	"os"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	storeconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
)

type InitializationParams struct {
	KubeconfigPath              string
	TomlConfigPath              string
	DryRun                      bool
	CircuitBreakerEnabled       bool
	DatabaseClientCertMountPath string
}

type Components struct {
	Reconciler      *reconciler.Reconciler
	K8sClient       *informer.FaultQuarantineClient
	CircuitBreaker  breaker.CircuitBreaker
	DatastoreConfig *datastore.DataStoreConfig
	Pipeline        interface{}
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting fault quarantine module initialization")

	tokenConfig, err := storeconfig.TokenConfigFromEnv("fault-quarantine")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	// Load datastore configuration using new pattern
	datastoreConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load datastore configuration: %w", err)
	}

	builder := client.GetPipelineBuilder()
	pipeline := builder.BuildAllHealthEventInsertsPipeline()

	var tomlCfg config.TomlConfig
	if err := configmanager.LoadTOMLConfig(params.TomlConfigPath, &tomlCfg); err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	k8sClient, err := informer.NewFaultQuarantineClient(params.KubeconfigPath, params.DryRun, 30*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized kubernetes client with embedded node informer")

	circuitBreaker, err := setupCircuitBreaker(ctx, params, tomlCfg, k8sClient)
	if err != nil {
		return nil, err
	}

	reconcilerCfg := createReconcilerConfig(
		tomlCfg,
		params.DryRun,
		params.CircuitBreakerEnabled,
		datastoreConfig,
		tokenConfig,
		pipeline,
	)

	reconcilerInstance := reconciler.NewReconciler(
		reconcilerCfg,
		k8sClient,
		circuitBreaker,
	)

	slog.Info("Initialization completed successfully")

	return &Components{
		Reconciler:      reconcilerInstance,
		K8sClient:       k8sClient,
		CircuitBreaker:  circuitBreaker,
		DatastoreConfig: datastoreConfig,
		Pipeline:        pipeline,
	}, nil
}

func createReconcilerConfig(
	tomlCfg config.TomlConfig,
	dryRun bool,
	circuitBreakerEnabled bool,
	datastoreConfig *datastore.DataStoreConfig,
	tokenConfig storeconfig.TokenConfig,
	pipeline interface{},
) reconciler.ReconcilerConfig {
	// Convert store config types to the types expected by reconciler
	clientTokenConfig := client.TokenConfig{
		ClientName:      tokenConfig.ClientName,
		TokenDatabase:   tokenConfig.TokenDatabase,
		TokenCollection: tokenConfig.TokenCollection,
	}

	return reconciler.ReconcilerConfig{
		TomlConfig:            tomlCfg,
		DryRun:                dryRun,
		CircuitBreakerEnabled: circuitBreakerEnabled,
		DataStoreConfig:       datastoreConfig,
		TokenConfig:           clientTokenConfig,
		DatabasePipeline:      pipeline,
	}
}

func setupCircuitBreaker(
	ctx context.Context,
	params InitializationParams,
	tomlCfg config.TomlConfig,
	k8sClient *informer.FaultQuarantineClient,
) (breaker.CircuitBreaker, error) {
	if !params.CircuitBreakerEnabled {
		slog.Info("Circuit breaker is disabled, skipping initialization")
		return nil, nil
	}

	// Use command line parameters if provided, otherwise fall back to TOML config
	cbConfig := tomlCfg.CircuitBreaker

	cb, err := initializeCircuitBreaker(ctx, k8sClient, cbConfig)
	if err != nil {
		return nil, fmt.Errorf("error while initializing circuit breaker: %w", err)
	}

	slog.Info("Successfully initialized circuit breaker")

	return cb, nil
}

func initializeCircuitBreaker(
	ctx context.Context,
	k8sClient *informer.FaultQuarantineClient,
	cbConfig config.CircuitBreaker,
) (breaker.CircuitBreaker, error) {
	circuitBreakerName := "circuit-breaker"

	namespace := os.Getenv("POD_NAMESPACE")

	duration, err := time.ParseDuration(cbConfig.Duration)
	if err != nil {
		return nil, fmt.Errorf("invalid circuit breaker duration %q: %w", cbConfig.Duration, err)
	}

	slog.Info("Initializing circuit breaker",
		"configMap", circuitBreakerName,
		"namespace", namespace,
		"percentage", cbConfig.Percentage,
		"duration", cbConfig.Duration)

	cb, err := breaker.NewSlidingWindowBreaker(ctx, breaker.Config{
		Window:             duration,
		TripPercentage:     float64(cbConfig.Percentage),
		K8sClient:          k8sClient,
		ConfigMapName:      circuitBreakerName,
		ConfigMapNamespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize circuit breaker: %w", err)
	}

	return cb, nil
}
