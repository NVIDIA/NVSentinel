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

package labeler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
)

// kataEnabledLabel belongs to the labeler module. The tests use it to check that the
// NVIDIA labeler leaves it alone.
const kataEnabledLabel = "nvsentinel.dgxc.nvidia.com/kata.enabled"

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
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM 3.x new deployment adds version label",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM pod with non-DCGM image new deployment does not add label",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "ready driver pod new deployment adds driver label",
			pod: &corev1.Pod{
				Name:   "driver-pod",
				Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "true",
		},
		{
			name: "ready NVIDIADriver CRD pod adds driver label",
			pod: &corev1.Pod{
				Name: "crd-driver-pod",
				Labels: map[string]string{
					"app":                "nvidia-gpu-driver-ubuntu22.04-7d9f5c",
					driverComponentLabel: driverComponentValue,
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "true",
		},
		{
			name: "NVIDIADriver CRD pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					Name: "crd-driver-pod",
					Labels: map[string]string{
						"app":                "nvidia-gpu-driver-ubuntu22.04-7d9f5c",
						driverComponentLabel: driverComponentValue,
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
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "ready GKE driver installer pod adds driver label",
			pod: &corev1.Pod{
				Name:   "driver-installer-pod",
				Labels: map[string]string{"k8s-app": "nvidia-driver-installer"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "true",
		},
		{
			name: "not ready driver pod new deployment does not add label",
			pod: &corev1.Pod{
				Name:   "driver-pod",
				Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "both DCGM and driver pods new deployment add both labels",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "true",
		},
		{
			name: "node already has correct labels redeployment no update needed",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name: "test-node",
				Labels: map[string]string{
					DCGMVersionLabel: "4.x",
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "pod with no node assignment new deployment fails",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name:   "test-node",
				Labels: map[string]string{},
			},
		},
		{
			name: "DCGM upgrade from 3.x to 4.x updates label",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name: "test-node",
				Labels: map[string]string{
					DCGMVersionLabel: "3.x",
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
		},
		{
			name: "DCGM downgrade from 4.x to 3.x updates label",
			pod: &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
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
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name: "test-node",
				Labels: map[string]string{
					DCGMVersionLabel: "4.x",
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
		},
		{
			name: "driver pod becomes not ready removes label",
			pod: &corev1.Pod{
				Name:   "driver-pod",
				Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
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
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
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
				Name: "test-node",
				Labels: map[string]string{
					DCGMVersionLabel: "4.x",
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
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
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
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
		},
		{
			name: "driver installer pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					Name:   "driver-installer-pod",
					Labels: map[string]string{"k8s-app": "nvidia-driver-installer"},
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
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
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
			defer func() { _ = testEnv.Stop() }()

			cli, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create a client")

			ns, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: "gpu-operator"}, metav1.CreateOptions{})
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

			labeler, err := NewLabeler(
				cli,
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				false,
				false,
				false,
			)
			require.NoError(t, err)
			go func() {
				require.NoError(t, labeler.Run(ctx), "failed to run labeler")
			}()

			synced := cache.WaitForCacheSync(ctx.Done(), labeler.informersSynced...)
			assert.True(t, synced, "failed to wait for cache sync")

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

func TestDCGMBootstrapCompleted(t *testing.T) {
	tests := []struct {
		name                         string
		requireDCGMReadyForBootstrap bool
		dcgmPodReady                 bool
		existingBootstrapAnnotation  string
		existingDCGMVersionLabel     string
		deletePodAfterLabelSet       bool
		expectedDCGMLabel            string
		expectedBootstrapAnnotation  string
	}{
		{
			name:                         "not deployed if bootstrap not completed and DCGM not ready",
			requireDCGMReadyForBootstrap: true,
			dcgmPodReady:                 false,
			existingBootstrapAnnotation:  "",
			existingDCGMVersionLabel:     "",
			deletePodAfterLabelSet:       false,
			expectedDCGMLabel:            "",
			expectedBootstrapAnnotation:  "",
		},
		{
			name:                         "deployed if requireDCGMReadyForBootstrap false, bootstrap not completed, DCGM not ready",
			requireDCGMReadyForBootstrap: false,
			dcgmPodReady:                 false,
			existingBootstrapAnnotation:  "",
			existingDCGMVersionLabel:     "",
			deletePodAfterLabelSet:       false,
			expectedDCGMLabel:            "4.x",
			expectedBootstrapAnnotation:  "true",
		},
		{
			name:                         "deployed if bootstrap not completed and DCGM ready",
			requireDCGMReadyForBootstrap: true,
			dcgmPodReady:                 true,
			existingBootstrapAnnotation:  "",
			existingDCGMVersionLabel:     "",
			deletePodAfterLabelSet:       false,
			expectedDCGMLabel:            "4.x",
			expectedBootstrapAnnotation:  "true",
		},
		{
			name:                         "deployed if bootstrap completed and DCGM not ready",
			requireDCGMReadyForBootstrap: true,
			dcgmPodReady:                 false,
			existingBootstrapAnnotation:  "true",
			existingDCGMVersionLabel:     "",
			deletePodAfterLabelSet:       false,
			expectedDCGMLabel:            "4.x",
			expectedBootstrapAnnotation:  "true",
		},
		{
			name:                         "annotation added when label already set, bootstrap not completed, DCGM ready",
			requireDCGMReadyForBootstrap: true,
			dcgmPodReady:                 true,
			existingBootstrapAnnotation:  "",
			existingDCGMVersionLabel:     "4.x",
			deletePodAfterLabelSet:       false,
			expectedDCGMLabel:            "4.x",
			expectedBootstrapAnnotation:  "true",
		},
		{
			name:                         "annotation persists but DCGM label removed when DCGM pod deleted",
			requireDCGMReadyForBootstrap: true,
			dcgmPodReady:                 true,
			existingBootstrapAnnotation:  "",
			existingDCGMVersionLabel:     "4.x",
			deletePodAfterLabelSet:       true,
			expectedDCGMLabel:            "",
			expectedBootstrapAnnotation:  "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			kubeClient, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create K8s client")

			ns, err := kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				Name: "gpu-operator",
			}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create namespace")

			node := &corev1.Node{
				Name:        "test-node",
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
			}
			if tt.existingBootstrapAnnotation != "" {
				node.Annotations[DCGMBootstrapCompletedAnnotation] = tt.existingBootstrapAnnotation
			}
			if tt.existingDCGMVersionLabel != "" {
				node.Labels[DCGMVersionLabel] = tt.existingDCGMVersionLabel
			}

			_, err = kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			labeler, err := NewLabeler(
				kubeClient,
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				false,
				false,
				false,
			)
			require.NoError(t, err, "failed to create labeler")
			labeler.requireDCGMReadyForBootstrap = tt.requireDCGMReadyForBootstrap

			labelerCtx, labelerCancel := context.WithCancel(ctx)
			defer labelerCancel()

			go func() { _ = labeler.Run(labelerCtx) }()

			require.Eventually(t, func() bool {
				return labeler.allInformersSynced()
			}, 10*time.Second, 100*time.Millisecond, "informers did not sync")

			dcgmPodStatus := corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			}
			if tt.dcgmPodReady {
				dcgmPodStatus = corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				}
			}

			dcgmPod := &corev1.Pod{
				Name:   "dcgm-pod",
				Labels: map[string]string{"app": "nvidia-dcgm"},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{Name: "dcgm", Image: "nvcr.io/nvidia/dcgm:4.1.0"},
					},
				},
			}

			po, err := kubeClient.CoreV1().Pods(ns.Name).Create(ctx, dcgmPod, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create dcgm pod")
			po.Status = dcgmPodStatus
			_, err = kubeClient.CoreV1().Pods(ns.Name).UpdateStatus(ctx, po, metav1.UpdateOptions{})
			require.NoError(t, err, "failed to update dcgm pod status")

			if tt.deletePodAfterLabelSet {
				require.Eventually(t, func() bool {
					no, err := kubeClient.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
					if err != nil {
						return false
					}
					return no.Labels[DCGMVersionLabel] != "" &&
						no.Annotations[DCGMBootstrapCompletedAnnotation] != ""
				}, 15*time.Second, 500*time.Millisecond, "dcgm label and annotation should be set before pod deletion")

				var noGrace int64 = 0
				err = kubeClient.CoreV1().Pods(ns.Name).Delete(ctx, dcgmPod.Name, metav1.DeleteOptions{GracePeriodSeconds: &noGrace})
				require.NoError(t, err, "failed to delete dcgm pod")
			}

			require.Eventually(t, func() bool {
				no, err := kubeClient.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
				if err != nil {
					return false
				}
				dcgmLabel := no.Labels[DCGMVersionLabel]
				annotation := no.Annotations[DCGMBootstrapCompletedAnnotation]
				if dcgmLabel != tt.expectedDCGMLabel {
					t.Logf("waiting for DCGM label: want=%q got=%q", tt.expectedDCGMLabel, dcgmLabel)
					return false
				}
				if annotation != tt.expectedBootstrapAnnotation {
					t.Logf("waiting for bootstrap annotation: want=%q got=%q", tt.expectedBootstrapAnnotation, annotation)
					return false
				}
				return true
			}, 15*time.Second, 500*time.Millisecond, "node labels/annotations not as expected")
		})
	}
}

func TestAssumeDCGMAvailableWhenPodSourceMissing(t *testing.T) {
	tests := []struct {
		name          string
		existingLabel string
		preserve      bool
		wantLabel     string
	}{
		{
			name:          "preserves valid existing label when enabled",
			existingLabel: "3.x",
			preserve:      true,
			wantLabel:     "3.x",
		},
		{
			name:          "removes stale label when disabled",
			existingLabel: "3.x",
			preserve:      false,
			wantLabel:     "",
		},
		{
			name:          "does not preserve invalid label",
			existingLabel: "5.x",
			preserve:      true,
			wantLabel:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := NewLabeler(
				fake.NewSimpleClientset(),
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				false,
				false,
				tt.preserve,
			)
			require.NoError(t, err)

			node := &corev1.Node{Name: "test-node", Labels: map[string]string{}}
			if tt.existingLabel != "" {
				node.Labels[DCGMVersionLabel] = tt.existingLabel
			}

			l.setDriverAndDCGMLabelValues(node, "", "")

			if tt.wantLabel == "" {
				require.NotContains(t, node.Labels, DCGMVersionLabel)
			} else {
				require.Equal(t, tt.wantLabel, node.Labels[DCGMVersionLabel])
			}
		})
	}
}

func TestNewLabeler_InvalidLabelSelectors_ReturnsError(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	t.Run("invalid pod label selector", func(t *testing.T) {
		_, err := NewLabeler(clientset, time.Minute, "invalid(value", "driver", "gke", false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create pod informer")
	})

	t.Run("invalid GKE installer label selector", func(t *testing.T) {
		_, err := NewLabeler(clientset, time.Minute, "dcgm", "driver", "invalid(value", false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create GKE installer informer")
	})
}

func TestLabelerInformerTransforms_EndToEnd(t *testing.T) {
	ctx := context.Background()
	testEnv := envtest.Environment{}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	namespace, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		Name: "gpu-operator",
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Run("node and pod cache shapes", func(t *testing.T) {
		node, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
			Name: "transform-disabled",
			Labels: map[string]string{
				managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
				"retain":                "label",
			},
			Annotations: map[string]string{
				DCGMBootstrapCompletedAnnotation: "true",
				"drop":                           "annotation",
			},
			Spec: corev1.NodeSpec{ProviderID: "drop-provider"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		node.Status.Capacity = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
		}
		node, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		require.NoError(t, err)

		pod, err := cli.CoreV1().Pods(namespace.Name).Create(ctx, &corev1.Pod{
			Name:        "dcgm-pod",
			Namespace:   namespace.Name,
			Labels:      map[string]string{"app": "nvidia-dcgm"},
			Annotations: map[string]string{"drop": "annotation"},
			Spec: corev1.PodSpec{
				NodeName:           node.Name,
				ServiceAccountName: "drop-service-account",
				Containers: []corev1.Container{{
					Name:    "dcgm",
					Image:   "nvcr.io/nvidia/dcgm:4.1.0",
					Command: []string{"drop-command"},
				}},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		}
		pod, err = cli.CoreV1().Pods(namespace.Name).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)

		labeler, stop := startTransformTestLabeler(t, cli, false)

		pods, err := labeler.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, node.Name)
		require.NoError(t, err)
		require.Len(t, pods, 1)

		cachedPod, ok := pods[0].(*corev1.Pod)
		require.True(t, ok)
		assert.Equal(t, &corev1.Pod{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
			Labels:          pod.Labels,
			Spec: corev1.PodSpec{
				NodeName: pod.Spec.NodeName,
				Containers: []corev1.Container{{
					Image: pod.Spec.Containers[0].Image,
				}},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}, cachedPod)

		isReady, _ := isTargetPod(cachedPod, true, nil)
		assert.True(t, isReady)

		dcgmVersion, err := labeler.resolveDCGMVersionLabelValue(node, nil)
		require.NoError(t, err)
		assert.Equal(t, dcgmVersion4, dcgmVersion)

		cachedNode, err := labeler.getNodeFromCache(node.Name)
		require.NoError(t, err)
		assert.NotEmpty(t, cachedNode.ResourceVersion)
		assert.Equal(t, &corev1.Node{
			Name:            node.Name,
			UID:             node.UID,
			ResourceVersion: cachedNode.ResourceVersion,
			Labels:          node.Labels,
			Annotations: map[string]string{
				DCGMBootstrapCompletedAnnotation: "true",
			},
		}, cachedNode)

		stop()
		zeroGracePeriod := int64(0)
		require.NoError(t, cli.CoreV1().Pods(namespace.Name).Delete(
			ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zeroGracePeriod}))
		require.NoError(t, cli.CoreV1().Nodes().Delete(
			ctx, node.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			_, podErr := cli.CoreV1().Pods(namespace.Name).Get(
				ctx, pod.Name, metav1.GetOptions{})
			_, nodeErr := cli.CoreV1().Nodes().Get(
				ctx, node.Name, metav1.GetOptions{})

			return errors.IsNotFound(podErr) && errors.IsNotFound(nodeErr)
		}, 30*time.Second, 100*time.Millisecond)
	})
}

func startTransformTestLabeler(
	t *testing.T,
	cli kubernetes.Interface,
	assumeDriverInstalled bool,
) (*Labeler, func()) {
	t.Helper()

	labeler, err := NewLabeler(
		cli,
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		assumeDriverInstalled,
		false,
		false,
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	labeler.ctx = ctx

	go labeler.podInformer.Run(ctx.Done())
	go labeler.crdDriverInformer.Run(ctx.Done())
	go labeler.gkeInstallerInformer.Run(ctx.Done())
	go labeler.nodeInformer.Run(ctx.Done())

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
		})
	}
	t.Cleanup(stop)

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer syncCancel()
	require.True(t, cache.WaitForCacheSync(syncCtx.Done(), labeler.informersSynced...))
	labeler.reconcileAllNodes()

	return labeler, stop
}

func TestStaleLabelsRemoval(t *testing.T) {
	tests := []struct {
		name                  string
		existingNode          *corev1.Node
		existingPods          []*corev1.Pod
		expectedDriverLabel   string
		expectedDCGMLabel     string
		shouldHaveDriverLabel bool
		shouldHaveDCGMLabel   bool
	}{
		{
			name: "both stale labels removed when no pods exist",
			existingNode: &corev1.Node{
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
					DCGMVersionLabel:     "3.x",
				},
			},
			existingPods:          []*corev1.Pod{},
			shouldHaveDriverLabel: false,
			shouldHaveDCGMLabel:   false,
		},
		{
			name: "driver label retained when driver pod exists",
			existingNode: &corev1.Node{
				Name: "test-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
				},
			},
			existingPods: []*corev1.Pod{
				{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{Name: "driver", Image: "nvcr.io/nvidia/driver:550.x"},
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
			expectedDriverLabel:   "true",
			shouldHaveDriverLabel: true,
			shouldHaveDCGMLabel:   false,
		},
		{
			name: "DCGM label retained when DCGM pod exists",
			existingNode: &corev1.Node{
				Name: "test-node",
				Labels: map[string]string{
					DCGMVersionLabel: "4.x",
				},
			},
			existingPods: []*corev1.Pod{
				{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{Name: "dcgm", Image: "nvcr.io/nvidia/dcgm:4.1.0"},
						},
					},
				},
			},
			expectedDCGMLabel:     "4.x",
			shouldHaveDriverLabel: false,
			shouldHaveDCGMLabel:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			cli, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			ns, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: "gpu-operator"}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create namespace")

			_, err = cli.CoreV1().Nodes().Create(ctx, tt.existingNode, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			for _, pod := range tt.existingPods {
				po, err := cli.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create pod")

				po.Status = pod.Status
				_, err = cli.CoreV1().Pods(ns.Name).UpdateStatus(ctx, po, metav1.UpdateOptions{})
				require.NoError(t, err, "failed to update pod status")
			}

			labeler, err := NewLabeler(
				cli,
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				false,
				false,
				false,
			)
			require.NoError(t, err, "failed to create labeler")

			labelerCtx, labelerCancel := context.WithCancel(ctx)
			defer labelerCancel()

			go func() {
				_ = labeler.Run(labelerCtx)
			}()

			require.Eventually(t, func() bool {
				return labeler.allInformersSynced()
			}, 10*time.Second, 100*time.Millisecond, "labeler informers did not sync")

			// Wait for pods to be indexed in custom indexes before testing
			if len(tt.existingPods) > 0 {
				require.Eventually(t, func() bool {
					dcgmObjs, _ := labeler.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, tt.existingNode.Name)
					driverObjs, _ := labeler.podInformer.GetIndexer().ByIndex(NodeDriverIndex, tt.existingNode.Name)
					return len(dcgmObjs) > 0 || len(driverObjs) > 0
				}, 10*time.Second, 100*time.Millisecond, "pods not indexed in custom indexes")
			}

			// Assert the result of the startup pass in Run, without another reconcile.
			require.Eventually(t, func() bool {
				node, err := cli.CoreV1().Nodes().Get(ctx, tt.existingNode.Name, metav1.GetOptions{})
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				driverLabel, driverExists := node.Labels[DriverInstalledLabel]
				dcgmLabel, dcgmExists := node.Labels[DCGMVersionLabel]

				t.Logf("Node labels: driver=%q (exists=%v), dcgm=%q (exists=%v)",
					driverLabel, driverExists, dcgmLabel, dcgmExists)

				if tt.shouldHaveDriverLabel {
					if !driverExists || driverLabel != tt.expectedDriverLabel {
						return false
					}
				} else {
					if driverExists {
						t.Logf("Driver label should not exist but found: %s", driverLabel)
						return false
					}
				}

				if tt.shouldHaveDCGMLabel {
					if !dcgmExists || dcgmLabel != tt.expectedDCGMLabel {
						return false
					}
				} else {
					if dcgmExists {
						t.Logf("DCGM label should not exist but found: %s", dcgmLabel)
						return false
					}
				}

				return true
			}, 15*time.Second, 500*time.Millisecond, "labels not set correctly on node %s", tt.existingNode.Name)
		})
	}
}

