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
				if cfg.Enabled {
					t.Error("expected Enabled to be false")
				}
				if cfg.CacheSize != DefaultCacheSize {
					t.Errorf("expected CacheSize %d, got %d", DefaultCacheSize, cfg.CacheSize)
				}
				if cfg.CacheTTL != DefaultCacheTTL {
					t.Errorf("expected CacheTTL %v, got %v", DefaultCacheTTL, cfg.CacheTTL)
				}
			},
		},
		{
			name: "enabled config",
			cfgMap: map[string]interface{}{
				"nodeMetadataAugmentationEnabled": "true",
			},
			validate: func(t *testing.T, cfg *Config) {
				if !cfg.Enabled {
					t.Error("expected Enabled to be true")
				}
			},
		},
		{
			name: "custom cache size",
			cfgMap: map[string]interface{}{
				"nodeMetadataCacheSize": float64(500),
			},
			validate: func(t *testing.T, cfg *Config) {
				if cfg.CacheSize != 500 {
					t.Errorf("expected CacheSize 500, got %d", cfg.CacheSize)
				}
			},
		},
		{
			name: "custom cache TTL",
			cfgMap: map[string]interface{}{
				"nodeMetadataCacheTTLSeconds": float64(3600),
			},
			validate: func(t *testing.T, cfg *Config) {
				expected := 3600 * time.Second
				if cfg.CacheTTL != expected {
					t.Errorf("expected CacheTTL %v, got %v", expected, cfg.CacheTTL)
				}
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
				if len(cfg.AllowedLabels) != 2 {
					t.Errorf("expected 2 allowed labels, got %d", len(cfg.AllowedLabels))
				}
				if cfg.AllowedLabels[0] != "topology.kubernetes.io/zone" {
					t.Errorf("unexpected allowed label: %s", cfg.AllowedLabels[0])
				}
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
				if !cfg.Enabled {
					t.Error("expected Enabled to be true")
				}
				if cfg.CacheSize != 2000 {
					t.Errorf("expected CacheSize 2000, got %d", cfg.CacheSize)
				}
				if cfg.CacheTTL != 7200*time.Second {
					t.Errorf("expected CacheTTL 7200s, got %v", cfg.CacheTTL)
				}
				if len(cfg.AllowedLabels) != 2 {
					t.Errorf("expected 2 allowed labels, got %d", len(cfg.AllowedLabels))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := NewConfigFromMap(tt.cfgMap)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

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
			if tt.expectErr && err == nil {
				t.Error("expected validation error")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

