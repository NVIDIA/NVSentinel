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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type EvictMode string

const (
	ModeImmediateEvict     EvictMode = "Immediate"
	ModeAllowCompletion    EvictMode = "AllowCompletion"
	ModeDeleteAfterTimeout EvictMode = "DeleteAfterTimeout"
)

type Duration struct {
	time.Duration
}

type UserNamespace struct {
	Name string    `toml:"name"`
	Mode EvictMode `toml:"mode"`
}

type TomlConfig struct {
	EvictionTimeoutInSeconds  Duration `toml:"evictionTimeoutInSeconds"`
	SystemNamespaces          string   `toml:"systemNamespaces"`
	DeleteAfterTimeoutMinutes int      `toml:"deleteAfterTimeoutMinutes"`
	// NotReadyTimeoutMinutes is the time after which a pod in NotReady state is considered stuck
	NotReadyTimeoutMinutes int             `toml:"notReadyTimeoutMinutes"`
	UserNamespaces         []UserNamespace `toml:"userNamespaces"`
}

func (d *Duration) UnmarshalTOML(text any) error {
	if v, ok := text.(string); ok {
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid duration format: %v", text)
		}

		if seconds <= 0 {
			return fmt.Errorf("eviction timeout must be a positive integer")
		}

		d.Duration = time.Duration(seconds) * time.Second

		return nil
	}

	return fmt.Errorf("invalid duration format: %v", text)
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	return validateAndSetDefaults(&config)
}

func LoadTomlConfigFromString(configString string) (*TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.Decode(configString, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config string: %w", err)
	}

	return validateAndSetDefaults(&config)
}

func validateAndSetDefaults(config *TomlConfig) (*TomlConfig, error) {
	if config.DeleteAfterTimeoutMinutes == 0 {
		config.DeleteAfterTimeoutMinutes = 60 // Default: 60 minutes
	}

	if config.DeleteAfterTimeoutMinutes <= 0 {
		return nil, fmt.Errorf("deleteAfterTimeout must be a positive integer")
	}

	if config.NotReadyTimeoutMinutes == 0 {
		config.NotReadyTimeoutMinutes = 5 // Default: 5 minutes
	}

	if config.NotReadyTimeoutMinutes <= 0 {
		return nil, fmt.Errorf("notReadyTimeoutMinutes must be a positive integer")
	}

	return config, nil
}

type ReconcilerConfig struct {
	TomlConfig    TomlConfig
	MongoConfig   storewatcher.MongoDBConfig
	TokenConfig   storewatcher.TokenConfig
	MongoPipeline mongo.Pipeline
	StateManager  statemanager.StateManager
}

// EnvConfig holds configuration loaded from environment variables
type EnvConfig struct {
	MongoURI                  string
	MongoDatabase             string
	MongoCollection           string
	TokenDatabase             string
	TokenCollection           string
	TotalTimeoutSeconds       int
	IntervalSeconds           int
	TotalCACertTimeoutSeconds int
	IntervalCACertSeconds     int
}

// LoadEnvConfig loads and validates environment variable configuration
func LoadEnvConfig() (*EnvConfig, error) {
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		return nil, fmt.Errorf("MONGODB_URI is not provided")
	}

	mongoDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		return nil, fmt.Errorf("MONGODB_DATABASE_NAME is not provided")
	}

	mongoCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoCollection == "" {
		return nil, fmt.Errorf("MONGODB_COLLECTION_NAME is not provided")
	}

	tokenDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		return nil, fmt.Errorf("MONGODB_DATABASE_NAME is not provided")
	}

	tokenCollection := os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		return nil, fmt.Errorf("MONGODB_TOKEN_COLLECTION_NAME is not provided")
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_INTERVAL_SECONDS: %w", err)
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_READ_INTERVAL_SECONDS: %w", err)
	}

	return &EnvConfig{
		MongoURI:                  mongoURI,
		MongoDatabase:             mongoDatabase,
		MongoCollection:           mongoCollection,
		TokenDatabase:             tokenDatabase,
		TokenCollection:           tokenCollection,
		TotalTimeoutSeconds:       totalTimeoutSeconds,
		IntervalSeconds:           intervalSeconds,
		TotalCACertTimeoutSeconds: totalCACertTimeoutSeconds,
		IntervalCACertSeconds:     intervalCACertSeconds,
	}, nil
}

// getEnvAsInt retrieves an environment variable as an integer with a default value
func getEnvAsInt(name string, defaultValue int) (int, error) {
	valueStr, exists := os.LookupEnv(name)
	if !exists {
		return defaultValue, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("error converting %s to integer: %w", name, err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value of %s must be a positive integer", name)
	}

	return value, nil
}

// NewMongoConfig creates a MongoDB configuration from environment config and certificate paths
func NewMongoConfig(envConfig *EnvConfig, mongoClientCertMountPath string) storewatcher.MongoDBConfig {
	return storewatcher.MongoDBConfig{
		URI:        envConfig.MongoURI,
		Database:   envConfig.MongoDatabase,
		Collection: envConfig.MongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    envConfig.TotalTimeoutSeconds,
		TotalPingIntervalSeconds:   envConfig.IntervalSeconds,
		TotalCACertTimeoutSeconds:  envConfig.TotalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: envConfig.IntervalCACertSeconds,
	}
}

// NewTokenConfig creates a token configuration from environment config
func NewTokenConfig(envConfig *EnvConfig) storewatcher.TokenConfig {
	return storewatcher.TokenConfig{
		ClientName:      "node-draining-module",
		TokenDatabase:   envConfig.TokenDatabase,
		TokenCollection: envConfig.TokenCollection,
	}
}

// NewMongoPipeline creates the MongoDB change stream pipeline for watching quarantine events
func NewMongoPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: "update"},
				bson.E{Key: "$or", Value: bson.A{
					bson.D{bson.E{Key: "updateDescription.updatedFields",
						Value: bson.D{bson.E{Key: "healtheventstatus.nodequarantined", Value: model.Quarantined}}}},
					bson.D{bson.E{Key: "updateDescription.updatedFields",
						Value: bson.D{bson.E{Key: "healtheventstatus.nodequarantined", Value: model.AlreadyQuarantined}}}},
					bson.D{bson.E{Key: "updateDescription.updatedFields",
						Value: bson.D{bson.E{Key: "healtheventstatus.nodequarantined", Value: model.UnQuarantined}}}},
				}},
			}},
		},
	}
}
