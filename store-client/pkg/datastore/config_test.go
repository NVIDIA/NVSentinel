// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDatastoreConfigPoolOptions(t *testing.T) {
	t.Run("max connections environment variable populates Options", func(t *testing.T) {
		t.Setenv("DATASTORE_PROVIDER", string(ProviderMongoDB))
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "30")

		config, err := LoadDatastoreConfig()
		require.NoError(t, err)
		assert.Equal(t, "30", config.Options["maxConnections"])
	})

	t.Run("unset max connections leaves Options empty", func(t *testing.T) {
		t.Setenv("DATASTORE_PROVIDER", string(ProviderPostgreSQL))
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "")

		config, err := LoadDatastoreConfig()
		require.NoError(t, err)
		assert.NotContains(t, config.Options, "maxConnections")
	})
}

func TestMaxConnections(t *testing.T) {
	t.Run("nothing configured means the provider default", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "")
		assert.Equal(t, 0, MaxConnections(nil))
		assert.Equal(t, 0, MaxConnections(map[string]string{}))
	})

	t.Run("the option wins over the environment", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "40")
		assert.Equal(t, 10, MaxConnections(map[string]string{"maxConnections": "10"}))
	})

	t.Run("the environment is the fallback", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "40")
		assert.Equal(t, 40, MaxConnections(nil))
	})

	t.Run("invalid values fall through", func(t *testing.T) {
		t.Setenv("DATASTORE_MAX_CONNECTIONS", "40")
		assert.Equal(t, 40, MaxConnections(map[string]string{"maxConnections": "abc"}))

		t.Setenv("DATASTORE_MAX_CONNECTIONS", "-5")
		assert.Equal(t, 0, MaxConnections(map[string]string{"maxConnections": "0"}))
	})
}
