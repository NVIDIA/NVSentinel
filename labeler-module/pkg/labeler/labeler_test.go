// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package labeler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
// source <(setup-envtest use -p env)
func TestLabeler_handlePodEvent(t *testing.T) {
	tests := []struct {
		name                string
		pod                 *corev1.Pod
		existingPods        []*corev1.Pod
		existingNode        *corev1.Node
		expectedDCGMLabel   string
		expectedDriverLabel string
	}{
		{
			name: "DCGM 4.x new deployment adds version label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM 3.x new deployment adds version label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM pod with non-DCGM image new deployment does not add label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/other:1.0.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "ready driver pod new deployment adds driver label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/driver:550.x",
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "true",
		},
		{
			name: "not ready driver pod new deployment does not add label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/driver:550.x",
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "both DCGM and driver pods new deployment add both labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/driver:550.x",
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "true",
		},
		{
			name: "node already has correct labels redeployment no update needed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
					},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "pod with no node assignment new deployment fails",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
		},
		{
			name: "DCGM upgrade from 3.x to 4.x updates label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:3.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "3.x",
					},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM downgrade from 4.x to 3.x updates label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.3.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
					},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
		},
		{
			name: "driver pod becomes not ready removes label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/driver:550.x",
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/driver:550.x",
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM pod deletion removes version label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "driver pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/driver:550.x",
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			testEnv := envtest.Environment{}
			timeout, poll := 30*time.Second, time.Second

			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer testEnv.Stop()

			cli, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create a client")

			ns, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"}}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create namespace")

			if tt.existingNode != nil {
				_, err := cli.CoreV1().Nodes().Create(ctx, tt.existingNode, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create node")
			}

			for _, pod := range tt.existingPods {
				po, err := cli.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create pod")

				po.Status = pod.Status
				_, err = cli.CoreV1().Pods(ns.Name).UpdateStatus(ctx, po, metav1.UpdateOptions{})
				require.NoError(t, err, "failed to update pod status")
			}

			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset")
			require.NoError(t, err)
			go func() {
				require.NoError(t, labeler.Run(ctx), "failed to run labeler")
			}()

			ok := cache.WaitForCacheSync(ctx.Done(), labeler.informerSynced)
			assert.True(t, ok, "failed to wait for cache sync")

			if tt.pod != nil {
				p, err := cli.CoreV1().Pods(ns.Name).Get(ctx, tt.pod.Name, metav1.GetOptions{})
				if err != nil && !errors.IsNotFound(err) {
					require.NoError(t, err, "failed to fetch pod")
				} else if err == nil {
					var noGracePeriod int64 = 0
					err := cli.CoreV1().Pods(ns.Name).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &noGracePeriod})
					require.NoError(t, err, "failed to delete existsing pod")

					require.Eventually(t, func() bool {
						_, err := cli.CoreV1().Pods(ns.Name).Get(ctx, tt.pod.Name, metav1.GetOptions{})
						require.Error(t, err, "pod is still running")

						return true
					}, timeout, poll, "failed waiting for pod to be deleted")
				}

				po, err := cli.CoreV1().Pods(ns.Name).Create(ctx, tt.pod, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create a pod")

				po.Status = tt.pod.Status
				_, err = cli.CoreV1().Pods(ns.Name).UpdateStatus(ctx, po, metav1.UpdateOptions{})
				require.NoError(t, err, "failed to update pod status")
			} else {
				for _, pod := range tt.existingPods {
					var noGracePeriod int64 = 0
					err := cli.CoreV1().Pods(ns.Name).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &noGracePeriod})
					require.NoError(t, err, "failed to delete existsing pod")

					require.Eventually(t, func() bool {
						_, err := cli.CoreV1().Pods(ns.Name).Get(ctx, pod.Name, metav1.GetOptions{})
						require.Error(t, err, "pod is still running")

						return true
					}, timeout, poll, "failed waiting for pod to be deleted")
				}
			}

			require.Eventually(t, func() bool {
				no, err := cli.CoreV1().Nodes().Get(ctx, tt.existingNode.Name, metav1.GetOptions{})
				require.NoError(t, err, "failed to fetch node")

				// Debug output to help diagnose failures
				t.Logf("Current node labels: %+v", no.Labels)
				t.Logf("Expected DCGM label: '%s', Expected driver label: '%s'", tt.expectedDCGMLabel, tt.expectedDriverLabel)

				if tt.expectedDCGMLabel != "" {
					if actualLabel, exists := no.Labels[DCGMVersionLabel]; !exists || actualLabel != tt.expectedDCGMLabel {
						t.Logf("DCGM label mismatch: expected='%s', actual='%s', exists=%v", tt.expectedDCGMLabel, actualLabel, exists)
						return false
					}
				} else {
					if _, exists := no.Labels[DCGMVersionLabel]; exists {
						t.Logf("DCGM label should not exist but found: %s", no.Labels[DCGMVersionLabel])
						return false
					}
				}

				if tt.expectedDriverLabel != "" {
					if actualLabel, exists := no.Labels[DriverInstalledLabel]; !exists || actualLabel != tt.expectedDriverLabel {
						t.Logf("Driver label mismatch: expected='%s', actual='%s', exists=%v", tt.expectedDriverLabel, actualLabel, exists)
						return false
					}
				} else {
					if _, exists := no.Labels[DriverInstalledLabel]; exists {
						t.Logf("Driver label should not exist but found: %s", no.Labels[DriverInstalledLabel])
						return false
					}
				}

				return true
			}, timeout, poll, "failed waiting for node label to be applied")
		})
	}
}