func TestAssumeDriverInstalled(t *testing.T) {
	tests := []struct {
		name                  string
		assumeDriverInstalled bool
		existingNode          *corev1.Node
		expectedDriverLabel   string
		shouldHaveDriverLabel bool
	}{
		{
			name:                  "assume-driver-installed sets label on GPU node without driver pods",
			assumeDriverInstalled: true,
			existingNode: &corev1.Node{
				Name: "gpu-node",
				Labels: map[string]string{
					"nvidia.com/gpu.present": "true",
				},
			},
			expectedDriverLabel:   "true",
			shouldHaveDriverLabel: true,
		},
		{
			name:                  "assume-driver-installed skips non-GPU node",
			assumeDriverInstalled: true,
			existingNode: &corev1.Node{
				Name:   "control-plane-node",
				Labels: map[string]string{},
			},
			shouldHaveDriverLabel: false,
		},
		{
			name:                  "assume-driver-installed preserves label on GPU node reconciliation",
			assumeDriverInstalled: true,
			existingNode: &corev1.Node{
				Name: "gpu-node",
				Labels: map[string]string{
					"nvidia.com/gpu.present": "true",
					DriverInstalledLabel:     "true",
				},
			},
			expectedDriverLabel:   "true",
			shouldHaveDriverLabel: true,
		},
		{
			name:                  "without flag stale label is removed when no driver pods",
			assumeDriverInstalled: false,
			existingNode: &corev1.Node{
				Name: "gpu-node",
				Labels: map[string]string{
					DriverInstalledLabel: "true",
				},
			},
			shouldHaveDriverLabel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			cli, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			_, err = cli.CoreV1().Nodes().Create(ctx, tt.existingNode, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			labeler, err := NewLabeler(
				cli,
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				tt.assumeDriverInstalled,
				false,
				false,
			)
			require.NoError(t, err, "failed to create labeler")

			labelerCtx, labelerCancel := context.WithCancel(ctx)
			defer labelerCancel()

			go func() {
				_ = labeler.Run(labelerCtx)
			}()

			require.Eventually(t, func() bool {
				return labeler.allInformersSynced()
			}, 10*time.Second, 100*time.Millisecond, "labeler informers did not sync")

			err = labeler.handleNodeEvent(tt.existingNode)
			require.NoError(t, err, "failed to handle node event")

			require.Eventually(t, func() bool {
				node, err := cli.CoreV1().Nodes().Get(ctx, tt.existingNode.Name, metav1.GetOptions{})
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				driverLabel, driverExists := node.Labels[DriverInstalledLabel]
				t.Logf("Node labels: driver=%q (exists=%v)", driverLabel, driverExists)

				if tt.shouldHaveDriverLabel {
					return driverExists && driverLabel == tt.expectedDriverLabel
				}
				return !driverExists
			}, 15*time.Second, 500*time.Millisecond, "driver label not set correctly on node %s", tt.existingNode.Name)
		})
	}
}

