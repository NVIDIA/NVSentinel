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

package nodemetadata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConfigFromMap(t *testing.T) {
	tests := []struct {
		name     string
		cfgMap   map[string]interface{}
		validate func(*testing.T, *Config)
	}{
		{
			name:   "default config (disabled)",
			cfgMap: map[string]interface{}{},
			validate: func(t *testing.T, cfg *Config) {
				assert.False(t, cfg.Enabled)
				assert.Equal(t, DefaultCacheSize, cfg.CacheSize)
				assert.Equal(t, DefaultCacheTTL, cfg.CacheTTL)
			},
		},
		{
			name: "enabled config",
			cfgMap: map[string]interface{}{
				"nodeMetadataAugmentationEnabled": "true",
			},
			validate: func(t *testing.T, cfg *Config) {
				assert.True(t, cfg.Enabled)
			},
		},
		{
			name: "custom cache size",
			cfgMap: map[string]interface{}{
				"nodeMetadataCacheSize": float64(500),
			},
			validate: func(t *testing.T, cfg *Config) {
				assert.Equal(t, 500, cfg.CacheSize)
			},
		},
		{
			name: "custom cache TTL",
			cfgMap: map[string]interface{}{
				"nodeMetadataCacheTTLSeconds": float64(3600),
			},
			validate: func(t *testing.T, cfg *Config) {
				assert.Equal(t, 3600*time.Second, cfg.CacheTTL)
			},
		},
		{
			name: "allowed labels",
			cfgMap: map[string]interface{}{
				"nodeMetadataAllowedLabels": []interface{}{
					"topology.kubernetes.io/zone",
					"topology.kubernetes.io/region",
				},
			},
			validate: func(t *testing.T, cfg *Config) {
				assert.Len(t, cfg.AllowedLabels, 2)
				assert.Equal(t, "topology.kubernetes.io/zone", cfg.AllowedLabels[0])
			},
		},
		{
			name: "full config",
			cfgMap: map[string]interface{}{
				"nodeMetadataAugmentationEnabled": "true",
				"nodeMetadataCacheSize":           float64(2000),
				"nodeMetadataCacheTTLSeconds":     float64(7200),
				"nodeMetadataAllowedLabels": []interface{}{
					"label1",
					"label2",
				},
			},
			validate: func(t *testing.T, cfg *Config) {
				assert.True(t, cfg.Enabled)
				assert.Equal(t, 2000, cfg.CacheSize)
				assert.Equal(t, 7200*time.Second, cfg.CacheTTL)
				assert.Len(t, cfg.AllowedLabels, 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := NewConfigFromMap(tt.cfgMap)
			require.NoError(t, err)

			tt.validate(t, cfg)
		})
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name      string
		config    *Config
		expectErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
			},
			expectErr: false,
		},
		{
			name: "invalid cache size",
			config: &Config{
				CacheSize: 0,
				CacheTTL:  1 * time.Hour,
			},
			expectErr: true,
		},
		{
			name: "invalid cache TTL",
			config: &Config{
				CacheSize: 100,
				CacheTTL:  0,
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
