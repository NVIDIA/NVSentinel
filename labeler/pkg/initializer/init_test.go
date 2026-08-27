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

	durations := make(map[string]time.Duration, len(tests))
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

			ctx, cancel := context.WithCancel(t.Context())
			runErr := make(chan error, 1)
			start := time.Now()
			go func() {
				runErr <- components.Labeler.Run(ctx)
			}()

			require.Eventually(t, func() bool {
				for _, nodeName := range nodeNames {
					node, getErr := adminClient.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
					if getErr != nil || node.Labels[labeler.DriverInstalledLabel] != labeler.LabelValueTrue {
						return false
					}
				}

				return true
			}, 20*time.Second, 50*time.Millisecond)
			durations[test.name] = time.Since(start)

			cancel()
			require.NoError(t, <-runErr)

			for _, nodeName := range nodeNames {
				require.NoError(t, adminClient.CoreV1().Nodes().Delete(
					t.Context(), nodeName, metav1.DeleteOptions{},
				))
			}
		})
	}

	lowRate := float64(nodeCount) / durations["low QPS"].Seconds()
	highRate := float64(nodeCount) / durations["high QPS"].Seconds()
	throughputRatio := highRate / lowRate
	t.Logf("initialized labeler throughput: low QPS=%.2f nodes/s, high QPS=%.2f nodes/s, ratio=%.2fx",
		lowRate, highRate, throughputRatio)
	assert.GreaterOrEqual(t, throughputRatio, 6.0)
	assert.LessOrEqual(t, throughputRatio, 14.0)
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