// errNodeLister is a NodeLister that always returns a non-NotFound error from Get,
// used to exercise the fail-closed paths in calculateAndSetNodeLabels and
// reconcileNodeLabels.
type errNodeLister struct{}

func (e errNodeLister) List(_ labels.Selector) ([]*corev1.Node, error) { return nil, nil }

func (e errNodeLister) Get(_ string) (*corev1.Node, error) {
	return nil, fmt.Errorf("simulated lister error")
}

// nodeIndexerLister builds a NodeLister backed by a fake cache.Indexer containing nodes.
func nodeIndexerLister(nodes ...*corev1.Node) listersv1.NodeLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, n := range nodes {
		_ = indexer.Add(n)
	}
	return listersv1.NewNodeLister(indexer)
}

func TestCalculateAndSetNodeLabels_ManagedGate(t *testing.T) {
	t.Run("strips detection labels when node is opted out", func(t *testing.T) {
		node := &corev1.Node{
			Name: "opted-out-node",
			Labels: map[string]string{
				managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
				DCGMVersionLabel:        "3.x",
				DriverInstalledLabel:    "true",
				kataEnabledLabel:        "true",
			},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}

		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, nil)
		require.NoError(t, err)

		assert.True(t, changed, "should report labels changed")
		assert.NotContains(t, target.Labels, DCGMVersionLabel, "DCGM label must be stripped")
		assert.NotContains(t, target.Labels, DriverInstalledLabel, "driver label must be stripped")
		assert.Equal(t, "true", target.Labels[kataEnabledLabel], "the labeler owns the kata label")
		// The managed label itself is owned by the ERR reconciler — labeler must not touch it.
		assert.Equal(t, managed.ManagedLabelValueFalse, target.Labels[managed.ManagedLabelKey])
	})

	t.Run("no-op when opted-out node has no detection labels", func(t *testing.T) {
		node := &corev1.Node{
			Name:   "opted-out-clean",
			Labels: map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}
		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, nil)
		require.NoError(t, err)
		assert.False(t, changed, "no labels to strip → no update needed")
	})

	t.Run("fail closed — returns false without mutating on lister error", func(t *testing.T) {
		node := &corev1.Node{
			Name:   "error-node",
			Labels: map[string]string{DCGMVersionLabel: "3.x", DriverInstalledLabel: "true"},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: errNodeLister{},
		}
		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, nil)
		require.NoError(t, err)

		assert.False(t, changed, "lister error must not report changes")
		assert.Contains(t, target.Labels, DCGMVersionLabel, "lister error must not strip labels")
		assert.Contains(t, target.Labels, DriverInstalledLabel, "lister error must not strip labels")
	})
}

