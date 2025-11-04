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
	"fmt"
	"time"
)

const (
	DefaultCacheSize = 50
	DefaultCacheTTL  = 1 * time.Hour
)

type Config struct {
	Enabled       bool          `json:"enabled"`
	CacheSize     int           `json:"cacheSize"`
	CacheTTL      time.Duration `json:"cacheTTL"`
	AllowedLabels []string      `json:"allowedLabels"`
}

func NewConfigFromMap(cfgMap map[string]interface{}) (*Config, error) {
	cfg := &Config{
		Enabled:       false,
		CacheSize:     DefaultCacheSize,
		CacheTTL:      DefaultCacheTTL,
		AllowedLabels: []string{},
	}

	if enabled, ok := cfgMap["nodeMetadataAugmentationEnabled"].(string); ok && enabled == "true" {
		cfg.Enabled = true
	}

	if cacheSize, ok := cfgMap["nodeMetadataCacheSize"].(float64); ok {
		cfg.CacheSize = int(cacheSize)
	}

	if cacheTTLSeconds, ok := cfgMap["nodeMetadataCacheTTLSeconds"].(float64); ok {
		cfg.CacheTTL = time.Duration(cacheTTLSeconds) * time.Second
	}

	if allowedLabels, ok := cfgMap["nodeMetadataAllowedLabels"].([]interface{}); ok {
		cfg.AllowedLabels = make([]string, 0, len(allowedLabels))

		for _, label := range allowedLabels {
			if labelStr, ok := label.(string); ok {
				cfg.AllowedLabels = append(cfg.AllowedLabels, labelStr)
			}
		}
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.CacheSize <= 0 {
		return fmt.Errorf("cacheSize must be positive")
	}

	if c.CacheTTL <= 0 {
		return fmt.Errorf("cacheTTL must be positive")
	}

	return nil
}
