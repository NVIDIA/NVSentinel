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

package configmanager

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// LoadTOMLConfig loads configuration from a TOML file into the provided config struct.
// The config parameter should be a pointer to a struct with TOML tags.
// Type is inferred from the config parameter.
//
// Example usage:
//
//	type Config struct {
//	    ServerPort int      `toml:"serverPort"`
//	    LogLevel   string   `toml:"logLevel"`
//	    Features   []string `toml:"features"`
//	}
//
//	var cfg Config
//	err := configmanager.LoadTOMLConfig("/etc/config.toml", &cfg)
//	if err != nil {
//	    log.Fatalf("Failed to load config: %v", err)
//	}
//
//	// Validate and set defaults after loading
//	if cfg.ServerPort == 0 {
//	    cfg.ServerPort = 8080
//	}
//	if cfg.ServerPort < 1 || cfg.ServerPort > 65535 {
//	    return fmt.Errorf("ServerPort must be between 1 and 65535")
//	}
func LoadTOMLConfig[T any](path string, config *T) error {
	if _, err := toml.DecodeFile(path, config); err != nil {
		return fmt.Errorf("failed to decode TOML file %s: %w", path, err)
	}

	return nil
}
