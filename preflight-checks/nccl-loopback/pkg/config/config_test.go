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

// Tests for env-based configuration helpers: float/int/bool parsing,
// required env vars, executable validation, and processing strategy parsing.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePositiveFloat(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "")
		v, err := parsePositiveFloat("TEST_FLOAT", 150.0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 150.0 {
			t.Errorf("got %f, want 150.0", v)
		}
	})

	t.Run("parses valid float", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "42.5")
		v, err := parsePositiveFloat("TEST_FLOAT", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 42.5 {
			t.Errorf("got %f, want 42.5", v)
		}
	})

	t.Run("rejects non-positive", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "-1.0")
		if _, err := parsePositiveFloat("TEST_FLOAT", 0); err == nil {
			t.Fatal("expected error for negative float")
		}
	})

	t.Run("rejects invalid", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "abc")
		if _, err := parsePositiveFloat("TEST_FLOAT", 0); err == nil {
			t.Fatal("expected error for non-numeric string")
		}
	})
}

func TestParsePositiveInt(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("TEST_INT", "")
		v, err := parsePositiveInt("TEST_INT", 256)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 256 {
			t.Errorf("got %d, want 256", v)
		}
	})

	t.Run("parses valid int", func(t *testing.T) {
		t.Setenv("TEST_INT", "128")
		v, err := parsePositiveInt("TEST_INT", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 128 {
			t.Errorf("got %d, want 128", v)
		}
	})

	t.Run("rejects non-positive", func(t *testing.T) {
		t.Setenv("TEST_INT", "0")
		if _, err := parsePositiveInt("TEST_INT", 0); err == nil {
			t.Fatal("expected error for zero")
		}
	})
}

func TestParseBool(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "")
		if parseBool("TEST_BOOL", false) {
			t.Error("expected false when unset with default false")
		}
	})

	t.Run("parses true", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "true")
		if !parseBool("TEST_BOOL", false) {
			t.Error("expected true")
		}
	})

	t.Run("invalid returns default", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "not-a-bool")
		if !parseBool("TEST_BOOL", true) {
			t.Error("expected default true for invalid input")
		}
	})
}

func TestRequireEnv(t *testing.T) {
	t.Run("returns value when set", func(t *testing.T) {
		t.Setenv("TEST_REQ", "value")
		v, err := requireEnv("TEST_REQ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "value" {
			t.Errorf("got %q, want %q", v, "value")
		}
	})

	t.Run("error when empty", func(t *testing.T) {
		t.Setenv("TEST_REQ", "")
		if _, err := requireEnv("TEST_REQ"); err == nil {
			t.Fatal("expected error for empty env var")
		}
	})
}

func TestValidateExecutable(t *testing.T) {
	t.Run("valid executable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "binary")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("failed to create test binary: %v", err)
		}
		if err := validateExecutable(path); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if err := validateExecutable("/nonexistent/binary"); err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("directory not executable", func(t *testing.T) {
		if err := validateExecutable(t.TempDir()); err == nil {
			t.Error("expected error for directory")
		}
	})

	t.Run("non-executable file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "noexec")
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		if err := validateExecutable(path); err == nil {
			t.Error("expected error for non-executable file")
		}
	})
}

func TestParseProcessingStrategy(t *testing.T) {
	t.Run("default strategy", func(t *testing.T) {
		t.Setenv("PROCESSING_STRATEGY", "")
		s, err := parseProcessingStrategy()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s == 0 {
			t.Error("expected non-zero strategy")
		}
	})

	t.Run("explicit strategy", func(t *testing.T) {
		t.Setenv("PROCESSING_STRATEGY", "EXECUTE_REMEDIATION")
		s, err := parseProcessingStrategy()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s == 0 {
			t.Error("expected non-zero strategy")
		}
	})

	t.Run("invalid strategy", func(t *testing.T) {
		t.Setenv("PROCESSING_STRATEGY", "INVALID_STRATEGY")
		if _, err := parseProcessingStrategy(); err == nil {
			t.Fatal("expected error for invalid strategy")
		}
	})
}
