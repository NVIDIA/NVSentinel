// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

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
	if len(cfg.Policies) == 0 {
		return fmt.Errorf("no policies defined")
	}

	policyNames := make(map[string]bool)

	for i, policy := range cfg.Policies {
		if policy.Name == "" {
			return fmt.Errorf("policy[%d]: name is required", i)
		}

		if policyNames[policy.Name] {
			return fmt.Errorf("policy[%d]: duplicate policy name %q", i, policy.Name)
		}

		policyNames[policy.Name] = true

		if policy.Resource.Version == "" {
			return fmt.Errorf("policy %q: resource.version is required", policy.Name)
		}

		if policy.Resource.Kind == "" {
			return fmt.Errorf("policy %q: resource.kind is required", policy.Name)
		}

		if policy.Predicate.Expression == "" {
			return fmt.Errorf("policy %q: predicate.expression is required", policy.Name)
		}

		if policy.HealthEvent.ComponentClass == "" {
			return fmt.Errorf("policy %q: healthEvent.componentClass is required", policy.Name)
		}

		if policy.HealthEvent.Message == "" {
			return fmt.Errorf("policy %q: healthEvent.message is required", policy.Name)
		}
	}

	return nil
}
