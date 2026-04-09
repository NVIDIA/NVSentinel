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

package config

import (
	"os"
	"strings"
	"testing"
)

func setPostgreSQLEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATASTORE_HOST", "localhost")
	t.Setenv("DATASTORE_PORT", "5432")
	t.Setenv("DATASTORE_DATABASE", "testdb")
	t.Setenv("DATASTORE_USERNAME", "testuser")
	t.Setenv("DATASTORE_SSLMODE", "disable")
}

func TestNewPostgreSQLCompatibleConfig_WithPassword(t *testing.T) {
	setPostgreSQLEnv(t)
	t.Setenv("DATASTORE_PASSWORD", "s3cret")

	cfg, err := newPostgreSQLCompatibleConfig("/certs", "", "default_table")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uri := cfg.GetConnectionURI()
	if !strings.Contains(uri, "password=s3cret") {
		t.Errorf("expected connection URI to contain password=s3cret, got: %s", uri)
	}
}

func TestNewPostgreSQLCompatibleConfig_WithoutPassword(t *testing.T) {
	setPostgreSQLEnv(t)
	os.Unsetenv("DATASTORE_PASSWORD")

	cfg, err := newPostgreSQLCompatibleConfig("/certs", "", "default_table")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uri := cfg.GetConnectionURI()
	if strings.Contains(uri, "password=") {
		t.Errorf("expected connection URI to not contain password=, got: %s", uri)
	}
}

func TestNewPostgreSQLCompatibleConfig_PasswordWithSpecialChars(t *testing.T) {
	setPostgreSQLEnv(t)
	t.Setenv("DATASTORE_PASSWORD", "p@ss w'ord\\1")

	cfg, err := newPostgreSQLCompatibleConfig("/certs", "", "default_table")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uri := cfg.GetConnectionURI()
	// Password with special chars should be single-quoted and escaped
	if !strings.Contains(uri, "password='p@ss w\\'ord\\\\1'") {
		t.Errorf("expected properly quoted password in URI, got: %s", uri)
	}
}
