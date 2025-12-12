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
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/auth"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/config"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/exporter"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/sink"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/transformer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	storeconfig "github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
	"github.com/nvidia/nvsentinel/store-client/pkg/helper"
)

type Params struct {
	ConfigPath     string
	OIDCSecretPath string
}

type Components struct {
	Exporter        *exporter.HealthEventsExporter
	DatastoreBundle *helper.DatastoreClientBundle
}

func InitializeAll(ctx context.Context, params Params) (*Components, error) {
	cfg, err := loadConfig(params.ConfigPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	tokenProvider, err := initializeOIDC(cfg, params.OIDCSecretPath)
	if err != nil {
		slog.Error("Failed to initialize OIDC", "error", err)
		return nil, fmt.Errorf("failed to initialize OIDC: %w", err)
	}

	httpSink := sink.NewHTTPSink(
		cfg.Exporter.Sink.Endpoint,
		cfg.Exporter.Sink.GetTimeout(),
		tokenProvider,
		cfg.Exporter.Sink.InsecureSkipVerify,
	)

	cloudEventsTransformer := transformer.NewCloudEventsTransformer(cfg.Exporter.Metadata)

	datastoreBundle, hasResumeToken, err := initializeDatastore(ctx)
	if err != nil {
		slog.Error("Failed to initialize datastore", "error", err)
		return nil, fmt.Errorf("failed to initialize datastore: %w", err)
	}

	exp := exporter.New(
		cfg,
		datastoreBundle.DatabaseClient,
		datastoreBundle.ChangeStreamWatcher,
		cloudEventsTransformer,
		httpSink,
		hasResumeToken,
	)

	return &Components{
		Exporter:        exp,
		DatastoreBundle: datastoreBundle,
	}, nil
}

func loadConfig(configPath string) (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		return nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	slog.Info("Configuration loaded", "path", configPath)

	return cfg, nil
}

func initializeOIDC(cfg *config.Config, secretPath string) (*auth.TokenProvider, error) {
	clientSecretBytes, err := os.ReadFile(secretPath)
	if err != nil {
		slog.Error("Failed to read OIDC client secret from file", "path", secretPath, "error", err)
		return nil, fmt.Errorf("failed to read OIDC client secret from file %s: %w", secretPath, err)
	}

	tokenProvider := auth.NewTokenProvider(
		cfg.Exporter.OIDC.TokenURL,
		cfg.Exporter.OIDC.ClientID,
		strings.TrimSpace(string(clientSecretBytes)),
		cfg.Exporter.OIDC.Scope,
		cfg.Exporter.OIDC.InsecureSkipVerify,
	)

	slog.Info("OIDC token provider initialized",
		"tokenURL", cfg.Exporter.OIDC.TokenURL,
		"clientID", cfg.Exporter.OIDC.ClientID,
		"scope", cfg.Exporter.OIDC.Scope)

	return tokenProvider, nil
}

func initializeDatastore(ctx context.Context) (*helper.DatastoreClientBundle, bool, error) {
	datastoreConfig, err := datastore.LoadDatastoreConfig()
	if err != nil {
		slog.Error("Failed to load datastore config", "error", err)
		return nil, false, fmt.Errorf("failed to load datastore config: %w", err)
	}

	builder := client.GetPipelineBuilder()
	pipeline := builder.BuildAllHealthEventInsertsPipeline()

	bundle, err := helper.NewDatastoreClientFromConfig(ctx, "event-exporter", *datastoreConfig, pipeline)
	if err != nil {
		slog.Error("Failed to create datastore client", "error", err)
		return nil, false, fmt.Errorf("failed to create datastore client: %w", err)
	}

	slog.Info("Datastore client initialized", "provider", datastoreConfig.Provider)

	hasResumeToken, err := checkResumeTokenExists(ctx)
	if err != nil {
		slog.Warn("Failed to check resume token, assuming false", "error", err)

		hasResumeToken = false
	}

	return bundle, hasResumeToken, nil
}

func checkResumeTokenExists(ctx context.Context) (bool, error) {
	tokenConfig, err := storeconfig.TokenConfigFromEnv("event-exporter")
	if err != nil {
		return false, fmt.Errorf("failed to get token config: %w", err)
	}

	slog.Info("Checking for existing resume token",
		"database", tokenConfig.TokenDatabase,
		"collection", tokenConfig.TokenCollection,
		"clientName", tokenConfig.ClientName)

	// Use dynamic cert mount path based on provider (PostgreSQL or MongoDB)
	certMountPath := storeconfig.GetCertMountPath()

	dbConfig, err := storeconfig.NewDatabaseConfigForCollectionType(
		certMountPath,
		storeconfig.CollectionTypeTokens,
	)
	if err != nil {
		return false, fmt.Errorf("failed to create token database config: %w", err)
	}

	clientFactory := factory.NewClientFactory(dbConfig)

	tokenClient, err := clientFactory.CreateDatabaseClient(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to create token client: %w", err)
	}

	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tokenClient.Close(closeCtx)
	}()

	filter := map[string]any{
		"clientName": tokenConfig.ClientName,
	}

	var tokenDoc struct {
		ResumeToken any `bson:"resumeToken"`
	}

	result, err := tokenClient.FindOne(ctx, filter, nil)
	if err != nil {
		return false, fmt.Errorf("failed to query resume token: %w", err)
	}

	if err := result.Decode(&tokenDoc); err != nil {
		if client.IsNoDocumentsError(err) {
			slog.Info("No resume token found - this appears to be first deployment")

			return false, nil
		}

		return false, fmt.Errorf("failed to decode token document: %w", err)
	}

	hasToken := tokenDoc.ResumeToken != nil
	if hasToken {
		slog.Info("Resume token exists - skipping backfill on this restart")
	}

	return hasToken, nil
}
