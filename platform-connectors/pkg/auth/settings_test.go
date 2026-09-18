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

package auth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configFromJSON decodes config the way configfile.Load does (numbers as
// json.Number, arrays as []any), so these tests exercise the real types the
// ConfigMap produces rather than hand-built Go maps that would hide
// type-assertion bugs.
func configFromJSON(t *testing.T, raw string) map[string]any {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()

	result := map[string]any{}
	require.NoError(t, dec.Decode(&result))

	return result
}

func TestSettingsFromConfig(t *testing.T) {
	disabled, err := SettingsFromConfig(configFromJSON(t, `{"enableNodeBindingAuth":"false","AuthMode":"bogus"}`))
	require.NoError(t, err, "nothing else is read when node binding is off")
	assert.Equal(t, Settings{}, disabled)

	full, err := SettingsFromConfig(configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthAudience":"aud",`+
		`"AuthCrossNodeServiceAccounts":["system:serviceaccount:nvsentinel:health-events-analyzer"],`+
		`"AuthMode":"audit","AuthFailOpenOnUnavailable":"true"}`))
	require.NoError(t, err)
	assert.Equal(t, Settings{
		Enabled:                  true,
		Audience:                 "aud",
		CrossNodeServiceAccounts: []string{"system:serviceaccount:nvsentinel:health-events-analyzer"},
		Mode:                     ModeAudit,
		FailOpenOnUnavailable:    true,
	}, full)

	minimal, err := SettingsFromConfig(configFromJSON(t, `{"enableNodeBindingAuth":true,"AuthAudience":"aud","AuthCrossNodeServiceAccounts":[]}`))
	require.NoError(t, err)
	assert.Equal(t, ModeEnforce, minimal.Mode, "absent mode enforces")
	assert.Empty(t, minimal.CrossNodeServiceAccounts)

	for name, raw := range map[string]string{
		"AuthAudience":                 `{"enableNodeBindingAuth":"true","AuthCrossNodeServiceAccounts":[]}`,
		"AuthCrossNodeServiceAccounts": `{"enableNodeBindingAuth":"true","AuthAudience":"aud"}`,
		"AuthMode":                     `{"enableNodeBindingAuth":"true","AuthAudience":"aud","AuthCrossNodeServiceAccounts":[],"AuthMode":"warn"}`,
		"enableNodeBindingAuth":        `{"AuthAudience":"aud","AuthCrossNodeServiceAccounts":[]}`,
	} {
		_, err := SettingsFromConfig(configFromJSON(t, raw))
		require.ErrorContains(t, err, name, "the error names the key")
	}
}

func TestStringSliceFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		key     string
		want    []string
		wantErr bool
	}{
		{
			name:    "absent key is a configuration error",
			raw:     `{"other": 1}`,
			key:     "AuthCrossNodeServiceAccounts",
			wantErr: true,
		},
		{
			name:    "explicit null is a configuration error",
			raw:     `{"AuthCrossNodeServiceAccounts": null}`,
			key:     "AuthCrossNodeServiceAccounts",
			wantErr: true,
		},
		{
			name: "empty array yields empty slice",
			raw:  `{"AuthCrossNodeServiceAccounts": []}`,
			key:  "AuthCrossNodeServiceAccounts",
			want: []string{},
		},
		{
			name: "populated array",
			raw:  `{"AuthCrossNodeServiceAccounts": ["system:serviceaccount:ns:a","system:serviceaccount:ns:b"]}`,
			key:  "AuthCrossNodeServiceAccounts",
			want: []string{"system:serviceaccount:ns:a", "system:serviceaccount:ns:b"},
		},
		{
			name:    "wrong container type is an error, not silently ignored",
			raw:     `{"AuthCrossNodeServiceAccounts": "not-a-list"}`,
			key:     "AuthCrossNodeServiceAccounts",
			wantErr: true,
		},
		{
			name:    "non-string element is an error",
			raw:     `{"AuthCrossNodeServiceAccounts": ["ok", 42]}`,
			key:     "AuthCrossNodeServiceAccounts",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stringSliceFromConfig(configFromJSON(t, tt.raw), tt.key)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNodeBindingEnabled(t *testing.T) {
	// Disabling enforcement must take saying so. Anything that is neither a
	// clear yes nor a clear no stops the process instead of quietly leaving the
	// socket open to any node name.
	tests := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "absent is a configuration error", raw: `{"other":1}`, wantErr: true},
		{name: "quoted true", raw: `{"enableNodeBindingAuth":"true","AuthCrossNodeServiceAccounts":[]}`, want: true},
		{name: "unquoted true", raw: `{"enableNodeBindingAuth":true}`, want: true},
		{name: "quoted false", raw: `{"enableNodeBindingAuth":"false"}`, want: false},
		{name: "unquoted false", raw: `{"enableNodeBindingAuth":false}`, want: false},
		{name: "explicit null is malformed", raw: `{"enableNodeBindingAuth":null}`, wantErr: true},
		{name: "typo is malformed", raw: `{"enableNodeBindingAuth":"yes"}`, wantErr: true},
		{name: "wrong case is malformed", raw: `{"enableNodeBindingAuth":"True"}`, wantErr: true},
		{name: "number is malformed", raw: `{"enableNodeBindingAuth":1}`, wantErr: true},
		{name: "empty string is malformed", raw: `{"enableNodeBindingAuth":""}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nodeBindingEnabled(configFromJSON(t, tt.raw))

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "must be true or false")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAuthMode(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Mode
		wantErr string
	}{
		{name: "absent defaults to enforce", raw: `{"other":1}`, want: ModeEnforce},
		{name: "explicit enforce", raw: `{"AuthMode":"enforce"}`, want: ModeEnforce},
		{name: "explicit audit", raw: `{"AuthMode":"audit"}`, want: ModeAudit},
		{name: "unknown value is rejected", raw: `{"AuthMode":"warn"}`, wantErr: `must be "enforce" or "audit"`},
		{name: "wrong type is rejected", raw: `{"AuthMode":1}`, wantErr: "must be a string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := authMode(configFromJSON(t, tt.raw))

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBoolFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		def     bool
		want    bool
		wantErr bool
	}{
		{name: "absent returns default", raw: `{"other":1}`, def: true, want: true},
		{name: "quoted true", raw: `{"k":"true"}`, want: true},
		{name: "unquoted true", raw: `{"k":true}`, want: true},
		{name: "quoted false", raw: `{"k":"false"}`, def: true, want: false},
		{name: "unquoted false", raw: `{"k":false}`, def: true, want: false},
		{name: "typo is malformed", raw: `{"k":"yes"}`, wantErr: true},
		{name: "number is malformed", raw: `{"k":1}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := boolFromConfig(configFromJSON(t, tt.raw), "k", tt.def)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
