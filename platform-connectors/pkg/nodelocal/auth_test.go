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

package nodelocal

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// configFromJSON decodes config.json text the way the binary does, so these
// tests see the types a rendered ConfigMap produces.
func configFromJSON(t *testing.T, raw string) map[string]any {
	t.Helper()

	m, err := configfile.Decode([]byte(raw))
	require.NoError(t, err)

	return m
}

// stubKubeconfig writes a kubeconfig pointing at nothing. Building a clientset
// from it never contacts the API server, which is all authInterceptor
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

func TestAuthInterceptor_Disabled(t *testing.T) {
	got, err := authInterceptor(context.Background(), configFromJSON(t, `{"enableNodeBindingAuth":"false"}`), "")

	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestAuthInterceptor_Enabled(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")

	raw := `{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`
	got, err := authInterceptor(context.Background(), configFromJSON(t, raw), stubKubeconfig(t))

	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestAuthInterceptor_RequiresNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "")

	_, err := authInterceptor(context.Background(),
		configFromJSON(t, `{"enableNodeBindingAuth":"true","AuthAudience":"a","AuthCrossNodeServiceAccounts":[]}`), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NODE_NAME")
}
