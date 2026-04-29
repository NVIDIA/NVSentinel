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
	"regexp"
	"strings"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

// Config represents the NIC Health Monitor configuration loaded from TOML.
type Config struct {
	// NicExclusionRegex contains comma-separated regex patterns for NICs to exclude
	NicExclusionRegex string `toml:"nicExclusionRegex"`

	// NicInclusionRegexOverride, when non-empty, bypasses automatic device discovery
	// and monitors only NIC devices whose names match these comma-separated regex patterns.
	NicInclusionRegexOverride string `toml:"nicInclusionRegexOverride"`

	// SysClassNetPath is the sysfs path for network interfaces (container mount point)
	SysClassNetPath string `toml:"sysClassNetPath"`

	// SysClassInfinibandPath is the sysfs path for InfiniBand devices (container mount point)
	SysClassInfinibandPath string `toml:"sysClassInfinibandPath"`
}

// LoadConfig reads and parses the TOML configuration file.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if err := configmanager.LoadTOMLConfig(path, cfg); err != nil {
		return nil, err
	}

	if cfg.SysClassNetPath == "" {
		cfg.SysClassNetPath = "/nvsentinel/sys/class/net"
	}

	if cfg.SysClassInfinibandPath == "" {
		cfg.SysClassInfinibandPath = "/nvsentinel/sys/class/infiniband"
	}

	if err := validateRegexList(cfg.NicExclusionRegex); err != nil {
		return nil, fmt.Errorf("invalid nicExclusionRegex: %w", err)
	}

	if err := validateRegexList(cfg.NicInclusionRegexOverride); err != nil {
		return nil, fmt.Errorf("invalid nicInclusionRegexOverride: %w", err)
	}

	return cfg, nil
}

func validateRegexList(commaSeparated string) error {
	if commaSeparated == "" {
		return nil
	}

	for _, pat := range strings.Split(commaSeparated, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}

		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
	}

	return nil
}
