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

// Package pipeline provides a transformer pipeline for processing health events.
// It includes a registry-based factory for creating transformers from configuration.
package pipeline

import (
	"fmt"
	"log/slog"
)

type Factory func(cfg *Config) (Transformer, error)

var registry = map[string]Factory{}

func Register(name string, factory Factory) {
	registry[name] = factory
}

func Create(cfg *Config) (Transformer, error) {
	factory, ok := registry[cfg.Name]
	if !ok {
		return nil, fmt.Errorf("unknown transformer: %s", cfg.Name)
	}

	return factory(cfg)
}

// NewFromConfigs creates a Pipeline from a slice of transformer configurations.
// Disabled transformers are skipped. Returns an error if any enabled transformer
// fails to initialize.
func NewFromConfigs(configs []Config) (*Pipeline, error) {
	var transformers []Transformer

	for _, cfg := range configs {
		if !cfg.Enabled {
			slog.Info("Transformer disabled", "name", cfg.Name)
			continue
		}

		t, err := Create(&cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create transformer %s: %w", cfg.Name, err)
		}

		transformers = append(transformers, t)
		slog.Info("Transformer registered", "name", t.Name())
	}

	return New(transformers...), nil
}
