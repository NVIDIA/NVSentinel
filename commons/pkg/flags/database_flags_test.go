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

package flags

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatabaseCertConfig_ResolveCertPath(t *testing.T) {
	tests := []struct {
		name                        string
		databaseClientCertMountPath string
		legacyMongoCertPath         string
		expectedResolvedPath        string
		description                 string
	}{
		{
			name:                        "new flag with default value uses legacy",
			databaseClientCertMountPath: "/etc/ssl/database-client",
			legacyMongoCertPath:         "/etc/ssl/mongo-client",
			expectedResolvedPath:        "/etc/ssl/mongo-client",
			description:                 "When new flag is default, legacy flag value should be used",
		},
		{
			name:                        "new flag with custom value uses new",
			databaseClientCertMountPath: "/custom/database-client",
			legacyMongoCertPath:         "/etc/ssl/mongo-client",
			expectedResolvedPath:        "/custom/database-client",
			description:                 "When new flag is explicitly set, it should be used",
		},
		{
			name:                        "new flag with default and legacy custom uses legacy",
			databaseClientCertMountPath: "/etc/ssl/database-client",
			legacyMongoCertPath:         "/custom/mongo-client",
			expectedResolvedPath:        "/custom/mongo-client",
			description:                 "When new flag is default and legacy is custom, legacy should be used",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DatabaseCertConfig{
				TLSEnabled:                  true,
				DatabaseClientCertMountPath: tt.databaseClientCertMountPath,
				LegacyMongoCertPath:         tt.legacyMongoCertPath,
			}

			resolvedPath := config.ResolveCertPath()

			assert.Equal(t, tt.expectedResolvedPath, resolvedPath, tt.description)
			assert.Equal(t, tt.expectedResolvedPath, config.ResolvedCertPath, "ResolvedCertPath should be set")
		})
	}
}

