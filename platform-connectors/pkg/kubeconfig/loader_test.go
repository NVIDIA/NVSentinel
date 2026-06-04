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

package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadUsesExplicitKubeconfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://10.0.0.1:443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "https://10.0.0.1:443", cfg.Host)
	require.Equal(t, "test-token", cfg.BearerToken)
}

func TestLoadReturnsPathErrorForMissingFile(t *testing.T) {
	_, err := Load("/path/does/not/exist")
	require.Error(t, err)
	require.ErrorContains(t, err, "/path/does/not/exist")
}

func TestLoadReturnsErrorWhenNoConfigSourcesAvailable(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("HOME", t.TempDir())

	_, err := Load("")
	require.Error(t, err)
	require.ErrorContains(t, err, "no configuration has been provided")
}
