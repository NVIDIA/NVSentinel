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

package postgresql

import (
	"context"
	"log/slog"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// init automatically registers the PostgreSQL provider with the global registry
func init() {
	slog.Info("Registering PostgreSQL datastore provider")
	datastore.RegisterProvider(datastore.ProviderPostgreSQL, NewPostgreSQLDataStore)
}

// NewPostgreSQLDataStore creates a new PostgreSQL datastore instance from configuration
func NewPostgreSQLDataStore(ctx context.Context, config datastore.DataStoreConfig) (datastore.DataStore, error) {
	// Create the PostgreSQL store that implements the DataStore interface
	return NewPostgreSQLStore(ctx, config)
}