func TestReconcileNodeLabels_ManagedGate(t *testing.T) {
	readyDriverPod := &corev1.Pod{
		Name:   "driver-pod",
		UID:    "driver-pod-uid",
		Labels: map[string]string{"app": "nvidia-driver-daemonset"},
		Spec:   corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}

	t.Run("does not stamp labels on an opted-out node", func(t *testing.T) {
		node := &corev1.Node{
			Name:   "test-node",
			Labels: map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse},
		}
		l, clientset := newLabelerWithCachedDriverPods(t, node, readyDriverPod)

		require.NoError(t, l.handlePodEvent(readyDriverPod))

		updated, err := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		assert.NotContains(t, updated.Labels, DriverInstalledLabel,
			"driver label must not be stamped on an opted-out node")
		assert.NotContains(t, updated.Labels, DCGMVersionLabel,
			"DCGM label must not be stamped on an opted-out node")
	})

	t.Run("fail closed — skips update on lister error", func(t *testing.T) {
		node := &corev1.Node{Name: "test-node", Labels: map[string]string{}}
		l, clientset := newLabelerWithCachedDriverPods(t, node, readyDriverPod)
		l.nodeLister = errNodeLister{}

		require.NoError(t, l.handlePodEvent(readyDriverPod), "fail-closed skip must not propagate as an error")

		updated, err := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		assert.NotContains(t, updated.Labels, DriverInstalledLabel,
			"lister error must not stamp labels on node")
	})
}

