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

package dedup

import (
	"fmt"
	"os"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

const (
	DefaultSuppressionWindow = 3 * time.Minute
	DefaultCleanupInterval   = 1 * time.Minute
)

// Config controls platform-connector health-event deduplication.
type Config struct {
	SuppressionWindow time.Duration `toml:"suppressionWindow"`
	CleanupInterval   time.Duration `toml:"cleanupInterval"`
	IncludeChecks     []string      `toml:"includeChecks"`
}

// LoadConfig loads dedup configuration from path, returning defaults when absent.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return DefaultConfig(), nil
	}

	var cfg Config
	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load dedup config %s: %w", path, err)
	}

	return &cfg, nil
}

// DefaultConfig returns the production default dedup configuration.
func DefaultConfig() *Config {
	return &Config{
		SuppressionWindow: DefaultSuppressionWindow,
		CleanupInterval:   DefaultCleanupInterval,
		IncludeChecks:     []string{},
	}
}

// Validate checks that dedup durations are usable.
func (c *Config) Validate() error {
	if c.SuppressionWindow <= 0 {
		return fmt.Errorf("suppressionWindow must be positive")
	}

	if c.CleanupInterval <= 0 {
		return fmt.Errorf("cleanupInterval must be positive")
	}

	return nil
}
