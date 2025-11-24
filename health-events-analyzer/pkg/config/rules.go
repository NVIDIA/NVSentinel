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

package config

import (
	"fmt"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

type HealthEventsAnalyzerRule struct {
	Name              string   `toml:"name"`
	Description       string   `toml:"description"`
	Stage             []string `toml:"stage"`
	RecommendedAction string   `toml:"recommended_action"`
}

type TomlConfig struct {
	Rules []HealthEventsAnalyzerRule `toml:"rules"`
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if err := configmanager.LoadTOMLConfig(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	return &config, nil
}
