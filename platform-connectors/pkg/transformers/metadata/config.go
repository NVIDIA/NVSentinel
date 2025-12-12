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

package metadata

import (
	"fmt"
	"os"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

const (
	DefaultCacheSize = 50
	DefaultCacheTTL  = 1 * time.Hour
)

type Config struct {
	CacheSize     int           `toml:"cacheSize"`
	CacheTTL      time.Duration `toml:"cacheTTL"`
	AllowedLabels []string      `toml:"allowedLabels"`
}

func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return DefaultConfig(), nil
	}

	var cfg Config
	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func DefaultConfig() *Config {
	return &Config{
		CacheSize:     DefaultCacheSize,
		CacheTTL:      DefaultCacheTTL,
		AllowedLabels: []string{},
	}
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