// TestReconcileNodeLabels_InformersNotSynced_SkipsWithoutAPICalls covers events that
// arrive before the informers have synced. A pod that the cache has not listed yet
// would look like a removed driver, so the event must not write, and must not read
// the node from the API server either. Run reconciles every node after the sync.
func TestReconcileNodeLabels_InformersNotSynced_SkipsWithoutAPICalls(t *testing.T) {
	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{DriverInstalledLabel: LabelValueTrue, DCGMVersionLabel: "4.x"},
	}
	l, clientset := newLabelerWithCachedDriverPods(t, node)
	l.informersSynced = []cache.InformerSynced{func() bool { return false }}
	fakeClient := clientset.(*fake.Clientset)
	fakeClient.ClearActions()

	require.NoError(t, l.reconcileNodeLabels(node.Name, nil))

	assert.Empty(t, fakeClient.Actions())
}

func TestNodeRequiresReconciliation(t *testing.T) {
	base := &corev1.Node{Name: "node-a", Labels: map[string]string{"other": "a"}}

	tests := []struct {
		name   string
		mutate func(*corev1.Node)
		want   bool
	}{
		{name: "unrelated label change", mutate: func(n *corev1.Node) { n.Labels["other"] = "b" }, want: false},
		{name: "kata label change", mutate: func(n *corev1.Node) { n.Labels[kataEnabledLabel] = "true" }, want: false},
		{
			name:   "managed label change",
			mutate: func(n *corev1.Node) { n.Labels[managed.ManagedLabelKey] = managed.ManagedLabelValueFalse },
			want:   true,
		},
		{name: "gpu-present label change", mutate: func(n *corev1.Node) { n.Labels[gpuPresentLabel] = LabelValueTrue }, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tt.mutate(updated)
			assert.Equal(t, tt.want, nodeRequiresReconciliation(base, updated))
		})
	}
}

