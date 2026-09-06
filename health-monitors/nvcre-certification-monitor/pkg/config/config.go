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

// Package config loads the nvcre-certification-monitor policy configuration.
package config

import (
	"fmt"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

type Config struct {
	Policies []Policy `toml:"policies"`
}

// Policy selects which Certification categories trigger health events.
// Match is a CEL expression evaluated against each category of a failed
// Certification. If it evaluates to true, the category's failed nodes are
// published as unhealthy events and tracked in the node annotation.
type Policy struct {
	Name  string `toml:"name"`
	Match string `toml:"match"`
}

// Load reads and validates the policy configuration from a TOML file.
func Load(path string) (*Config, error) {
	var cfg Config
	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if len(cfg.Policies) == 0 {
		return fmt.Errorf("no policies defined")
	}

	names := make(map[string]bool, len(cfg.Policies))

	for i, p := range cfg.Policies {
		if p.Name == "" {
			return fmt.Errorf("policy[%d]: name is required", i)
		}

		if names[p.Name] {
			return fmt.Errorf("policy[%d]: duplicate policy name %q", i, p.Name)
		}

		names[p.Name] = true

		if p.Match == "" {
			return fmt.Errorf("policy %q: match expression is required", p.Name)
		}
	}

	return nil
}
