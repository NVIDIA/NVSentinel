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
	"encoding/json"
	"os"
	"path/filepath"
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
		configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`), "")

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