// TestReconcileNodeLabels_ConflictRetry_RecalculatesFromLiveNode verifies that a patch
// that hits a conflict recalculates the labels from the node as the API server has it.
func TestReconcileNodeLabels_ConflictRetry_RecalculatesFromLiveNode(t *testing.T) {
	// The stale DCGM label makes the first attempt patch, so that it hits the conflict.
	cachedNode := &corev1.Node{Name: "node-a", Labels: map[string]string{DCGMVersionLabel: "4.x"}}
	l, clientset := newLabelerWithCachedDriverPods(t, cachedNode)
	l.assumeDriverInstalled = true

	liveNode, err := clientset.CoreV1().Nodes().Get(context.Background(), cachedNode.Name, metav1.GetOptions{})
	require.NoError(t, err)
	liveNode.Labels[gpuPresentLabel] = LabelValueTrue
	_, err = clientset.CoreV1().Nodes().Update(context.Background(), liveNode, metav1.UpdateOptions{})
	require.NoError(t, err)

	patchAttempts := 0
	clientset.(*fake.Clientset).PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		patchAttempts++
		if patchAttempts == 1 {
			return true, nil, errors.NewConflict(
				schema.GroupResource{Resource: "nodes"}, cachedNode.Name, fmt.Errorf("simulated conflict"))
		}

		return false, nil, nil
	})

	require.NoError(t, l.reconcileNodeLabels(cachedNode.Name, nil))
	require.Equal(t, 2, patchAttempts)

	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), cachedNode.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, LabelValueTrue, updated.Labels[DriverInstalledLabel])
	require.NotContains(t, updated.Labels, DCGMVersionLabel)
}

// newLabelerWithCachedDriverPods builds a Labeler with manually controlled informer
// stores. The driver pods exist only in the pod cache, allowing the test to represent
// a cache that has not yet observed their deletion.
func newLabelerWithCachedDriverPods(t *testing.T, node *corev1.Node,
	cachedDriverPods ...*corev1.Pod) (*Labeler, kubernetes.Interface) {
	t.Helper()

	podInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{
		NodeDCGMIndex:   podNodeIndexerByLabel("app", "nvidia-dcgm"),
		NodeDriverIndex: podNodeIndexerByLabel("app", "nvidia-driver-daemonset"),
	})

	for _, pod := range cachedDriverPods {
		require.NoError(t, podInformer.GetIndexer().Add(pod))
	}

	nodeInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	require.NoError(t, nodeInformer.GetStore().Add(node))

	emptyPodInformer := func(index string, indexFunc cache.IndexFunc) cache.SharedIndexInformer {
		return cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0,
			cache.Indexers{index: indexFunc})
	}

	clientset := fake.NewSimpleClientset(node)

	return &Labeler{
		ctx:          context.Background(),
		clientset:    clientset,
		podInformer:  podInformer,
		nodeInformer: nodeInformer,
		nodeLister:   listersv1.NewNodeLister(nodeInformer.GetIndexer()),
		crdDriverInformer: emptyPodInformer(NodeDriverIndex,
			podNodeIndexerByLabel(driverComponentLabel, driverComponentValue)),
		gkeInstallerInformer: emptyPodInformer(NodeGKEDriverInstallerIndex,
			podNodeIndexerByLabel("k8s-app", "nvidia-driver-installer")),
		// Pod-derived labels are reconciled only after every informer has synced.
		informersSynced: []cache.InformerSynced{func() bool { return true }},
	}, clientset
}

