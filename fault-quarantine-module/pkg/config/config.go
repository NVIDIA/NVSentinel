// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"github.com/BurntSushi/toml"
)

type Rule struct {
	Kind       string `toml:"kind"`
	Expression string `toml:"expression"`
}

type Taint struct {
	Key    string `toml:"key"`
	Value  string `toml:"value"`
	Effect string `toml:"effect"`
}

type Cordon struct {
	ShouldCordon bool `toml:"shouldCordon"`
}

type Match struct {
	Any []Rule `toml:"any"`
	All []Rule `toml:"all"`
}

type RuleSet struct {
	Version  string `toml:"version"`
	Name     string `toml:"name"`
	Priority int    `toml:"priority"`
	Match    Match  `toml:"match"`
	Taint    Taint  `toml:"taint"`
	Cordon   Cordon `toml:"cordon"`
}

type TomlConfig struct {
	LabelPrefix string    `toml:"label-prefix"`
	RuleSets    []RuleSet `toml:"rule-sets"`
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, err
	}

	return &config, nil
}
