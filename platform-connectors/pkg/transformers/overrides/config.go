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

package overrides

import (
	"fmt"
	"os"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type Config struct {
	Enabled bool   `toml:"enabled"`
	Rules   []Rule `toml:"rules"`
}

type Rule struct {
	Name     string   `toml:"name"`
	When     string   `toml:"when"`
	Override Override `toml:"override"`
}

type Override struct {
	IsFatal           *bool   `toml:"isFatal,omitempty"`
	IsHealthy         *bool   `toml:"isHealthy,omitempty"`
	RecommendedAction *string `toml:"recommendedAction,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &Config{Enabled: false}, nil
	}

	var cfg Config
	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if len(c.Rules) == 0 {
		return fmt.Errorf("no rules defined but overrides enabled")
	}

	for i, rule := range c.Rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("rule[%d]: %w", i, err)
		}
	}

	return nil
}

func (r *Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}

	if r.When == "" {
		return fmt.Errorf("(%s): when expression is required", r.Name)
	}

	if r.Override.isEmpty() {
		return fmt.Errorf("(%s): at least one override field required", r.Name)
	}

	if r.Override.RecommendedAction != nil {
		if _, err := r.Override.ParseRecommendedAction(); err != nil {
			return fmt.Errorf("(%s): %w", r.Name, err)
		}
	}

	return nil
}

func (o *Override) isEmpty() bool {
	return o.IsFatal == nil && o.IsHealthy == nil && o.RecommendedAction == nil
}

func (o *Override) ParseRecommendedAction() (pb.RecommendedAction, error) {
	if o.RecommendedAction == nil {
		return pb.RecommendedAction_NONE, nil
	}

	action := *o.RecommendedAction

	val, ok := pb.RecommendedAction_value[action]
	if !ok {
		return pb.RecommendedAction_CONTACT_SUPPORT,
			fmt.Errorf("invalid recommendedAction: %s ", action)
	}

	return pb.RecommendedAction(val), nil
}
