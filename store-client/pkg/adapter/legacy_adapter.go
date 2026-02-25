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

package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/nvsentinel/store-client/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ConvertDataStoreConfigToLegacy converts a DataStoreConfig to a legacy DatabaseConfig interface
func ConvertDataStoreConfigToLegacy(dsConfig *datastore.DataStoreConfig) config.DatabaseConfig {
	return NewLegacyDatabaseConfigAdapter(dsConfig)
}

// ConvertDataStoreConfigToLegacyWithCertPath converts a DataStoreConfig to a legacy DatabaseConfig
// interface with explicit certificate mount path override
func ConvertDataStoreConfigToLegacyWithCertPath(
	dsConfig *datastore.DataStoreConfig,
	certMountPath string,
) config.DatabaseConfig {
	return NewLegacyDatabaseConfigAdapterWithCertPath(dsConfig, certMountPath)
}

// LegacyDatabaseConfigAdapter adapts DataStoreConfig to the DatabaseConfig interface
// This provides backward compatibility for modules that still use the legacy interface
type LegacyDatabaseConfigAdapter struct {
	dsConfig      *datastore.DataStoreConfig
	certMountPath string
}

// NewLegacyDatabaseConfigAdapter creates a new legacy adapter
func NewLegacyDatabaseConfigAdapter(dsConfig *datastore.DataStoreConfig) *LegacyDatabaseConfigAdapter {
	return &LegacyDatabaseConfigAdapter{
		dsConfig: dsConfig,
	}
}

// NewLegacyDatabaseConfigAdapterWithCertPath creates a new legacy adapter with certificate mount
// path override
func NewLegacyDatabaseConfigAdapterWithCertPath(
	dsConfig *datastore.DataStoreConfig,
	certMountPath string,
) *LegacyDatabaseConfigAdapter {
	return &LegacyDatabaseConfigAdapter{
		dsConfig:      dsConfig,
		certMountPath: certMountPath,
	}
}

func (l *LegacyDatabaseConfigAdapter) GetConnectionURI() string {
	// For PostgreSQL, build a proper connection string with key=value pairs
	if l.dsConfig.Provider == datastore.ProviderPostgreSQL {
		return l.buildPostgreSQLConnectionString()
	}

	// For MongoDB, prefer MONGODB_URI from environment (for backward compatibility)
	// This matches the behavior of config.NewDatabaseConfigFromEnvWithDefaults()
	// which is used by services that go through the new datastore abstraction
	if mongoURI := os.Getenv("MONGODB_URI"); mongoURI != "" {
		return mongoURI
	}

	// Fall back to host field (which may be a full URI if loaded from YAML)
	return l.dsConfig.Connection.Host
}

func (l *LegacyDatabaseConfigAdapter) GetDatabaseName() string {
	return l.dsConfig.Connection.Database
}

func (l *LegacyDatabaseConfigAdapter) GetCollectionName() string {
	// Default collection name for health events
	return "HealthEvents"
}

// buildPostgreSQLConnectionString builds a PostgreSQL connection string from DataStoreConfig
func (l *LegacyDatabaseConfigAdapter) buildPostgreSQLConnectionString() string {
	conn := l.dsConfig.Connection

	candidates := []struct {
		key   string
		value string
	}{
		{"host", conn.Host},
		{"port", formatPort(conn.Port)},
		{"dbname", conn.Database},
		{"user", conn.Username},
		{"password", conn.Password},
		{"sslmode", conn.SSLMode},
		{"sslcert", conn.SSLCert},
		{"sslkey", conn.SSLKey},
		{"sslrootcert", conn.SSLRootCert},
	}

	var params []string

	for _, c := range candidates {
		if c.value != "" {
			params = append(params, c.key+"="+c.value)
		}
	}

	return strings.Join(params, " ")
}

func formatPort(port int) string {
	if port > 0 {
		return fmt.Sprintf("%d", port)
	}

	return ""
}

func (l *LegacyDatabaseConfigAdapter) GetCertConfig() config.CertificateConfig {
	return &LegacyCertConfigAdapter{
		dsConfig:      l.dsConfig,
		certMountPath: l.certMountPath,
	}
}

func (l *LegacyDatabaseConfigAdapter) GetTimeoutConfig() config.TimeoutConfig {
	return &LegacyTimeoutConfigAdapter{}
}

func (l *LegacyDatabaseConfigAdapter) GetAppName() string {
	return os.Getenv("APP_NAME")
}

// LegacyCertConfigAdapter adapts DataStoreConfig certificate configuration
type LegacyCertConfigAdapter struct {
	dsConfig      *datastore.DataStoreConfig
	certMountPath string
}

// getCertPath checks if the certificate exists at the new path, falls back to legacy path
func (l *LegacyCertConfigAdapter) getCertPath() string {
	// If a custom cert mount path is specified, use it
	if l.certMountPath != "" {
		return l.certMountPath
	}

	// If SSL paths are set in the config, prefer those
	if l.dsConfig.Connection.SSLCert != "" {
		return filepath.Dir(l.dsConfig.Connection.SSLCert)
	}

	// Check if ca.crt exists at the legacy mongo-client path first (most common)
	legacyPath := "/etc/ssl/mongo-client"
	if _, err := os.Stat(legacyPath + "/ca.crt"); err == nil {
		return legacyPath
	}

	// Fall back to new database-client path
	newPath := "/etc/ssl/database-client"
	if _, err := os.Stat(newPath + "/ca.crt"); err == nil {
		return newPath
	}

	// If neither exists, return the legacy path (most likely to be mounted)
	return legacyPath
}

func (l *LegacyCertConfigAdapter) GetCertPath() string {
	// Always use getCertPath() logic if certMountPath is provided
	if l.certMountPath != "" {
		return filepath.Join(l.getCertPath(), "tls.crt")
	}

	if l.dsConfig.Connection.SSLCert != "" {
		return l.dsConfig.Connection.SSLCert
	}

	return filepath.Join(l.getCertPath(), "tls.crt")
}

func (l *LegacyCertConfigAdapter) GetKeyPath() string {
	// Always use getCertPath() logic if certMountPath is provided
	if l.certMountPath != "" {
		return filepath.Join(l.getCertPath(), "tls.key")
	}

	if l.dsConfig.Connection.SSLKey != "" {
		return l.dsConfig.Connection.SSLKey
	}

	return filepath.Join(l.getCertPath(), "tls.key")
}

func (l *LegacyCertConfigAdapter) GetCACertPath() string {
	// Always use getCertPath() logic if certMountPath is provided
	if l.certMountPath != "" {
		return filepath.Join(l.getCertPath(), "ca.crt")
	}

	if l.dsConfig.Connection.SSLRootCert != "" {
		return l.dsConfig.Connection.SSLRootCert
	}

	return filepath.Join(l.getCertPath(), "ca.crt")
}

// LegacyTimeoutConfigAdapter provides default timeout configuration
type LegacyTimeoutConfigAdapter struct{}

func (l *LegacyTimeoutConfigAdapter) GetPingTimeoutSeconds() int {
	return 30
}

func (l *LegacyTimeoutConfigAdapter) GetPingIntervalSeconds() int {
	return 5
}

func (l *LegacyTimeoutConfigAdapter) GetCACertTimeoutSeconds() int {
	return 360
}

func (l *LegacyTimeoutConfigAdapter) GetCACertIntervalSeconds() int {
	return 5
}

func (l *LegacyTimeoutConfigAdapter) GetChangeStreamRetryDeadlineSeconds() int {
	return 300
}

func (l *LegacyTimeoutConfigAdapter) GetChangeStreamRetryIntervalSeconds() int {
	return 10
}
