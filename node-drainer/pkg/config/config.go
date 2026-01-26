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
	"strconv"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/config"
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

type CustomDrainConfig struct {
	Enabled               bool     `toml:"enabled"`
	TemplateMountPath     string   `toml:"templateMountPath"`
	TemplateFileName      string   `toml:"templateFileName"`
	Namespace             string   `toml:"namespace"`
	Timeout               Duration `toml:"timeout"`
	ApiGroup              string   `toml:"apiGroup"`
	Version               string   `toml:"version"`
	Kind                  string   `toml:"kind"`
	StatusConditionType   string   `toml:"statusConditionType"`
	StatusConditionStatus string   `toml:"statusConditionStatus"`
}

type TomlConfig struct {
	EvictionTimeoutInSeconds  Duration `toml:"evictionTimeoutInSeconds"`
	SystemNamespaces          string   `toml:"systemNamespaces"`
	DeleteAfterTimeoutMinutes int      `toml:"deleteAfterTimeoutMinutes"`
	// NotReadyTimeoutMinutes is the time after which a pod in NotReady state is considered stuck
	NotReadyTimeoutMinutes int               `toml:"notReadyTimeoutMinutes"`
	UserNamespaces         []UserNamespace   `toml:"userNamespaces"`
	CustomDrain            CustomDrainConfig `toml:"customDrain"`
	PartialDrainEnabled    bool              `toml:"partialDrainEnabled"`
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

func validateCustomDrainConfig(config *TomlConfig) error {
	if !config.CustomDrain.Enabled {
		return nil
	}

	if len(config.UserNamespaces) > 0 {
		return fmt.Errorf("cannot use both customDrain.enabled=true and userNamespaces configuration")
	}

	requiredFields := map[string]string{
		"templateMountPath":     config.CustomDrain.TemplateMountPath,
		"templateFileName":      config.CustomDrain.TemplateFileName,
		"namespace":             config.CustomDrain.Namespace,
		"apiGroup":              config.CustomDrain.ApiGroup,
		"version":               config.CustomDrain.Version,
		"kind":                  config.CustomDrain.Kind,
		"statusConditionType":   config.CustomDrain.StatusConditionType,
		"statusConditionStatus": config.CustomDrain.StatusConditionStatus,
	}

	for field, value := range requiredFields {
		if value == "" {
			return fmt.Errorf("customDrain.%s is required when customDrain.enabled=true", field)
		}
	}

	if config.CustomDrain.Timeout.Duration == 0 {
		config.CustomDrain.Timeout.Duration = 3600 * time.Second
	}

	return nil
}

func validateAndSetDefaults(config *TomlConfig) (*TomlConfig, error) {
	if err := validateCustomDrainConfig(config); err != nil {
		return nil, err
	}

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
	TomlConfig     TomlConfig
	DatabaseConfig config.DatabaseConfig
	TokenConfig    client.TokenConfig
	StateManager   statemanager.StateManager
}

// EnvConfig holds configuration loaded from environment variables
type EnvConfig struct {
	DatabaseURI               string
	DatabaseName              string
	DatabaseCollection        string
	TokenDatabase             string
	TokenCollection           string
	TotalTimeoutSeconds       int
	IntervalSeconds           int
	TotalCACertTimeoutSeconds int
	IntervalCACertSeconds     int
}

// LoadEnvConfig loads and validates environment variable configuration using centralized store-client
func LoadEnvConfig() (*EnvConfig, error) {
	// Use centralized configuration from store-client
	databaseConfig, err := config.NewDatabaseConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load database configuration: %w", err)
	}

	// Load token configuration using centralized function
	tokenConfig, err := config.TokenConfigFromEnv("node-drainer")
	if err != nil {
		return nil, fmt.Errorf("failed to load token configuration: %w", err)
	}

	// Get timeout configuration
	timeoutConfig := databaseConfig.GetTimeoutConfig()

	return &EnvConfig{
		DatabaseURI:               databaseConfig.GetConnectionURI(),
		DatabaseName:              databaseConfig.GetDatabaseName(),
		DatabaseCollection:        databaseConfig.GetCollectionName(),
		TokenDatabase:             tokenConfig.TokenDatabase,
		TokenCollection:           tokenConfig.TokenCollection,
		TotalTimeoutSeconds:       timeoutConfig.GetPingTimeoutSeconds(),
		IntervalSeconds:           timeoutConfig.GetPingIntervalSeconds(),
		TotalCACertTimeoutSeconds: timeoutConfig.GetCACertTimeoutSeconds(),
		IntervalCACertSeconds:     timeoutConfig.GetCACertIntervalSeconds(),
	}, nil
}

// NewDatabaseConfig creates a database configuration from environment config and certificate paths
func NewDatabaseConfig(databaseClientCertMountPath string) (config.DatabaseConfig, error) {
	if databaseClientCertMountPath != "" {
		return config.NewDatabaseConfigFromEnvWithDefaults(databaseClientCertMountPath)
	}

	return config.NewDatabaseConfigFromEnv()
}

// NewTokenConfig is DEPRECATED and should not be used.
// This function hardcoded ClientName="node-draining-module", which caused resume token
// lookup failures because LoadEnvConfig uses ClientName="node-drainer".
// Instead, use config.TokenConfigFromEnv("node-drainer") directly like other modules.
//
// Deprecated: Use config.TokenConfigFromEnv("node-drainer") instead.
func NewTokenConfig(envConfig *EnvConfig) client.TokenConfig {
	// Return config with the CORRECT ClientName to match what's used for token storage
	return client.TokenConfig{
		ClientName:      "node-drainer", // Fixed: was "node-draining-module"
		TokenDatabase:   envConfig.TokenDatabase,
		TokenCollection: envConfig.TokenCollection,
	}
}

// NewQuarantinePipeline creates the database change stream pipeline for watching quarantine events
// This uses the provider-specific pipeline builder for optimal performance with both MongoDB and PostgreSQL
func NewQuarantinePipeline() interface{} {
	builder := client.GetPipelineBuilder()
	return builder.BuildNodeQuarantineStatusPipeline()
}
