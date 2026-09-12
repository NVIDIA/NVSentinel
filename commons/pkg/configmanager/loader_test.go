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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testTOMLConfig struct {
	Name    string `toml:"name"`
	Port    int    `toml:"port"`
	Enabled bool   `toml:"enabled"`
}

func TestLoadTOMLConfig(t *testing.T) {
	t.Parallel()

	tomlContent := `name = "test"
port = 8080
enabled = true
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(tomlContent), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	var cfg testTOMLConfig

	err := LoadTOMLConfig(configPath, &cfg)
	if err != nil {
		t.Fatalf("failed to load TOML config: %v", err)
	}

	if cfg.Name != "test" {
		t.Errorf("expected name 'test', got '%s'", cfg.Name)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Port)
	}

	if !cfg.Enabled {
		t.Error("expected enabled to be true")
	}
}

func TestLoadTOMLConfigNonExistentFile(t *testing.T) {
	t.Parallel()

	var cfg testTOMLConfig

	nonExistentPath := filepath.Join(t.TempDir(), "nonexistent.toml")
	err := LoadTOMLConfig(nonExistentPath, &cfg)
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestLoadTOMLConfigInvalidSyntax(t *testing.T) {
	t.Parallel()

	invalidTOML := `name = "test"
port = this is not valid toml
enabled = true
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid_config.toml")
	if err := os.WriteFile(configPath, []byte(invalidTOML), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	var cfg testTOMLConfig

	err := LoadTOMLConfig(configPath, &cfg)
	if err == nil {
		t.Fatal("expected error for invalid TOML syntax, got nil")
	}
}

func TestLoadTOMLConfigStrictRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	contents := "name = \"test\"\nunknown_option = true\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var cfg testTOMLConfig
	err := LoadTOMLConfigStrict(configPath, &cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown_option") {
		t.Fatalf("LoadTOMLConfigStrict() error = %v", err)
	}
}

func TestLoadTOMLConfigStrictAcceptsKnownKeys(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("name = \"test\"\nport = 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var cfg testTOMLConfig
	if err := LoadTOMLConfigStrict(configPath, &cfg); err != nil {
		t.Fatalf("LoadTOMLConfigStrict() error = %v", err)
	}
	if cfg.Name != "test" || cfg.Port != 8080 {
		t.Fatalf("LoadTOMLConfigStrict() config = %#v", cfg)
	}
}

func TestLoadTOMLConfigStrictRejectsInvalidTOML(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("name = [invalid"), 0o600); err != nil {
		t.Fatal(err)
	}

	var cfg testTOMLConfig
	if err := LoadTOMLConfigStrict(configPath, &cfg); err == nil {
		t.Fatal("LoadTOMLConfigStrict() accepted invalid TOML")
	}
}
