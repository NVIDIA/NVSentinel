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
)

// DatabaseConfig represents database-agnostic configuration
// This interface allows for different database implementations in the future
type DatabaseConfig interface {
	GetConnectionURI() string
	GetDatabaseName() string
	GetCollectionName() string
	GetCertConfig() CertificateConfig
	GetTimeoutConfig() TimeoutConfig
}

// CertificateConfig holds TLS certificate configuration
type CertificateConfig interface {
	GetCertPath() string
	GetKeyPath() string
	GetCACertPath() string
}

// TimeoutConfig holds timeout-related configuration
type TimeoutConfig interface {
	GetPingTimeoutSeconds() int
	GetPingIntervalSeconds() int
	GetCACertTimeoutSeconds() int
	GetCACertIntervalSeconds() int
	GetChangeStreamRetryDeadlineSeconds() int
	GetChangeStreamRetryIntervalSeconds() int
}

// Standard environment variable names used across all modules
const (
	EnvMongoDBURI                            = "MONGODB_URI"
	EnvMongoDBDatabaseName                   = "MONGODB_DATABASE_NAME"
	EnvMongoDBCollectionName                 = "MONGODB_COLLECTION_NAME"
	EnvMongoDBTokenCollectionName            = "MONGODB_TOKEN_COLLECTION_NAME" // nolint:gosec
	EnvMongoDBMaintenanceEventCollectionName = "MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME"
	EnvMongoDBPingTimeoutTotalSeconds        = "MONGODB_PING_TIMEOUT_TOTAL_SECONDS"
	EnvMongoDBPingIntervalSeconds            = "MONGODB_PING_INTERVAL_SECONDS"
	EnvCACertMountTimeoutTotalSeconds        = "CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS"
	EnvCACertReadIntervalSeconds             = "CA_CERT_READ_INTERVAL_SECONDS"
	// New: MONGODB_ prefixed versions (preferred)
	EnvMongoDBChangeStreamRetryDeadlineSeconds = "MONGODB_CHANGE_STREAM_RETRY_DEADLINE_SECONDS"
	EnvMongoDBChangeStreamRetryIntervalSeconds = "MONGODB_CHANGE_STREAM_RETRY_INTERVAL_SECONDS"
	// Legacy: Non-prefixed versions (for backward compatibility)
	EnvChangeStreamRetryDeadlineSeconds = "CHANGE_STREAM_RETRY_DEADLINE_SECONDS"
	EnvChangeStreamRetryIntervalSeconds = "CHANGE_STREAM_RETRY_INTERVAL_SECONDS"
)

// Default values that match existing module defaults for backward compatibility
const (
	DefaultPingTimeoutSeconds        = 300 // 5 minutes
	DefaultPingIntervalSeconds       = 5   // 5 seconds
	DefaultCACertTimeoutSeconds      = 360 // 6 minutes (matches existing modules)
	DefaultCACertIntervalSeconds     = 5   // 5 seconds
	DefaultChangeStreamRetryDeadline = 60  // 1 minute
	DefaultChangeStreamRetryInterval = 3   // 3 seconds
	DefaultCertMountPath             = "/etc/ssl/mongo-client"
)

// StandardDatabaseConfig implements DatabaseConfig for MongoDB with standard environment variables
type StandardDatabaseConfig struct {
	connectionURI  string
	databaseName   string
	collectionName string
	certConfig     CertificateConfig
	timeoutConfig  TimeoutConfig
}

// StandardCertificateConfig implements CertificateConfig
type StandardCertificateConfig struct {
	certPath   string
	keyPath    string
	caCertPath string
}

// StandardTimeoutConfig implements TimeoutConfig
type StandardTimeoutConfig struct {
	pingTimeoutSeconds               int
	pingIntervalSeconds              int
	caCertTimeoutSeconds             int
	caCertIntervalSeconds            int
	changeStreamRetryDeadlineSeconds int
	changeStreamRetryIntervalSeconds int
}

// Collection type constants for database-agnostic collection specification
const (
	CollectionTypeHealthEvents      = "health-events"
	CollectionTypeMaintenanceEvents = "maintenance-events"
	CollectionTypeTokens            = "tokens"
)

// NewDatabaseConfigFromEnv loads database configuration from environment variables
// This consolidates the duplicated configuration loading from all modules
func NewDatabaseConfigFromEnv() (DatabaseConfig, error) {
	return NewDatabaseConfigFromEnvWithDefaults(DefaultCertMountPath)
}

// NewDatabaseConfigFromEnvWithDefaults allows custom certificate mount path
func NewDatabaseConfigFromEnvWithDefaults(certMountPath string) (DatabaseConfig, error) {
	return NewDatabaseConfigWithCollection(certMountPath, "", "")
}

