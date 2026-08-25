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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
)

// configFromJSON parses config the same way loadConfig does, so these tests
// exercise the real types the ConfigMap produces (int64 for whole numbers,
// []interface{} for arrays) rather than hand-built Go maps that would hide
// type-assertion bugs.
func configFromJSON(t *testing.T, raw string) map[string]interface{} {
	t.Helper()

	result := make(map[string]interface{})
	require.NoError(t, json.Unmarshal([]byte(raw), &result))

	return result
}

// stubKubeconfig writes a kubeconfig pointing at nothing. Building a clientset
// from it never contacts the API server, which is all initializeAuthInterceptor
// does; in a pod the equivalent comes from the in-cluster SA mount.
func stubKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user: {}
`), 0o600))

	return path
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

func TestInitializeAuthInterceptor_Disabled(t *testing.T) {
	for _, raw := range []string{
		`{"enableNodeBindingAuth":"false"}`,
		`{"enableNodeBindingAuth":false}`,
	} {
		got, err := initializeAuthInterceptor(context.Background(), configFromJSON(t, raw), "")

		require.NoError(t, err)
		assert.Nil(t, got, "config %s should disable node binding", raw)
	}
}

func TestInitializeAuthInterceptor_MalformedFlagFailsStartup(t *testing.T) {
	// A ConfigMap the chart could not have produced must not be interpreted.
	_, err := initializeAuthInterceptor(context.Background(),
		configFromJSON(t, `{"enableNodeBindingAuth":"yes"}`), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "enableNodeBindingAuth")
}

func TestInitializeAuthInterceptor_EnabledFormats(t *testing.T) {
	// The chart quotes the flag; an unquoted true from a hand-edited ConfigMap
	// is accepted too. An absent flag is NOT accepted - see TestNodeBindingEnabled.
	t.Setenv("NODE_NAME", "node-a")

	for _, raw := range []string{
		`{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`,
		`{"enableNodeBindingAuth":true,"AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`,
	} {
		got, err := initializeAuthInterceptor(context.Background(), configFromJSON(t, raw), stubKubeconfig(t))

		require.NoError(t, err)
		assert.NotNil(t, got, "config %s should enable node binding", raw)
	}
}

func TestInitializeAuthInterceptor_RequiresNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "")

	_, err := initializeAuthInterceptor(context.Background(),
		configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthCrossNodeServiceAccounts":[]}`), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NODE_NAME")
}

func TestInitializeAuthInterceptor_BuildsWithoutCrossNodeSAs(t *testing.T) {
	// Node-local monitors present tokens too, so the interceptor is fully
	// configured even when nothing is allowlisted for cross-node reach.
	t.Setenv("NODE_NAME", "node-a")

	got, err := initializeAuthInterceptor(context.Background(),
		configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`), stubKubeconfig(t))

	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestInitializeAuthInterceptor_RequiresAudience(t *testing.T) {
	// Without an audience no token can be verified, so there would be nothing
	// to enforce. Refuse to start rather than run a check that cannot fire.
	t.Setenv("NODE_NAME", "node-a")

	_, err := initializeAuthInterceptor(context.Background(),
		configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthCrossNodeServiceAccounts":[]}`), stubKubeconfig(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AuthAudience")
}

func TestAuthMode(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    auth.Mode
		wantErr string
	}{
		{name: "absent defaults to enforce", raw: `{"other":1}`, want: auth.ModeEnforce},
		{name: "explicit enforce", raw: `{"AuthMode":"enforce"}`, want: auth.ModeEnforce},
		{name: "explicit audit", raw: `{"AuthMode":"audit"}`, want: auth.ModeAudit},
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

func TestInitializeAuthInterceptor_RejectsUnknownMode(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")

	_, err := initializeAuthInterceptor(context.Background(), configFromJSON(t,
		`{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[],"AuthMode":"warn"}`),
		stubKubeconfig(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AuthMode")
}

func TestInitializeAuthInterceptor_AuditModeAndFailOpenBuild(t *testing.T) {
	// Both flags are optional and wire through to a working interceptor.
	t.Setenv("NODE_NAME", "node-a")

	got, err := initializeAuthInterceptor(context.Background(), configFromJSON(t,
		`{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[],`+
			`"AuthMode":"audit","AuthFailOpenOnUnavailable":true}`),
		stubKubeconfig(t))

	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestInitializeAuthInterceptor_RejectsNonCanonicalAllowlistEntry(t *testing.T) {
	// The chart no longer prefixes the namespace, so a bare name reaching this
	// far is a typo that would silently pin a cluster-scoped publisher.
	t.Setenv("NODE_NAME", "node-a")

	_, err := initializeAuthInterceptor(context.Background(), configFromJSON(t,
		`{"enableNodeBindingAuth":"true","AuthAudience":"a",`+
			`"AuthCrossNodeServiceAccounts":["csp-health-monitor"]}`), stubKubeconfig(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a canonical Kubernetes username")
}