func TestDatabaseCertConfig_GetCertPath(t *testing.T) {
	// Create a temporary directory structure for testing
	tempDir, err := os.MkdirTemp("", "cert_test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Create test certificate directories
	legacyPath := filepath.Join(tempDir, "mongo-client")
	newPath := filepath.Join(tempDir, "database-client")
	customPath := filepath.Join(tempDir, "custom")

	require.NoError(t, os.MkdirAll(legacyPath, 0755))
	require.NoError(t, os.MkdirAll(newPath, 0755))
	require.NoError(t, os.MkdirAll(customPath, 0755))

	// Create ca.crt files in test directories
	require.NoError(t, os.WriteFile(filepath.Join(legacyPath, "ca.crt"), []byte("legacy cert"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(newPath, "ca.crt"), []byte("new cert"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(customPath, "ca.crt"), []byte("custom cert"), 0644))

	tests := []struct {
		name         string
		resolvedPath string
		expectedPath string
		description  string
	}{
		{
			name:         "resolved path exists",
			resolvedPath: customPath,
			expectedPath: customPath,
			description:  "When resolved path has ca.crt, it should be used",
		},
		{
			name:         "resolved path missing no fallback returns empty",
			resolvedPath: filepath.Join(tempDir, "nonexistent"),
			expectedPath: "",
			description:  "When resolved path and all fallback paths are missing, should return empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DatabaseCertConfig{
				TLSEnabled:       true,
				ResolvedCertPath: tt.resolvedPath,
			}

			certPath := config.GetCertPath()
			assert.Equal(t, tt.expectedPath, certPath, tt.description)
		})
	}

	// Test fallback to legacy path when resolved path is missing.
	// GetCertPath has hardcoded fallback paths, so this can only run
	// in environments where /etc/ssl/mongo-client/ca.crt exists.
	t.Run("resolved path missing fallback to legacy", func(t *testing.T) {
		if _, err := os.Stat("/etc/ssl/mongo-client/ca.crt"); err != nil {
			t.Skip("Skipping fallback test: /etc/ssl/mongo-client/ca.crt not present on this host")
		}

		config := &DatabaseCertConfig{
			TLSEnabled:       true,
			ResolvedCertPath: filepath.Join(tempDir, "nonexistent"),
		}

		certPath := config.GetCertPath()
		assert.Equal(t, "/etc/ssl/mongo-client", certPath,
			"When resolved path is missing, should fallback to legacy path")
	})
}

func TestDatabaseCertConfig_TLSDisabled_ResolveCertPath(t *testing.T) {
	config := &DatabaseCertConfig{
		TLSEnabled:                  false,
		DatabaseClientCertMountPath: "/etc/ssl/database-client",
		LegacyMongoCertPath:         "/etc/ssl/mongo-client",
	}

	certPath := config.ResolveCertPath()
	assert.Empty(t, certPath, "ResolveCertPath should return empty when TLS is disabled")
}

func TestDatabaseCertConfig_TLSDisabled_GetCertPath(t *testing.T) {
	// Even when cert files exist, GetCertPath should return empty when TLS is disabled
	tempDir, err := os.MkdirTemp("", "cert_test_tls_disabled")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	certDir := filepath.Join(tempDir, "certs")
	require.NoError(t, os.MkdirAll(certDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(certDir, "ca.crt"), []byte("test cert"), 0644))

	config := &DatabaseCertConfig{
		TLSEnabled:       false,
		ResolvedCertPath: certDir,
	}

	certPath := config.GetCertPath()
	assert.Empty(t, certPath, "GetCertPath should return empty when TLS is disabled, even if certs exist")
}

func TestDatabaseCertConfig_TLSEnabled_GetCertPath(t *testing.T) {
	// When TLS is enabled and certs exist, should return the path
	tempDir, err := os.MkdirTemp("", "cert_test_tls_enabled")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	certDir := filepath.Join(tempDir, "certs")
	require.NoError(t, os.MkdirAll(certDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(certDir, "ca.crt"), []byte("test cert"), 0644))

	config := &DatabaseCertConfig{
		TLSEnabled:       true,
		ResolvedCertPath: certDir,
	}

	certPath := config.GetCertPath()
	assert.Equal(t, certDir, certPath, "GetCertPath should return cert path when TLS is enabled and certs exist")
}

func TestDatabaseCertConfig_TLSEnabled_NoCerts_ReturnsEmpty(t *testing.T) {
	// When TLS is enabled but no certs exist anywhere, should return empty
	config := &DatabaseCertConfig{
		TLSEnabled:       true,
		ResolvedCertPath: "/nonexistent/path/that/does/not/exist",
	}

	certPath := config.GetCertPath()
	assert.Empty(t, certPath, "GetCertPath should return empty when TLS is enabled but no certs exist")
}

func TestDatabaseCertConfig_GetCertPath_WithRealPaths(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "cert_test_real_paths")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	legacyPath := filepath.Join(tempDir, "mongo-client")
	newPath := filepath.Join(tempDir, "database-client")

	require.NoError(t, os.MkdirAll(legacyPath, 0755))
	require.NoError(t, os.MkdirAll(newPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(legacyPath, "ca.crt"), []byte("legacy cert"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(newPath, "ca.crt"), []byte("new cert"), 0644))

	tests := []struct {
		name         string
		resolvedPath string
		expectedPath string
		description  string
	}{
		{
			name:         "legacy path preference",
			resolvedPath: legacyPath,
			expectedPath: legacyPath,
			description:  "Should handle legacy path correctly",
		},
		{
			name:         "new path preference",
			resolvedPath: newPath,
			expectedPath: newPath,
			description:  "Should handle new path correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DatabaseCertConfig{
				TLSEnabled:       true,
				ResolvedCertPath: tt.resolvedPath,
			}

			certPath := config.GetCertPath()

			assert.NotEmpty(t, certPath, "GetCertPath should return a non-empty path")
			assert.Contains(t, []string{legacyPath, newPath, tt.resolvedPath},
				certPath, "Should return one of the expected paths")
			assert.Equal(t, tt.expectedPath, certPath, tt.description)
		})
	}
}
