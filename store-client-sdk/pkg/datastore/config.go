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

package datastore

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
	"log/slog"
)

// LoadDatastoreConfig loads datastore configuration from environment variables and YAML
func LoadDatastoreConfig() (*DataStoreConfig, error) {
	provider := os.Getenv("DATASTORE_PROVIDER")
	slog.Info("Loading datastore config", "provider", provider)

	if provider != "" {
		return loadConfigFromEnv(provider)
	}

	if yamlConfigStr := os.Getenv("DATASTORE_YAML"); yamlConfigStr != "" {
		return loadConfigFromYAMLString(yamlConfigStr)
	}

	if yamlPath := os.Getenv("DATASTORE_YAML_PATH"); yamlPath != "" {
		return loadDatastoreConfigFromYAMLFile(yamlPath)
	}

	return loadDefaultConfig(), nil
}

// loadConfigFromEnv loads configuration from environment variables
func loadConfigFromEnv(provider string) (*DataStoreConfig, error) {
	config := &DataStoreConfig{
		Provider: DataStoreProvider(provider),
	}

	loadConnectionConfigFromEnv(config)
	setDefaultPorts(config)
	loadOptionsFromEnv(config)

	return config, nil
}

// loadConnectionConfigFromEnv loads connection configuration from environment
func loadConnectionConfigFromEnv(config *DataStoreConfig) {
	config.Connection = ConnectionConfig{
		Host:        getEnvWithDefault("DATASTORE_HOST", "localhost"),
		Port:        0, // Will be set based on provider
		Database:    getEnvWithDefault("DATASTORE_DATABASE", "nvsentinel"),
		Username:    os.Getenv("DATASTORE_USERNAME"),
		Password:    os.Getenv("DATASTORE_PASSWORD"),
		SSLMode:     os.Getenv("DATASTORE_SSLMODE"),
		SSLCert:     os.Getenv("DATASTORE_SSLCERT"),
		SSLKey:      os.Getenv("DATASTORE_SSLKEY"),
		SSLRootCert: os.Getenv("DATASTORE_SSLROOTCERT"),
	}
}

// setDefaultPorts sets default ports based on provider
func setDefaultPorts(config *DataStoreConfig) {
	switch config.Provider {
	case ProviderMongoDB:
		setMongoDBDefaults(config)
	case ProviderPostgreSQL:
		config.Connection.Port = getEnvIntWithDefault("DATASTORE_PORT", 5432)
	default:
		setGenericPort(config)
	}
}

// setMongoDBDefaults sets MongoDB-specific defaults
func setMongoDBDefaults(config *DataStoreConfig) {
	config.Connection.Port = getEnvIntWithDefault("DATASTORE_PORT", 27017)

	// For MongoDB, use existing environment variables for backward compatibility
	if config.Connection.Host == "localhost" {
		if mongoURI := os.Getenv("MONGODB_URI"); mongoURI != "" {
			config.Connection.Host = mongoURI
		}
	}

	if config.Connection.Database == "nvsentinel" {
		if mongoDB := os.Getenv("MONGODB_DATABASE_NAME"); mongoDB != "" {
			config.Connection.Database = mongoDB
		}
	}
}

// setGenericPort sets port for unknown providers
func setGenericPort(config *DataStoreConfig) {
	if portStr := os.Getenv("DATASTORE_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			config.Connection.Port = port
		}
	}
}

// loadOptionsFromEnv loads options from environment variables
func loadOptionsFromEnv(config *DataStoreConfig) {
	config.Options = make(map[string]string)

	if maxConns := os.Getenv("DATASTORE_MAX_CONNECTIONS"); maxConns != "" {
		config.Options["maxConnections"] = maxConns
	}

	if maxIdle := os.Getenv("DATASTORE_MAX_IDLE_CONNECTIONS"); maxIdle != "" {
		config.Options["maxIdleConnections"] = maxIdle
	}
}

// loadConfigFromYAMLString loads configuration from YAML string
func loadConfigFromYAMLString(yamlConfigStr string) (*DataStoreConfig, error) {
	var config DataStoreConfig
	if err := yaml.Unmarshal([]byte(yamlConfigStr), &config); err != nil {
		return nil, fmt.Errorf("failed to parse datastore YAML config: %w", err)
	}

	return &config, nil
}

// loadDefaultConfig loads default MongoDB configuration for backward compatibility
func loadDefaultConfig() *DataStoreConfig {
	return &DataStoreConfig{
		Provider: ProviderMongoDB,
		Connection: ConnectionConfig{
			Host:     getEnvWithDefault("MONGODB_URI", "mongodb://localhost:27017"),
			Database: getEnvWithDefault("MONGODB_DATABASE_NAME", "nvsentinel"),
		},
		Options: make(map[string]string),
	}
}

// loadDatastoreConfigFromYAMLFile loads configuration from a YAML file
func loadDatastoreConfigFromYAMLFile(yamlPath string) (*DataStoreConfig, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read datastore config file %s: %w", yamlPath, err)
	}

	var config DataStoreConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse datastore YAML config from file %s: %w", yamlPath, err)
	}

	return &config, nil
}

// getEnvWithDefault returns environment variable value or default
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}

// getEnvIntWithDefault returns environment variable as int or default
func getEnvIntWithDefault(key string, defaultValue int) int {
	if valueStr := os.Getenv(key); valueStr != "" {
		if value, err := strconv.Atoi(valueStr); err == nil {
			return value
		}
	}

	return defaultValue
}
