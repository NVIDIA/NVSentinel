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

package initializer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/labeler/pkg/labeler"
)

func TestInitializeAll_RateLimitScenarios_InitializedLabelerUsesConfiguredQPS(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	testConfig, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })

	adminClient, err := kubernetes.NewForConfig(testConfig)
	require.NoError(t, err)

	kubeconfigPath := writeKubeconfig(t, testConfig)

	tests := []struct {
		name   string
		prefix string
		qps    float64
	}{
		{name: "low QPS", prefix: "low-qps", qps: 4},
		{name: "high QPS", prefix: "high-qps", qps: 40},
	}

	const nodeCount = 10

	spans := make(map[string]time.Duration, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodeNames := make([]string, nodeCount)
			for idx := range nodeCount {
				nodeNames[idx] = fmt.Sprintf("%s-%d-%d", test.prefix, idx, time.Now().UnixNano())
				_, createErr := adminClient.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
					Name:   nodeNames[idx],
					Labels: map[string]string{"nvidia.com/gpu.present": "true"},
				}, metav1.CreateOptions{})
				require.NoError(t, createErr)
			}

			components, initErr := InitializeAll(InitializationParams{
				KubeconfigPath:        kubeconfigPath,
				DCGMAppLabel:          "nvidia-dcgm",
				DriverAppLabel:        "nvidia-driver-daemonset",
				GKEInstallerAppLabel:  "nvidia-driver-installer",
				AssumeDriverInstalled: true,
				KubernetesClientRateLimits: kubeclient.RateLimitConfig{
					QPS:   test.qps,
					Burst: 1,
				},
			})
			require.NoError(t, initErr)

			nodeWatch, watchErr := adminClient.CoreV1().Nodes().Watch(t.Context(), metav1.ListOptions{})
			require.NoError(t, watchErr)
			defer nodeWatch.Stop()

			ctx, cancel := context.WithCancel(t.Context())
			runErr := make(chan error, 1)
			go func() {
				runErr <- components.Labeler.Run(ctx)
			}()

			spans[test.name] = waitForLabelSpan(t, nodeWatch, nodeNames)

			cancel()
			require.NoError(t, <-runErr)

			for _, nodeName := range nodeNames {
				require.NoError(t, adminClient.CoreV1().Nodes().Delete(
					t.Context(), nodeName, metav1.DeleteOptions{},
				))
			}
		})
	}

	// The labeler patches each node once, and Burst is 1, so the client rate limiter spaces the patches at least
	// 1/QPS apart. The span from the first label to the last label excludes the start-up time of the labeler, which
	// depends on the load of the machine. The lower bound allows for a delayed watch event.
	for _, test := range tests {
		minSpan := time.Duration(float64(nodeCount-1) / test.qps * 0.5 * float64(time.Second))
		t.Logf("%s: labeled %d nodes in %v, minimum %v", test.name, nodeCount, spans[test.name], minSpan)
		assert.GreaterOrEqual(t, spans[test.name], minSpan)
	}

	// With 10 times the QPS, the labeler must label the nodes much faster.
	assert.Less(t, spans["high QPS"], spans["low QPS"]/3)
}

// waitForLabelSpan returns the time from the first to the last of the given nodes that nodeWatch shows with the
// driver-installed label.
func waitForLabelSpan(t *testing.T, nodeWatch watch.Interface, nodeNames []string) time.Duration {
	t.Helper()

	pending := make(map[string]bool, len(nodeNames))
	for _, nodeName := range nodeNames {
		pending[nodeName] = true
	}

	var first, last time.Time

	timeout := time.After(20 * time.Second)
	for len(pending) > 0 {
		select {
		case event, ok := <-nodeWatch.ResultChan():
			require.True(t, ok, "node watch closed")

			node, isNode := event.Object.(*corev1.Node)
			if !isNode || !pending[node.Name] || node.Labels[labeler.DriverInstalledLabel] != labeler.LabelValueTrue {
				continue
			}

			last = time.Now()
			if first.IsZero() {
				first = last
			}

			delete(pending, node.Name)
		case <-timeout:
			require.FailNow(t, "nodes were not labeled", "unlabeled nodes: %v", pending)
		}
	}

	return last.Sub(first)
}

func writeKubeconfig(t *testing.T, config *rest.Config) string {
	t.Helper()

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"test": {
				Server:                   config.Host,
				CertificateAuthorityData: config.CAData,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"test": {
				ClientCertificateData: config.CertData,
				ClientKeyData:         config.KeyData,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"test": {Cluster: "test", AuthInfo: "test"},
		},
		CurrentContext: "test",
	}, kubeconfigPath))

	return kubeconfigPath
}
