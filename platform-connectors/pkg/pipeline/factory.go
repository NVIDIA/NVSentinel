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
	"context"
	"fmt"
	"log/slog"
)

type Options struct {
	KubeconfigPath string
}

// Factory creates a Transformer from its pipeline config and shared options.
type Factory func(cfg *Config, opts Options) (Transformer, error)

// DisabledCheck is called when a stage is skipped (Enabled=false).
// Implementations should log warnings for misconfigurations that become
// silent when the stage is off (e.g. a skip-label key configured but no
// transformer to enforce it).
type DisabledCheck func(ctx context.Context, cfg *Config)

var (
	registry       = map[string]Factory{}
	disabledChecks = map[string]DisabledCheck{}
)

func Register(name string, factory Factory) {
	registry[name] = factory
}

// RegisterDisabledCheck registers an optional callback invoked when the
// named stage is present in the pipeline config but disabled.
func RegisterDisabledCheck(name string, check DisabledCheck) {
	disabledChecks[name] = check
}

func Create(cfg *Config, opts Options) (Transformer, error) {
	factory, ok := registry[cfg.Name]
	if !ok {
		return nil, fmt.Errorf("unknown transformer: %s", cfg.Name)
	}

	return factory(cfg, opts)
}

// NewFromConfigs creates a Pipeline from a slice of transformer configurations.
// Disabled stages are skipped. Returns an error if any enabled stage fails to initialize.
func NewFromConfigs(ctx context.Context, configs []Config, opts Options) (*Pipeline, error) {
	var transformers []Transformer

	for _, cfg := range configs {
		if !cfg.Enabled {
			slog.InfoContext(ctx, "Pipeline stage disabled", "name", cfg.Name)

			if check, ok := disabledChecks[cfg.Name]; ok {
				check(ctx, &cfg)
			}

			continue
		}

		if factory, ok := registry[cfg.Name]; ok {
			t, err := factory(&cfg, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to create transformer %s: %w", cfg.Name, err)
			}

			transformers = append(transformers, t)
			slog.InfoContext(ctx, "Transformer registered", "name", t.Name())

			continue
		}

		return nil, fmt.Errorf("unknown pipeline stage: %s", cfg.Name)
	}

	return New(transformers...), nil
}