// NewDatabaseConfigForCollectionType creates database configuration for a specific collection type
// This provides database abstraction by mapping logical collection types to provider-specific configuration
func NewDatabaseConfigForCollectionType(certMountPath, collectionType string) (DatabaseConfig, error) {
	// Map logical collection types to MongoDB-specific environment variables and defaults
	// This encapsulates MongoDB-specific knowledge within the store-client
	var envVar, defaultCollection string

	switch collectionType {
	case CollectionTypeHealthEvents:
		envVar = EnvMongoDBCollectionName
		defaultCollection = "HealthEvents"
	case CollectionTypeMaintenanceEvents:
		envVar = EnvMongoDBMaintenanceEventCollectionName
		defaultCollection = "MaintenanceEvents"
	case CollectionTypeTokens:
		envVar = EnvMongoDBTokenCollectionName
		defaultCollection = "ResumeTokens"
	default:
		// For unknown types, use the standard collection environment variable
		envVar = EnvMongoDBCollectionName
		defaultCollection = ""
	}

	return NewDatabaseConfigWithCollection(certMountPath, envVar, defaultCollection)
}

// NewDatabaseConfigWithCollection allows custom certificate mount path and collection configuration
// If collectionEnvVar is provided, it will be used instead of the default MONGODB_COLLECTION_NAME
// If defaultCollection is provided, it will be used as fallback if the environment variable is not set
func NewDatabaseConfigWithCollection(
	certMountPath, collectionEnvVar, defaultCollection string,
) (DatabaseConfig, error) {
	// Load required environment variables
	connectionURI := os.Getenv(EnvMongoDBURI)
	if connectionURI == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", EnvMongoDBURI)
	}

	databaseName := os.Getenv(EnvMongoDBDatabaseName)
	if databaseName == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", EnvMongoDBDatabaseName)
	}

	// Determine which collection environment variable to use
	collectionEnvName := EnvMongoDBCollectionName
	if collectionEnvVar != "" {
		collectionEnvName = collectionEnvVar
	}

	collectionName := os.Getenv(collectionEnvName)
	if collectionName == "" {
		if defaultCollection != "" {
			collectionName = defaultCollection
		} else {
			return nil, fmt.Errorf("required environment variable %s is not set", collectionEnvName)
		}
	}

	// Load timeout configuration with defaults
	timeoutConfig, err := loadTimeoutConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load timeout configuration: %w", err)
	}

	// Build certificate configuration
	certConfig := &StandardCertificateConfig{
		certPath:   filepath.Join(certMountPath, "tls.crt"),
		keyPath:    filepath.Join(certMountPath, "tls.key"),
		caCertPath: filepath.Join(certMountPath, "ca.crt"),
	}

	return &StandardDatabaseConfig{
		connectionURI:  connectionURI,
		databaseName:   databaseName,
		collectionName: collectionName,
		certConfig:     certConfig,
		timeoutConfig:  timeoutConfig,
	}, nil
}

// loadTimeoutConfigFromEnv loads timeout configuration from environment variables
func loadTimeoutConfigFromEnv() (TimeoutConfig, error) {
	pingTimeout, err := getPositiveIntEnvVar(EnvMongoDBPingTimeoutTotalSeconds, DefaultPingTimeoutSeconds)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", EnvMongoDBPingTimeoutTotalSeconds, err)
	}

	pingInterval, err := getPositiveIntEnvVar(EnvMongoDBPingIntervalSeconds, DefaultPingIntervalSeconds)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", EnvMongoDBPingIntervalSeconds, err)
	}

	caCertTimeout, err := getPositiveIntEnvVar(EnvCACertMountTimeoutTotalSeconds, DefaultCACertTimeoutSeconds)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", EnvCACertMountTimeoutTotalSeconds, err)
	}

	caCertInterval, err := getPositiveIntEnvVar(EnvCACertReadIntervalSeconds, DefaultCACertIntervalSeconds)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", EnvCACertReadIntervalSeconds, err)
	}

	// Check both new (MONGODB_ prefixed) and legacy (non-prefixed) variable names for backward compatibility
	changeStreamRetryDeadline, err := getPositiveIntEnvVarWithFallback(
		EnvMongoDBChangeStreamRetryDeadlineSeconds,
		EnvChangeStreamRetryDeadlineSeconds,
		DefaultChangeStreamRetryDeadline)
	if err != nil {
		return nil, fmt.Errorf("invalid change stream retry deadline: %w", err)
	}

	changeStreamRetryInterval, err := getPositiveIntEnvVarWithFallback(
		EnvMongoDBChangeStreamRetryIntervalSeconds,
		EnvChangeStreamRetryIntervalSeconds,
		DefaultChangeStreamRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid change stream retry interval: %w", err)
	}

	// Validation: ping interval must be less than ping timeout
	if pingInterval >= pingTimeout {
		return nil, fmt.Errorf("ping interval (%d) must be less than ping timeout (%d)", pingInterval, pingTimeout)
	}

	return &StandardTimeoutConfig{
		pingTimeoutSeconds:               pingTimeout,
		pingIntervalSeconds:              pingInterval,
		caCertTimeoutSeconds:             caCertTimeout,
		caCertIntervalSeconds:            caCertInterval,
		changeStreamRetryDeadlineSeconds: changeStreamRetryDeadline,
		changeStreamRetryIntervalSeconds: changeStreamRetryInterval,
	}, nil
}

