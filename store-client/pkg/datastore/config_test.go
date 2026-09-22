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

func TestLoadDatastoreConfig_MaxConnectionsEnv_PopulatesOptions(t *testing.T) {
	tests := []struct {
		name     string
		provider DataStoreProvider
		env      string
		wantSet  bool
		want     string
	}{
		{name: "set value lands in the maxConnections option", provider: ProviderMongoDB, env: "30", wantSet: true, want: "30"},
		{name: "unset leaves the option absent", provider: ProviderPostgreSQL, env: "", wantSet: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATASTORE_PROVIDER", string(tt.provider))
			t.Setenv("DATASTORE_MAX_CONNECTIONS", tt.env)

			config, err := LoadDatastoreConfig()
			require.NoError(t, err)

			got, set := config.Options["maxConnections"]
			assert.Equal(t, tt.wantSet, set)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMaxConnections_Precedence_OptionThenEnvironmentThenZero(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		options map[string]string
		want    int
	}{
		{name: "nothing configured is zero, the provider default", env: "", options: nil, want: 0},
		{name: "empty options and no environment is zero", env: "", options: map[string]string{}, want: 0},
		{
			name: "the option wins over the environment",
			env:  "40", options: map[string]string{"maxConnections": "10"}, want: 10,
		},
		{name: "the environment is the fallback", env: "40", options: nil, want: 40},
		{
			name: "an invalid option falls through to the environment",
			env:  "40", options: map[string]string{"maxConnections": "abc"}, want: 40,
		},
		{
			name: "invalid values everywhere fall through to zero",
			env:  "-5", options: map[string]string{"maxConnections": "0"}, want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATASTORE_MAX_CONNECTIONS", tt.env)
			assert.Equal(t, tt.want, MaxConnections(tt.options))
		})
	}
}