// TestReconcileNodeLabels_DriverPodDeletedDuringReconcile_DeleteWins verifies that a
// startup reconcile cannot overwrite a concurrent pod-deletion result.
func TestReconcileNodeLabels_DriverPodDeletedDuringReconcile_DeleteWins(t *testing.T) {
	const nodeName = "test-node"

	node := &corev1.Node{Name: nodeName, Labels: map[string]string{}}
	pod := &corev1.Pod{
		Name:      "driver-pod",
		Namespace: "default",
		UID:       "driver-pod-uid",
		Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
		Spec:      corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	l, clientset := newLabelerWithCachedDriverPods(t, node, pod)
	fakeClient := clientset.(*fake.Clientset)

	reconcileReachedPatch := make(chan struct{})
	releaseReconcile := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReconcile) }) }
	// Unblocks the reconcile goroutine if an assertion fails while it waits in the reactor.
	t.Cleanup(release)
	var blocked atomic.Bool
	fakeClient.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if blocked.CompareAndSwap(false, true) {
			close(reconcileReachedPatch)
			<-releaseReconcile
		}

		return false, nil, nil
	})

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- l.reconcileNodeLabels(nodeName, nil)
	}()

	select {
	case <-reconcileReachedPatch:
	case err := <-reconcileDone:
		t.Fatalf("reconcile returned without patching: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile did not reach the patch")
	}

	// Informer stores remove an object before invoking its delete handler.
	require.NoError(t, l.podInformer.GetIndexer().Delete(pod))
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- l.handlePodDeleteEvent(pod) }()

	select {
	case err := <-deleteDone:
		t.Fatalf("pod deletion was not serialized with node reconciliation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	require.NoError(t, <-reconcileDone)
	require.NoError(t, <-deleteDone)

	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Labels, DriverInstalledLabel)
}

// TestReconcileNodeLabels_CachedNode_UsesPatchOnly pins the API-call budget and payload
// of the label write path.
func TestReconcileNodeLabels_CachedNode_UsesPatchOnly(t *testing.T) {
	const nodeName = "gpu-node"

	clientset := fake.NewSimpleClientset(&corev1.Node{
		Name:            nodeName,
		ResourceVersion: "1",
		Labels:          map[string]string{gpuPresentLabel: LabelValueTrue},
	})

	labeler, _ := startTransformTestLabeler(t, clientset, true)
	requireCachedLabel(t, labeler, nodeName, DriverInstalledLabel, LabelValueTrue)

	tests := []struct {
		name          string
		setup         func(*testing.T)
		expectedPatch string
	}{
		{
			name: "label change costs one merge patch",
			setup: func(t *testing.T) {
				node, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
				require.NoError(t, getErr)
				delete(node.Labels, DriverInstalledLabel)
				_, updateErr := clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, updateErr)
				requireCachedLabelAbsent(t, labeler, nodeName, DriverInstalledLabel)
			},
			expectedPatch: fmt.Sprintf(
				`{"metadata":{"labels":{%q:%q}}}`,
				DriverInstalledLabel,
				LabelValueTrue,
			),
		},
		{
			name: "no-op reconcile costs no API call",
			setup: func(t *testing.T) {
				requireCachedLabel(t, labeler, nodeName, DriverInstalledLabel, LabelValueTrue)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			clientset.ClearActions()

			require.NoError(t, labeler.reconcileNodeLabels(nodeName, nil))

			expectedPatchCount := 0
			if tt.expectedPatch != "" {
				expectedPatchCount = 1
				action := nodePatchAction(t, clientset.Actions())
				assert.Equal(t, types.MergePatchType, action.GetPatchType())
				assert.JSONEq(t, tt.expectedPatch, string(action.GetPatch()))
			}

			assert.Equal(t, expectedPatchCount, countNodeActions(clientset, "patch"))
			assert.Equal(t, 0, countNodeActions(clientset, "get"))
			assert.Equal(t, 0, countNodeActions(clientset, "update"))

			updated, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, LabelValueTrue, updated.Labels[DriverInstalledLabel], "the desired label is present")
			assert.Equal(t, LabelValueTrue, updated.Labels[gpuPresentLabel],
				"a merge patch must leave labels it does not mention alone")
		})
	}
}

