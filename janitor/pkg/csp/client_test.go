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

package csp

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvider_String(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		expected string
	}{
		{"kind provider", ProviderKind, "kind"},
		{"aws provider", ProviderAWS, "aws"},
		{"gcp provider", ProviderGCP, "gcp"},
		{"azure provider", ProviderAzure, "azure"},
		{"oci provider", ProviderOCI, "oci"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, string(tt.provider))
		})
	}
}

func TestGetProviderFromEnv_Valid(t *testing.T) {
	// Save original env var
	originalCSP := os.Getenv("CSP")
	defer func() {
		if originalCSP != "" {
			os.Setenv("CSP", originalCSP)
		} else {
			os.Unsetenv("CSP")
		}
	}()

	tests := []struct {
		name     string
		envValue string
		expected Provider
	}{
		{"kind", "kind", ProviderKind},
		{"aws", "aws", ProviderAWS},
		{"gcp", "gcp", ProviderGCP},
		{"azure", "azure", ProviderAzure},
		{"oci", "oci", ProviderOCI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			err := os.Setenv("CSP", tt.envValue)
			require.NoError(t, err)

			// Get provider
			provider, err := GetProviderFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tt.expected, provider)
		})
	}
}

func TestGetProviderFromEnv_Invalid(t *testing.T) {
	// Save original env var
	originalCSP := os.Getenv("CSP")
	defer func() {
		if originalCSP != "" {
			os.Setenv("CSP", originalCSP)
		} else {
			os.Unsetenv("CSP")
		}
	}()

	tests := []struct {
		name           string
		envValue       string
		expectDefault  bool
		expectedResult Provider
	}{
		{"empty string defaults to kind", "", true, ProviderKind},
		{"invalid provider", "invalid", false, ""},
		{"random string", "random-provider", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			if tt.envValue != "" {
				err := os.Setenv("CSP", tt.envValue)
				require.NoError(t, err)
			} else {
				os.Unsetenv("CSP")
			}

			// Get provider
			provider, err := GetProviderFromEnv()
			if tt.expectDefault {
				// Empty CSP defaults to kind
				require.NoError(t, err)
				assert.Equal(t, tt.expectedResult, provider)
			} else {
				// Invalid values should error
				assert.Error(t, err)
				assert.Equal(t, tt.expectedResult, provider)
			}
		})
	}
}

func TestNewWithProvider_Kind(t *testing.T) {
	ctx := context.Background()

	// Kind client should be created successfully
	client, err := NewWithProvider(ctx, ProviderKind)
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestNew_MissingEnvVar(t *testing.T) {
	// Save original env var
	originalCSP := os.Getenv("CSP")
	defer func() {
		if originalCSP != "" {
			os.Setenv("CSP", originalCSP)
		} else {
			os.Unsetenv("CSP")
		}
	}()

	// Unset CSP environment variable
	os.Unsetenv("CSP")

	ctx := context.Background()

	// When CSP is not set, it defaults to "kind"
	client, err := New(ctx)
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestNew_ValidProvider(t *testing.T) {
	// Save original env var
	originalCSP := os.Getenv("CSP")
	defer func() {
		if originalCSP != "" {
			os.Setenv("CSP", originalCSP)
		} else {
			os.Unsetenv("CSP")
		}
	}()

	// Set CSP to kind (which doesn't require cloud credentials)
	err := os.Setenv("CSP", "kind")
	require.NoError(t, err)

	ctx := context.Background()

	// Should successfully create kind client
	client, err := New(ctx)
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestProviderConstants(t *testing.T) {
	// Verify all provider constants are defined correctly
	assert.Equal(t, Provider("kind"), ProviderKind)
	assert.Equal(t, Provider("aws"), ProviderAWS)
	assert.Equal(t, Provider("gcp"), ProviderGCP)
	assert.Equal(t, Provider("azure"), ProviderAzure)
	assert.Equal(t, Provider("oci"), ProviderOCI)
}

func TestNewWithProvider_AllProviders(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		provider      Provider
		shouldSucceed bool
		skipReason    string
	}{
		{
			name:          "kind provider",
			provider:      ProviderKind,
			shouldSucceed: true,
		},
		{
			name:          "aws provider",
			provider:      ProviderAWS,
			shouldSucceed: false,
			skipReason:    "AWS client requires credentials",
		},
		{
			name:          "gcp provider",
			provider:      ProviderGCP,
			shouldSucceed: false,
			skipReason:    "GCP client requires credentials",
		},
		{
			name:          "azure provider",
			provider:      ProviderAzure,
			shouldSucceed: false,
			skipReason:    "Azure client requires credentials",
		},
		{
			name:          "oci provider",
			provider:      ProviderOCI,
			shouldSucceed: false,
			skipReason:    "OCI client requires credentials",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.shouldSucceed && tt.skipReason != "" {
				t.Skip(tt.skipReason)
			}

			client, err := NewWithProvider(ctx, tt.provider)
			if tt.shouldSucceed {
				require.NoError(t, err)
				assert.NotNil(t, client)
			} else {
				// Cloud providers may error without credentials
				if err != nil {
					assert.Error(t, err)
				}
			}
		})
	}
}

func TestNew_WithContext(t *testing.T) {
	// Save original env var
	originalCSP := os.Getenv("CSP")
	defer func() {
		if originalCSP != "" {
			os.Setenv("CSP", originalCSP)
		} else {
			os.Unsetenv("CSP")
		}
	}()

	// Test that context is properly passed through
	err := os.Setenv("CSP", "kind")
	require.NoError(t, err)

	// Create a context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should succeed with valid context
	client, err := New(ctx)
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestGetProviderFromString(t *testing.T) {
	// Test the GetProviderFromString function directly
	tests := []struct {
		name     string
		input    string
		expected Provider
		wantErr  bool
	}{
		{"kind lowercase", "kind", ProviderKind, false},
		{"aws lowercase", "aws", ProviderAWS, false},
		{"gcp lowercase", "gcp", ProviderGCP, false},
		{"azure lowercase", "azure", ProviderAzure, false},
		{"oci lowercase", "oci", ProviderOCI, false},
		{"kind uppercase", "KIND", ProviderKind, false}, // case insensitive
		{"aws uppercase", "AWS", ProviderAWS, false},
		{"gcp mixed case", "GcP", ProviderGCP, false},
		{"azure mixed case", "Azure", ProviderAzure, false},
		{"invalid", "invalid", "", true},
		{"empty", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := GetProviderFromString(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, provider)
			}
		})
	}
}
