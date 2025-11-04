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
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type InitializationParams struct {
	KubeconfigPath     string
	TomlConfigPath     string
	DryRun             bool
	EnableLogCollector bool
}

type Components struct {
	Reconciler *reconciler.Reconciler
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	slog.Info("Starting fault remediation module initialization")

	mongoConfig, tokenConfig, err := storewatcher.LoadConfigFromEnv("fault-remediation")
	if err != nil {
		return nil, fmt.Errorf("failed to load MongoDB configuration: %w", err)
	}

	pipeline := createMongoPipeline()

	var tomlConfig config.TomlConfig
	if err := configmanager.LoadTOMLConfig(params.TomlConfigPath, &tomlConfig); err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	if params.DryRun {
		slog.Info("Running in dry-run mode")
	}

	if params.EnableLogCollector {
		slog.Info("Log collector enabled")
	}

	k8sClient, clientSet, err := reconciler.NewK8sClient(
		params.KubeconfigPath,
		params.DryRun,
		reconciler.TemplateData{
			TemplateMountPath:   tomlConfig.Template.MountPath,
			TemplateFileName:    tomlConfig.Template.FileName,
			MaintenanceResource: tomlConfig.MaintenanceResource,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized k8s client")

	reconcilerCfg := reconciler.ReconcilerConfig{
		MongoConfig:        mongoConfig,
		TokenConfig:        tokenConfig,
		MongoPipeline:      pipeline,
		RemediationClient:  k8sClient,
		StateManager:       statemanager.NewStateManager(clientSet),
		EnableLogCollector: params.EnableLogCollector,
		UpdateMaxRetries:   tomlConfig.UpdateRetry.MaxRetries,
		UpdateRetryDelay:   time.Duration(tomlConfig.UpdateRetry.RetryDelaySeconds) * time.Second,
	}

	reconcilerInstance := reconciler.NewReconciler(reconcilerCfg, params.DryRun)

	slog.Info("Initialization completed successfully")

	return &Components{
		Reconciler: reconcilerInstance,
	}, nil
}

func createMongoPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: "update"},
				bson.E{Key: "$or", Value: bson.A{
					// Watch for quarantine events (for remediation)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.StatusSucceeded, model.AlreadyDrained}},
						}},
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.Quarantined, model.AlreadyQuarantined}},
						}},
					},
					// Watch for unquarantine events (for annotation cleanup)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: model.UnQuarantined},
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: model.StatusSucceeded},
					},
				}},
			}},
		},
	}
}
