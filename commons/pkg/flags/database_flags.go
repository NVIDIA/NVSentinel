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

package flags

import (
	"flag"
	"log/slog"
	"os"
)

// DatabaseCertConfig holds the configuration for database client certificate paths
type DatabaseCertConfig struct {
	DatabaseClientCertMountPath string
	LegacyMongoCertPath         string
	ResolvedCertPath            string
}

// RegisterDatabaseCertFlags registers both the new and legacy database certificate flags
// for backward compatibility. This centralizes the flag parsing logic that was duplicated
// across multiple modules.
func RegisterDatabaseCertFlags() *DatabaseCertConfig {
	config := &DatabaseCertConfig{}

	flag.StringVar(&config.DatabaseClientCertMountPath, "database-client-cert-mount-path", "/etc/ssl/database-client",
		"path where the database client cert is mounted")

	// Support legacy flag name for backward compatibility
	flag.StringVar(&config.LegacyMongoCertPath, "mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongo client cert is mounted (legacy flag, use database-client-cert-mount-path)")

	return config
}

// ResolveCertPath resolves the actual certificate path to use based on flag values and file existence.
// This implements the backward compatibility logic that was duplicated across multiple modules.
func (c *DatabaseCertConfig) ResolveCertPath() string {
	// If new flag is still default and legacy flag is provided, use legacy flag value
	if c.DatabaseClientCertMountPath == "/etc/ssl/database-client" {
		c.ResolvedCertPath = c.LegacyMongoCertPath
		slog.Info("Using legacy certificate path for backward compatibility",
			"resolved_path", c.ResolvedCertPath,
			"database_flag", c.DatabaseClientCertMountPath,
			"legacy_flag", c.LegacyMongoCertPath)
	} else {
		c.ResolvedCertPath = c.DatabaseClientCertMountPath
		slog.Info("Using new certificate path",
			"resolved_path", c.ResolvedCertPath)
	}

	return c.ResolvedCertPath
}

// GetCertPath checks if the certificate exists at the resolved path, with fallback logic
func (c *DatabaseCertConfig) GetCertPath() string {
	if c.ResolvedCertPath == "" {
		c.ResolveCertPath()
	}

	// Check if ca.crt exists at the resolved path
	if _, err := os.Stat(c.ResolvedCertPath + "/ca.crt"); err == nil {
		return c.ResolvedCertPath
	}

	// Fall back to legacy mongo-client path
	legacyPath := "/etc/ssl/mongo-client"
	if _, err := os.Stat(legacyPath + "/ca.crt"); err == nil {
		slog.Info("Fallback to legacy certificate path", "path", legacyPath)
		return legacyPath
	}

	// Fall back to new database-client path
	newPath := "/etc/ssl/database-client"
	if _, err := os.Stat(newPath + "/ca.crt"); err == nil {
		slog.Info("Fallback to new certificate path", "path", newPath)
		return newPath
	}

	// If neither exists, return the resolved path (original behavior)
	slog.Warn("Certificate file not found at any expected location, using resolved path",
		"resolved_path", c.ResolvedCertPath)

	return c.ResolvedCertPath
}
