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
)

func TestInitializeKubernetesClient_RateLimitScenarios_InitializedInformersUseConfiguredQPS(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	testConfig, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })

	adminClient, err := kubernetes.NewForConfig(testConfig)
	require.NoError(t, err)

	kubeconfigPath := writeNodeDrainerKubeconfig(t, testConfig)

	tests := []struct {
		name   string
		prefix string
		qps    float64
	}{
		{name: "low QPS", prefix: "low-qps", qps: 4},
		{name: "high QPS", prefix: "high-qps", qps: 40},
	}

	const podCount = 10

	durations := make(map[string]time.Duration, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			namespace := fmt.Sprintf("%s-%d", test.prefix, time.Now().UnixNano())
			_, createErr := adminClient.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			}, metav1.CreateOptions{})
			require.NoError(t, createErr)

			nodeNames := make([]string, podCount)
			for idx := range podCount {
				nodeNames[idx] = fmt.Sprintf("%s-node-%d", test.prefix, idx)
				_, createErr = adminClient.CoreV1().Pods(namespace).Create(t.Context(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", test.prefix, idx)},
					Spec: corev1.PodSpec{
						NodeName:   nodeNames[idx],
						Containers: []corev1.Container{{Name: "workload", Image: "example.invalid/workload"}},
					},
				}, metav1.CreateOptions{})
				require.NoError(t, createErr)
			}

			clientset, _, initErr := initializeKubernetesClient(InitializationParams{
				KubeconfigPath: kubeconfigPath,
				KubernetesClientRateLimits: kubeclient.RateLimitConfig{
					QPS:   test.qps,
					Burst: 1,
				},
			})
			require.NoError(t, initErr)

			notReadyTimeoutMinutes := 10
			initializedInformers, initErr := initializeInformers(
				clientset, &notReadyTimeoutMinutes, false, false, "kube-system",
			)
			require.NoError(t, initErr)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			require.NoError(t, initializedInformers.Run(ctx))

			start := time.Now()
			for _, nodeName := range nodeNames {
				require.NoError(t, initializedInformers.EvictAllPodsInImmediateMode(
					ctx, namespace, nodeName, 0, nil,
				))
			}
			durations[test.name] = time.Since(start)
		})
	}

	lowRate := float64(podCount) / durations["low QPS"].Seconds()
	highRate := float64(podCount) / durations["high QPS"].Seconds()
	throughputRatio := highRate / lowRate
	t.Logf("initialized eviction throughput: low QPS=%.2f pods/s, high QPS=%.2f pods/s, ratio=%.2fx",
		lowRate, highRate, throughputRatio)
	assert.GreaterOrEqual(t, throughputRatio, 6.0)
	assert.LessOrEqual(t, throughputRatio, 14.0)
}

func writeNodeDrainerKubeconfig(t *testing.T, config *rest.Config) string {
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