func TestReconcileNodeLabels_DelayedInformerEvent_EventuallyConverges(t *testing.T) {
	const nodeName = "delayed-node"

	initial := &corev1.Node{
		Name:   nodeName,
		Labels: map[string]string{gpuPresentLabel: "false"},
	}

	// A controlled fake watch lets the API object advance independently of the
	// informer cache, making the stale-cache checkpoints deterministic.
	clientset := fake.NewSimpleClientset(initial)
	nodeWatch := watch.NewRaceFreeFake()
	clientset.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nodeWatch, nil
	})

	labeler, _ := startTransformTestLabeler(t, clientset, true)
	requireCachedLabel(t, labeler, nodeName, gpuPresentLabel, "false")

	// Change the API object while withholding its watch event. The informer is synced,
	// but its node projection deliberately remains one generation behind.
	liveNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	liveNode.Labels[gpuPresentLabel] = LabelValueTrue
	liveNode, err = clientset.CoreV1().Nodes().Update(t.Context(), liveNode, metav1.UpdateOptions{})
	require.NoError(t, err)

	cachedNode, err := labeler.getNodeFromCache(nodeName)
	require.NoError(t, err)
	assert.Equal(t, "false", cachedNode.Labels[gpuPresentLabel])
	assert.NotContains(t, liveNode.Labels, DriverInstalledLabel)

	// A reconcile against the stale projection computes the stale derived value and
	// correctly emits no write.
	clientset.ClearActions()
	require.NoError(t, labeler.reconcileNodeLabels(nodeName, nil))
	assert.Empty(t, clientset.Actions())

	// Deliver the delayed input event. Its handler must reconcile from the new cache
	// generation and correct the derived label.
	nodeWatch.Modify(liveNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, gpuPresentLabel, LabelValueTrue)

	require.Eventually(t, func() bool {
		node, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
		return getErr == nil && node.Labels[DriverInstalledLabel] == LabelValueTrue
	}, 10*time.Second, 10*time.Millisecond)

	finalNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	nodeWatch.Modify(finalNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, DriverInstalledLabel, LabelValueTrue)
}

func countNodeActions(clientset *fake.Clientset, verb string) int {
	count := 0

	for _, action := range clientset.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "nodes" {
			count++
		}
	}

	return count
}

func nodePatchAction(t *testing.T, actions []k8stesting.Action) k8stesting.PatchAction {
	t.Helper()

	for _, action := range actions {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "nodes" {
			patchAction, ok := action.(k8stesting.PatchAction)
			require.True(t, ok)

			return patchAction
		}
	}

	require.FailNow(t, "node PATCH action not found")
	return nil
}

// requireCachedLabel waits for the informer cache to show the given label, which is
// how a test tells that the labeler has finished reacting to earlier events.
func requireCachedLabel(t *testing.T, l *Labeler, nodeName, key, value string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := l.getNodeFromCache(nodeName)
		return err == nil && node.Labels[key] == value
	}, 10*time.Second, 10*time.Millisecond, "cache never showed %s=%s", key, value)
}

func requireCachedLabelAbsent(t *testing.T, l *Labeler, nodeName, key string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := l.getNodeFromCache(nodeName)
		if err != nil {
			return false
		}

		_, exists := node.Labels[key]

		return !exists
	}, 10*time.Second, 10*time.Millisecond, "cache never dropped %s", key)
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name       string
		conditions []corev1.PodCondition
		want       bool
	}{
		{name: "no conditions is not ready", want: false},
		{
			name:       "ready condition true is ready",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			want:       true,
		},
		{
			name:       "ready condition false is not ready",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			want:       false,
		},
		{
			name:       "ready condition unknown is not ready",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionUnknown}},
			want:       false,
		},
		{
			name: "other true conditions without ready is not ready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			want: false,
		},
		{
			name: "ready condition true after other conditions is ready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: tt.conditions}}
			assert.Equal(t, tt.want, isPodReady(pod))
		})
	}
}

// TestPodEventHandlers_ImageChangeWithoutReadinessChange_UpdatesDCGMLabel covers an
// in-place DCGM image change on a bootstrapped node whose DCGM pod stays not ready.
func TestPodEventHandlers_ImageChangeWithoutReadinessChange_UpdatesDCGMLabel(t *testing.T) {
	node := &corev1.Node{
		Name:        "test-node",
		Labels:      map[string]string{DCGMVersionLabel: "3.x"},
		Annotations: map[string]string{DCGMBootstrapCompletedAnnotation: "true"},
	}
	oldPod := &corev1.Pod{
		Name:   "dcgm-pod",
		UID:    "dcgm-pod-uid",
		Labels: map[string]string{"app": "nvidia-dcgm"},
		Spec: corev1.PodSpec{
			NodeName:   node.Name,
			Containers: []corev1.Container{{Image: "nvcr.io/nvidia/dcgm:3.3.0"}},
		},
	}
	newPod := oldPod.DeepCopy()
	newPod.Spec.Containers[0].Image = "nvcr.io/nvidia/dcgm:4.1.0"

	l, clientset := newLabelerWithCachedDriverPods(t, node, newPod)
	l.getEventHandlers().OnUpdate(oldPod, newPod)

	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "4.x", updated.Labels[DCGMVersionLabel])
}

// TestPodEventHandlers_DeleteTombstone_RemovesDriverLabel covers a driver pod delete
// that the watch missed. The informer delivers it as a tombstone.
func TestPodEventHandlers_DeleteTombstone_RemovesDriverLabel(t *testing.T) {
	node := &corev1.Node{Name: "test-node", Labels: map[string]string{DriverInstalledLabel: LabelValueTrue}}
	pod := &corev1.Pod{
		Name:   "driver-pod",
		UID:    "driver-pod-uid",
		Labels: map[string]string{"app": "nvidia-driver-daemonset"},
		Spec:   corev1.PodSpec{NodeName: node.Name},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}

	// The pod is already gone from the cache, as after a relist.
	l, clientset := newLabelerWithCachedDriverPods(t, node)
	tombstone := cache.DeletedFinalStateUnknown{Key: "default/driver-pod", Obj: pod}

	handler := cache.FilteringResourceEventHandler{FilterFunc: isScheduledPod, Handler: l.getEventHandlers()}
	handler.OnDelete(tombstone)

	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Labels, DriverInstalledLabel)
}
