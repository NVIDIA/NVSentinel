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

package mongodb

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// init automatically registers the MongoDB provider with the global registry
func init() {
	slog.Info("Registering MongoDB datastore provider")
	datastore.RegisterProvider(datastore.ProviderMongoDB, NewMongoDBDataStore)

	// Register MongoDB builder factory as the default
	slog.Info("Registering MongoDB builder factory")
	client.SetBuilderFactory(NewMongoBuilderFactory())
}

// NewMongoDBDataStore creates a new MongoDB datastore instance from configuration
func NewMongoDBDataStore(ctx context.Context, config datastore.DataStoreConfig) (datastore.DataStore, error) {
	// For MongoDB, we use the existing implementation but adapt it to the new interface
	// Get certificate mount path from environment variable or TLS config
	var certMountPath *string

	slog.Info("NewMongoDBDataStore TLS config check",
		"hasTLSConfig", config.Connection.TLSConfig != nil,
		"certPath", func() string {
			if config.Connection.TLSConfig != nil {
				return config.Connection.TLSConfig.CertPath
			}

			return ""
		}(),
		"caPath", func() string {
			if config.Connection.TLSConfig != nil {
				return config.Connection.TLSConfig.CAPath
			}

			return ""
		}())

	if config.Connection.TLSConfig != nil && config.Connection.TLSConfig.CertPath != "" {
		// Extract the directory path from the cert path
		certDir := filepath.Dir(config.Connection.TLSConfig.CertPath)
		certMountPath = &certDir
		slog.Info("Extracted cert directory from TLSConfig", "certDir", certDir)
	} else if envPath := os.Getenv("MONGODB_CLIENT_CERT_MOUNT_PATH"); envPath != "" {
		certMountPath = &envPath
		slog.Info("Using MONGODB_CLIENT_CERT_MOUNT_PATH from environment", "envPath", envPath)
	} else {
		slog.Warn("No certificate path found in TLSConfig or environment")
	}

	// Create the adapted MongoDB store that implements our new DataStore interface
	return NewAdaptedMongoStore(ctx, certMountPath, config)
}
