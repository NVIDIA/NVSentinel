// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"regexp"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

// Load reads and validates the config from path.
// Regex patterns are compiled at load time to fail fast.
func Load(path string) (*Config, error) {
	var cfg Config
	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		return nil, err
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if len(cfg.Patterns) == 0 {
		return fmt.Errorf("at least one pattern is required")
	}

	patternNames := make(map[string]bool)

	for i, p := range cfg.Patterns {
		if p.Name == "" {
			return fmt.Errorf("patterns[%d]: name is required", i)
		}

		if patternNames[p.Name] {
			return fmt.Errorf("patterns[%d]: duplicate pattern name %q", i, p.Name)
		}

		patternNames[p.Name] = true

		if p.Regex == "" {
			return fmt.Errorf("pattern %q: regex is required", p.Name)
		}

		if _, err := regexp.Compile(p.Regex); err != nil {
			return fmt.Errorf("pattern %q: invalid regex %q: %w", p.Name, p.Regex, err)
		}

		if p.CheckName == "" {
			return fmt.Errorf("pattern %q: checkName is required", p.Name)
		}

		if p.ComponentClass == "" {
			return fmt.Errorf("pattern %q: componentClass is required", p.Name)
		}
	}

	return nil
}
