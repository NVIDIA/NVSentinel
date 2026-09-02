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

	for _, parameter := range []string{"sslcert=", "sslkey=", "sslrootcert="} {
		if strings.Contains(uri, parameter) {
			t.Errorf("expected password authentication not to infer %s from the bundled certificate path, got: %s",
				parameter, uri)
		}
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

	for _, expected := range []string{
		"sslcert=/certs/tls.crt",
		"sslkey=/certs/tls.key",
		"sslrootcert=/certs/ca.crt",
	} {
		if !strings.Contains(uri, expected) {
			t.Errorf("expected certificate authentication URI to contain %s, got: %s", expected, uri)
		}
	}
}

func TestNewPostgreSQLCompatibleConfig_PasswordWithExplicitCA(t *testing.T) {
	setPostgreSQLEnv(t)
	t.Setenv("DATASTORE_PASSWORD", "s3cret")
	t.Setenv("DATASTORE_SSLROOTCERT", "/etc/ssl/external postgres/ca.crt")

	cfg, err := newPostgreSQLCompatibleConfig("/bundled-postgres-certs", "", "default_table")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uri := cfg.GetConnectionURI()
	if !strings.Contains(uri, "sslrootcert='/etc/ssl/external postgres/ca.crt'") {
		t.Errorf("expected connection URI to contain the explicitly configured CA, got: %s", uri)
	}

	if strings.Contains(uri, "sslcert=") || strings.Contains(uri, "sslkey=") ||
		strings.Contains(uri, "/bundled-postgres-certs") {
		t.Errorf("expected explicit CA-only password authentication not to infer client certificates, got: %s", uri)
	}
}
