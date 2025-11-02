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
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/eventwatcher"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	_ "github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore/providers"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/helper"
)

type InitializationParams struct {
	DatabaseClientCertMountPath string
	KubeconfigPath              string
	TomlConfigPath              string
	DryRun                      bool
	CircuitBreakerPercentage    int
	CircuitBreakerDuration      time.Duration
	CircuitBreakerEnabled       bool
}

type Components struct {
	Reconciler     *reconciler.Reconciler
	EventWatcher   *eventwatcher.EventWatcher
	K8sClient      *informer.FaultQuarantineClient
	CircuitBreaker breaker.CircuitBreaker
}

type EnvConfig struct {
	Namespace                                    string
	UnprocessedEventsMetricUpdateIntervalSeconds int
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting fault quarantine module initialization")

	envConfig, err := loadEnvConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load environment configuration: %w", err)
	}

	// Use standardized datastore client initialization
	pipeline := client.BuildInsertOnlyPipelineAgnostic()

	// Create datastore client bundle using helper with custom cert path
	bundle, err := helper.NewDatastoreClientWithCertPath(ctx, "fault-quarantine-module", params.DatabaseClientCertMountPath, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to create datastore client bundle: %w", err)
	}
	defer bundle.Close(ctx)

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

	var circuitBreaker breaker.CircuitBreaker

	if params.CircuitBreakerEnabled {
		cb, err := initializeCircuitBreaker(
			ctx,
			k8sClient,
			envConfig.Namespace,
			params.CircuitBreakerPercentage,
			params.CircuitBreakerDuration,
		)
		if err != nil {
			return nil, fmt.Errorf("error while initializing circuit breaker: %w", err)
		}

		circuitBreaker = cb

		slog.Info("Successfully initialized circuit breaker")
	} else {
		slog.Info("Circuit breaker is disabled, skipping initialization")
	}

	reconcilerCfg := createReconcilerConfig(
		tomlCfg,
		params.DryRun,
		params.CircuitBreakerEnabled,
	)

	reconcilerInstance := reconciler.NewReconciler(
		reconcilerCfg,
		k8sClient,
		circuitBreaker,
	)

	eventWatcher := eventwatcher.NewEventWatcher(
		bundle.ChangeStreamWatcher,
		bundle.DatabaseClient,
		time.Duration(envConfig.UnprocessedEventsMetricUpdateIntervalSeconds)*time.Second,
		reconcilerInstance,
	)

	reconcilerInstance.SetEventWatcher(eventWatcher)

	slog.Info("Initialization completed successfully")

	return &Components{
		Reconciler:     reconcilerInstance,
		EventWatcher:   eventWatcher,
		K8sClient:      k8sClient,
		CircuitBreaker: circuitBreaker,
	}, nil
}


func loadEnvConfig() (*EnvConfig, error) {
	envSpecs := []configmanager.EnvVarSpec{
		{Name: "POD_NAMESPACE"},
	}

	envVars, envErrors := configmanager.ReadEnvVars(envSpecs)
	if len(envErrors) > 0 {
		for _, err := range envErrors {
			slog.Error("Environment variable error", "error", err)
		}

		return nil, fmt.Errorf("required environment variables are missing")
	}

	unprocessedEventsMetricUpdateIntervalSeconds, err :=
		getPositiveIntEnvVar("UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS", 25)
	if err != nil {
		return nil, err
	}

	return &EnvConfig{
		Namespace: envVars["POD_NAMESPACE"],
		UnprocessedEventsMetricUpdateIntervalSeconds: unprocessedEventsMetricUpdateIntervalSeconds,
	}, nil
}

func getPositiveIntEnvVar(name string, defaultValue int) (int, error) {
	value, err := configmanager.GetEnvVar[int](name, &defaultValue,
		func(v int) error {
			if v <= 0 {
				return fmt.Errorf("must be positive")
			}

			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}

	return value, nil
}

func createReconcilerConfig(
	tomlCfg config.TomlConfig,
	dryRun bool,
	circuitBreakerEnabled bool,
) reconciler.ReconcilerConfig {
	return reconciler.ReconcilerConfig{
		TomlConfig:            tomlCfg,
		DryRun:                dryRun,
		CircuitBreakerEnabled: circuitBreakerEnabled,
	}
}

func initializeCircuitBreaker(
	ctx context.Context,
	k8sClient *informer.FaultQuarantineClient,
	namespace string,
	percentage int,
	duration time.Duration,
) (breaker.CircuitBreaker, error) {
	circuitBreakerName := "fault-quarantine-circuit-breaker"

	slog.Info("Initializing circuit breaker", "configMap", circuitBreakerName, "namespace", namespace)

	cb, err := breaker.NewSlidingWindowBreaker(ctx, breaker.Config{
		Window:             duration,
		TripPercentage:     float64(percentage),
		K8sClient:          k8sClient,
		ConfigMapName:      circuitBreakerName,
		ConfigMapNamespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize circuit breaker: %w", err)
	}

	return cb, nil
}
