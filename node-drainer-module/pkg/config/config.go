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
	"fmt"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

type EvictMode string

const (
	ModeImmediateEvict     EvictMode = "Immediate"
	ModeAllowCompletion    EvictMode = "AllowCompletion"
	ModeDeleteAfterTimeout EvictMode = "DeleteAfterTimeout"
)

type Duration struct {
	time.Duration
}

type UserNamespace struct {
	Name string    `toml:"name"`
	Mode EvictMode `toml:"mode"`
}

type TomlConfig struct {
	EvictionTimeoutInSeconds  Duration `toml:"evictionTimeoutInSeconds"`
	SystemNamespaces          string   `toml:"systemNamespaces"`
	DeleteAfterTimeoutMinutes int      `toml:"deleteAfterTimeoutMinutes"`
	// NotReadyTimeoutMinutes is the time after which a pod in NotReady state is considered stuck
	NotReadyTimeoutMinutes int             `toml:"notReadyTimeoutMinutes"`
	UserNamespaces         []UserNamespace `toml:"userNamespaces"`
}

func (d *Duration) UnmarshalTOML(text interface{}) error {
	if v, ok := text.(string); ok {
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid duration format: %v", text)
		}
		if seconds <= 0 {
			return fmt.Errorf("eviction timeout must be a positive integer")
		}
		d.Duration = time.Duration(seconds) * time.Second
		return nil
	}
	return fmt.Errorf("invalid duration format: %v", text)
}

func LoadTomlConfig(path string) (*TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, err
	}

	return validateAndSetDefaults(&config)
}

// LoadTomlConfigFromString loads configuration from a TOML string (for Helm chart usage)
func LoadTomlConfigFromString(configString string) (*TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.Decode(configString, &config); err != nil {
		return nil, fmt.Errorf("failed to decode TOML config string: %w", err)
	}

	return validateAndSetDefaults(&config)
}

// validateAndSetDefaults validates the configuration and sets default values
func validateAndSetDefaults(config *TomlConfig) (*TomlConfig, error) {
	if config.DeleteAfterTimeoutMinutes == 0 {
		config.DeleteAfterTimeoutMinutes = 60 // Default: 60 minutes
	}

	if config.DeleteAfterTimeoutMinutes <= 0 {
		return nil, fmt.Errorf("deleteAfterTimeout must be a positive integer")
	}

	if config.NotReadyTimeoutMinutes == 0 {
		config.NotReadyTimeoutMinutes = 5 // Default: 5 minutes
	}

	if config.NotReadyTimeoutMinutes <= 0 {
		return nil, fmt.Errorf("notReadyTimeoutMinutes must be a positive integer")
	}
	return config, nil
}
