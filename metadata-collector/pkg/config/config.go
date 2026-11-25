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

// Package config loads the configuration of the pod device mapper.
package config

import (
	"errors"
	"fmt"

	"github.com/BurntSushi/toml"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// ErrPartialRemediationEntityTypesMissing means the configuration has no partialRemediationEntityTypes key.
// The Helm chart always renders it from global.partialRemediationEntityTypes, which holds the default.
var ErrPartialRemediationEntityTypesMissing = errors.New(
	"partialRemediationEntityTypes is missing; set global.partialRemediationEntityTypes in the Helm values")

type fileConfig struct {
	PartialRemediationEntityTypes model.PartialRemediationEntityTypes `toml:"partialRemediationEntityTypes"`
}

// LoadPartialRemediationEntityTypes reads the entity types from the TOML file at path. The mapper records the
// devices that they list (ADR-071).
func LoadPartialRemediationEntityTypes(path string) (model.PartialRemediationEntityTypes, error) {
	if path == "" {
		return nil, errors.New("--config is not set; the pod device mapper needs the file with " +
			"partialRemediationEntityTypes")
	}

	var config fileConfig
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", path, err)
	}

	if config.PartialRemediationEntityTypes == nil {
		return nil, ErrPartialRemediationEntityTypesMissing
	}

	if err := config.PartialRemediationEntityTypes.Validate(); err != nil {
		return nil, fmt.Errorf("invalid partialRemediationEntityTypes: %w", err)
	}

	return config.PartialRemediationEntityTypes, nil
}
