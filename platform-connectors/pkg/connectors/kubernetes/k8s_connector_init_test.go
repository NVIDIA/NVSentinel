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

package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

func TestInitializeK8sConnector_UsesExplicitKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(`
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

	stopCh := make(chan struct{})
	defer close(stopCh)

	connector, clientset, err := InitializeK8sConnector(
		context.Background(),
		ringbuffer.NewRingBuffer("kubernetes", context.Background()),
		5,
		10,
		stopCh,
		K8sConnectorConfig{
			MaxNodeConditionMessageLength: 1024,
			CompactedHealthEventMsgLen:    72,
		},
		kubeconfigPath,
	)
	require.NoError(t, err)
	require.NotNil(t, connector)
	require.NotNil(t, connector.clientset)
	require.NotNil(t, clientset)
}