// getPositiveIntEnvVar reads an integer environment variable with a default value
// This consolidates the duplicated getEnvAsInt functions from all modules
func getPositiveIntEnvVar(name string, defaultValue int) (int, error) {
	valueStr := os.Getenv(name)
	if valueStr == "" {
		return defaultValue, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("error converting %s to integer: %w", name, err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value of %s must be a positive integer, got %d", name, value)
	}

	return value, nil
}

// getPositiveIntEnvVarWithFallback reads an integer environment variable with backward compatibility
// It first checks the preferred variable name, then falls back to the legacy name if not found
func getPositiveIntEnvVarWithFallback(preferredName, legacyName string, defaultValue int) (int, error) {
	// Try the preferred name first
	valueStr := os.Getenv(preferredName)
	if valueStr != "" {
		value, err := strconv.Atoi(valueStr)
		if err != nil {
			return 0, fmt.Errorf("error converting %s to integer: %w", preferredName, err)
		}

		if value <= 0 {
			return 0, fmt.Errorf("value of %s must be a positive integer, got %d", preferredName, value)
		}

		return value, nil
	}

	// Fall back to legacy name for backward compatibility
	valueStr = os.Getenv(legacyName)
	if valueStr != "" {
		value, err := strconv.Atoi(valueStr)
		if err != nil {
			return 0, fmt.Errorf("error converting %s to integer: %w", legacyName, err)
		}

		if value <= 0 {
			return 0, fmt.Errorf("value of %s must be a positive integer, got %d", legacyName, value)
		}

		return value, nil
	}

	// Neither found, use default
	return defaultValue, nil
}

// TokenConfigFromEnv creates token configuration from environment variables
func TokenConfigFromEnv(clientName string) (TokenConfig, error) {
	tokenDatabase := os.Getenv(EnvMongoDBDatabaseName)
	if tokenDatabase == "" {
		return TokenConfig{}, fmt.Errorf("required environment variable %s is not set", EnvMongoDBDatabaseName)
	}

	tokenCollection := os.Getenv(EnvMongoDBTokenCollectionName)
	if tokenCollection == "" {
		return TokenConfig{}, fmt.Errorf("required environment variable %s is not set", EnvMongoDBTokenCollectionName)
	}

	return TokenConfig{
		ClientName:      clientName,
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}, nil
}

// TokenConfig holds resume token configuration
type TokenConfig struct {
	ClientName      string
	TokenDatabase   string
	TokenCollection string
}

// Interface method implementations

func (c *StandardDatabaseConfig) GetConnectionURI() string {
	return c.connectionURI
}

func (c *StandardDatabaseConfig) GetDatabaseName() string {
	return c.databaseName
}

func (c *StandardDatabaseConfig) GetCollectionName() string {
	return c.collectionName
}

func (c *StandardDatabaseConfig) GetCertConfig() CertificateConfig {
	return c.certConfig
}

func (c *StandardDatabaseConfig) GetTimeoutConfig() TimeoutConfig {
	return c.timeoutConfig
}

func (c *StandardCertificateConfig) GetCertPath() string {
	return c.certPath
}

func (c *StandardCertificateConfig) GetKeyPath() string {
	return c.keyPath
}

func (c *StandardCertificateConfig) GetCACertPath() string {
	return c.caCertPath
}

func (c *StandardTimeoutConfig) GetPingTimeoutSeconds() int {
	return c.pingTimeoutSeconds
}

func (c *StandardTimeoutConfig) GetPingIntervalSeconds() int {
	return c.pingIntervalSeconds
}

func (c *StandardTimeoutConfig) GetCACertTimeoutSeconds() int {
	return c.caCertTimeoutSeconds
}

func (c *StandardTimeoutConfig) GetCACertIntervalSeconds() int {
	return c.caCertIntervalSeconds
}

func (c *StandardTimeoutConfig) GetChangeStreamRetryDeadlineSeconds() int {
	return c.changeStreamRetryDeadlineSeconds
}

func (c *StandardTimeoutConfig) GetChangeStreamRetryIntervalSeconds() int {
	return c.changeStreamRetryIntervalSeconds
}
