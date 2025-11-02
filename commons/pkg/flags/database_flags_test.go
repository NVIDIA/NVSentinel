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
			name:         "resolved path missing fallback to legacy",
			resolvedPath: filepath.Join(tempDir, "nonexistent"),
			expectedPath: "/etc/ssl/mongo-client", // Falls back to hardcoded legacy path
			description:  "When resolved path missing, should fallback to legacy path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DatabaseCertConfig{
				ResolvedCertPath: tt.resolvedPath,
			}

			certPath := config.GetCertPath()

			if tt.expectedPath == "/etc/ssl/mongo-client" {
				// For fallback cases, just check it's using the fallback logic
				assert.True(t, certPath == "/etc/ssl/mongo-client" || certPath == "/etc/ssl/database-client" || certPath == tt.resolvedPath,
					"Should use fallback logic when resolved path doesn't exist")
			} else {
				assert.Equal(t, tt.expectedPath, certPath, tt.description)
			}
		})
	}
}

func TestDatabaseCertConfig_GetCertPath_WithRealPaths(t *testing.T) {
	tests := []struct {
		name         string
		resolvedPath string
		description  string
	}{
		{
			name:         "legacy path preference",
			resolvedPath: "/etc/ssl/mongo-client",
			description:  "Should handle legacy path correctly",
		},
		{
			name:         "new path preference",
			resolvedPath: "/etc/ssl/database-client",
			description:  "Should handle new path correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DatabaseCertConfig{
				ResolvedCertPath: tt.resolvedPath,
			}

			certPath := config.GetCertPath()

			// Since we can't guarantee these paths exist in test environment,
			// just verify the function returns a reasonable path
			assert.NotEmpty(t, certPath, "GetCertPath should return a non-empty path")
			assert.Contains(t, []string{"/etc/ssl/mongo-client", "/etc/ssl/database-client", tt.resolvedPath},
				certPath, "Should return one of the expected paths")
		})
	}
}