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

package labeler

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
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
			name: "ready NVIDIADriver CRD pod adds driver label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "crd-driver-pod",
					Labels: map[string]string{
						"app":                "nvidia-gpu-driver-ubuntu22.04-7d9f5c",
						driverComponentLabel: driverComponentValue,
					},
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
			name: "NVIDIADriver CRD pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "crd-driver-pod",
						Labels: map[string]string{
							"app":                "nvidia-gpu-driver-ubuntu22.04-7d9f5c",
							driverComponentLabel: driverComponentValue,
						},
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
			name: "ready GKE driver installer pod adds driver label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-installer-pod",
					Labels: map[string]string{"k8s-app": "nvidia-driver-installer"},
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
		{
			name: "driver installer pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-installer-pod",
						Labels: map[string]string{"k8s-app": "nvidia-driver-installer"},
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
			defer func() { _ = testEnv.Stop() }()

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

			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", "nvidia-driver-installer", "", false, false,
				devicecounts.Config{}, false)
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
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
			}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create namespace")

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Labels:      make(map[string]string),
					Annotations: make(map[string]string),
				},
			}
			if tt.existingBootstrapAnnotation != "" {
				node.Annotations[DCGMBootstrapCompletedAnnotation] = tt.existingBootstrapAnnotation
			}
			if tt.existingDCGMVersionLabel != "" {
				node.Labels[DCGMVersionLabel] = tt.existingDCGMVersionLabel
			}

			_, err = kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			labeler, err := NewLabeler(kubeClient, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset",
				"nvidia-driver-installer", "", false, false,
				devicecounts.Config{}, false)
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
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
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
				"",
				false,
				false,
				devicecounts.Config{},
				tt.preserve,
			)
			require.NoError(t, err)

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node", Labels: map[string]string{}}}
			if tt.existingLabel != "" {
				node.Labels[DCGMVersionLabel] = tt.existingLabel
			}

			l.updateDriverAndDCGMLabels(node, "", "")

			if tt.wantLabel == "" {
				require.NotContains(t, node.Labels, DCGMVersionLabel)
			} else {
				require.Equal(t, tt.wantLabel, node.Labels[DCGMVersionLabel])
			}
		})
	}
}

// TestKataLabelOverride verifies that the kataLabelOverride parameter correctly
// adds custom kata detection labels to the labeler instance.
func TestKataLabelOverride(t *testing.T) {
	tests := []struct {
		name       string
		override   string
		wantLabels []string
	}{
		{
			name:       "no override - default only",
			override:   "",
			wantLabels: []string{KataRuntimeDefaultLabel},
		},
		{
			name:       "override equals default",
			override:   KataRuntimeDefaultLabel,
			wantLabels: []string{KataRuntimeDefaultLabel},
		},
		{
			name:       "with custom override",
			override:   "custom.io/kata-enabled",
			wantLabels: []string{KataRuntimeDefaultLabel, "custom.io/kata-enabled"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			clientset, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			l, err := NewLabeler(
				clientset,
				time.Minute,
				"nvidia-dcgm",
				"nvidia-driver-daemonset",
				"nvidia-driver-installer",
				tt.override,
				false,
				false,
				devicecounts.Config{},
				false,
			)

			if err != nil {
				t.Fatalf("NewLabeler() error = %v", err)
			}

			if l == nil {
				t.Fatal("NewLabeler() returned nil labeler")
			}

			require.Equal(t, tt.wantLabels, l.kataLabels, "kataLabels mismatch for override %q", tt.override)
		})
	}
}

func TestNewLabeler_InvalidLabelSelectors_ReturnsError(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	t.Run("invalid pod label selector", func(t *testing.T) {
		_, err := NewLabeler(clientset, time.Minute, "invalid(value", "driver", "gke", "", false, false,
			devicecounts.Config{}, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create pod informer")
	})

	t.Run("invalid GKE installer label selector", func(t *testing.T) {
		_, err := NewLabeler(clientset, time.Minute, "dcgm", "driver", "invalid(value", "", false, false,
			devicecounts.Config{}, false)
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
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Run("default cache shape and pod transform", func(t *testing.T) {
		node, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "transform-disabled",
				Labels: map[string]string{
					managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
					"retain":                "label",
				},
				Annotations: map[string]string{
					DCGMBootstrapCompletedAnnotation: "true",
					"drop":                           "annotation",
				},
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
			ObjectMeta: metav1.ObjectMeta{
				Name:        "dcgm-pod",
				Namespace:   namespace.Name,
				Labels:      map[string]string{"app": "nvidia-dcgm"},
				Annotations: map[string]string{"drop": "annotation"},
			},
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

		labeler, stop := startTransformTestLabeler(t, cli, devicecounts.Config{})

		pods, err := labeler.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, node.Name)
		require.NoError(t, err)
		require.Len(t, pods, 1)

		cachedPod, ok := pods[0].(*corev1.Pod)
		require.True(t, ok)
		assert.Equal(t, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            pod.Name,
				Namespace:       pod.Namespace,
				UID:             pod.UID,
				ResourceVersion: pod.ResourceVersion,
				Labels:          pod.Labels,
			},
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

		dcgmVersion, err := labeler.getDCGMVersionForNode(node.Name, nil)
		require.NoError(t, err)
		assert.Equal(t, dcgmVersion4, dcgmVersion)

		cachedNode, err := labeler.getNodeFromCache(node.Name)
		require.NoError(t, err)
		assert.Equal(t, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:            node.Name,
				UID:             node.UID,
				ResourceVersion: node.ResourceVersion,
				Labels:          node.Labels,
				Annotations: map[string]string{
					DCGMBootstrapCompletedAnnotation: "true",
				},
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

	t.Run("device count fields drive peer learning", func(t *testing.T) {
		nodes := make([]*corev1.Node, 0, 2)
		for name, count := range map[string]string{
			"transform-peer":   "8",
			"transform-target": "4",
		} {
			node, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        name,
					Annotations: map[string]string{"drop": "annotation"},
				},
				Spec: corev1.NodeSpec{ProviderID: "drop-provider"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			node.Status = corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(count),
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(count),
				},
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
			}
			node, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)

			nodes = append(nodes, node)
		}

		config := deviceCountConfigWithExpression(
			"int(node.status.allocatable['nvidia.com/gpu'])",
		)
		labeler, stop := startTransformTestLabeler(t, cli, config)

		require.Eventually(t, func() bool {
			node, err := cli.CoreV1().Nodes().Get(
				ctx, "transform-target", metav1.GetOptions{})
			if err != nil {
				return false
			}

			return node.Labels["test.nvsentinel/current"] == "4" &&
				node.Labels["test.nvsentinel/expected"] == "8"
		}, 30*time.Second, 100*time.Millisecond)

		for _, node := range nodes {
			cachedNode, err := labeler.getNodeFromCache(node.Name)
			require.NoError(t, err)
			assert.Equal(t, node.Status.Allocatable, cachedNode.Status.Allocatable)
			assert.Equal(t, node.Status.Capacity, cachedNode.Status.Capacity)
			assert.Empty(t, cachedNode.Annotations)
			assert.Empty(t, cachedNode.Spec)
			assert.Nil(t, cachedNode.Status.Conditions)
		}

		stop()
	})
}

func startTransformTestLabeler(
	t *testing.T,
	cli kubernetes.Interface,
	deviceCountConfig devicecounts.Config,
) (*Labeler, func()) {
	t.Helper()

	labeler, err := NewLabeler(
		cli,
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"",
		false,
		false,
		deviceCountConfig,
		false,
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	labeler.ctx = ctx

	go labeler.podInformer.Run(ctx.Done())
	go labeler.crdDriverInformer.Run(ctx.Done())
	go labeler.gkeInstallerInformer.Run(ctx.Done())
	go labeler.nodeInformer.Run(ctx.Done())
	if labeler.resourceSliceInformer != nil {
		go labeler.resourceSliceInformer.Run(ctx.Done())
	}

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

func TestNewLabeler_ResourceSliceInformerEnabled(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	t.Run("disabled config does not create ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"nvidia-dcgm",
			"nvidia-driver-daemonset",
			"nvidia-driver-installer",
			"",
			false,
			false,
			devicecounts.Config{},
			false,
		)
		require.NoError(t, err)
		require.Nil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 4)
	})

	t.Run("node-only enabled config does not create ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"nvidia-dcgm",
			"nvidia-driver-daemonset",
			"nvidia-driver-installer",
			"",
			false,
			false,
			testDeviceCountConfig(),
			false,
		)
		require.NoError(t, err)
		require.Nil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 4)
	})

	t.Run("ResourceSlice expression creates ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"nvidia-dcgm",
			"nvidia-driver-daemonset",
			"nvidia-driver-installer",
			"",
			false,
			false,
			testResourceSliceDeviceCountConfig(),
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 5)
	})
}

func TestLabelerNodeRequiresReconciliation_DeviceCountLabels(t *testing.T) {
	labeler, err := NewLabeler(
		fake.NewSimpleClientset(),
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"",
		false,
		false,
		testDeviceCountConfig(),
		false,
	)
	require.NoError(t, err)

	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Labels: map[string]string{
				"nvidia.com/gpu.count":     "4",
				"test.nvsentinel/current":  "4",
				"test.nvsentinel/expected": "8",
			},
		},
	}

	t.Run("ignores labels managed by device counts", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["test.nvsentinel/current"] = "3"
		newNode.Labels["test.nvsentinel/expected"] = "8"

		require.False(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})

	t.Run("reconciles when an input node label changes", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["nvidia.com/gpu.count"] = "8"

		require.True(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})
}

func TestLabelerNodeRequiresReconciliation_AllocatableChanges(t *testing.T) {
	t.Run("reconciles when allocatable changes with status-backed class", func(t *testing.T) {
		labeler, err := NewLabeler(
			fake.NewSimpleClientset(),
			time.Minute,
			"nvidia-dcgm",
			"nvidia-driver-daemonset",
			"nvidia-driver-installer",
			"",
			false,
			false,
			testAllocatableDeviceCountConfig(),
			false,
		)
		require.NoError(t, err)

		oldNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("0"),
				},
			},
		}

		newNode := oldNode.DeepCopy()
		newNode.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("4"),
		}

		require.True(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})

	t.Run("does not reconcile allocatable changes with label-only class", func(t *testing.T) {
		labeler, err := NewLabeler(
			fake.NewSimpleClientset(),
			time.Minute,
			"nvidia-dcgm",
			"nvidia-driver-daemonset",
			"nvidia-driver-installer",
			"",
			false,
			false,
			testDeviceCountConfig(),
			false,
		)
		require.NoError(t, err)

		oldNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"nvidia.com/gpu.count":     "4",
					"test.nvsentinel/current":  "4",
					"test.nvsentinel/expected": "8",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("0"),
				},
			},
		}

		newNode := oldNode.DeepCopy()
		newNode.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("4"),
		}

		require.False(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})
}

func TestLabelerResourceSlicesForNodeFiltersByNodeName(t *testing.T) {
	labeler, err := NewLabeler(
		fake.NewSimpleClientset(),
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"",
		false,
		false,
		testResourceSliceDeviceCountConfig(),
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, labeler.resourceSliceInformer)

	nodeName := "node-a"
	otherNodeName := "node-b"

	require.NoError(t, labeler.resourceSliceInformer.GetStore().Add(&resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		Spec: resourcev1.ResourceSliceSpec{
			NodeName: &nodeName,
		},
	}))
	require.NoError(t, labeler.resourceSliceInformer.GetStore().Add(&resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-b"},
		Spec: resourcev1.ResourceSliceSpec{
			NodeName: &otherNodeName,
		},
	}))
	require.NoError(t, labeler.resourceSliceInformer.GetStore().Add(&resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "global-slice"},
	}))

	resourceSlices := labeler.resourceSlicesForNode(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	})

	require.Len(t, resourceSlices, 1)
	require.Equal(t, "slice-a", resourceSlices[0].Name)
}

func testDeviceCountConfig() devicecounts.Config {
	return devicecounts.Config{
		Enabled: true,
		Classes: []devicecounts.ClassConfig{
			{
				Name:    "gpu",
				Enabled: true,
				Labels: devicecounts.Labels{
					Current:  "test.nvsentinel/current",
					Expected: "test.nvsentinel/expected",
				},
				CurrentExpression: "int(node.metadata.labels['nvidia.com/gpu.count'])",
			},
		},
	}
}

func testResourceSliceDeviceCountConfig() devicecounts.Config {
	config := testDeviceCountConfig()
	config.Classes[0].CurrentExpression = "resourceSlices.size()"

	return config
}

func deviceCountConfigWithExpression(expression string) devicecounts.Config {
	config := testDeviceCountConfig()
	config.Classes[0].CurrentExpression = expression

	return config
}

func testAllocatableDeviceCountConfig() devicecounts.Config {
	return devicecounts.Config{
		Enabled: true,
		Classes: []devicecounts.ClassConfig{
			{
				Name:    "nic",
				Enabled: true,
				Labels: devicecounts.Labels{
					Current:  "test.nvsentinel/nic-current",
					Expected: "test.nvsentinel/nic-expected",
				},
				CurrentExpression: "int(node.status.allocatable['nvidia.com/mlnxnics'])",
			},
		},
	}
}

// TestKataLabelOverrideIsolation verifies that creating multiple labeler instances
// with different overrides doesn't pollute each other (tests for race conditions).
func TestKataLabelOverrideIsolation(t *testing.T) {
	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")
	defer func() { _ = testEnv.Stop() }()

	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create kubernetes client")

	// Create first instance with override "first"
	l1, err := NewLabeler(
		clientset,
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"first.io/kata",
		false,
		false,
		devicecounts.Config{},
		false,
	)
	if err != nil {
		t.Fatalf("NewLabeler(first) error = %v", err)
	}

	// Create second instance with override "second"
	l2, err := NewLabeler(
		clientset,
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"second.io/kata",
		false,
		false,
		devicecounts.Config{},
		false,
	)
	if err != nil {
		t.Fatalf("NewLabeler(second) error = %v", err)
	}

	// Create third instance with no override
	l3, err := NewLabeler(
		clientset,
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"",
		false,
		false,
		devicecounts.Config{},
		false,
	)
	if err != nil {
		t.Fatalf("NewLabeler(empty) error = %v", err)
	}

	// All instances should be valid and independent
	if l1 == nil || l2 == nil || l3 == nil {
		t.Fatal("One or more labeler instances is nil")
	}

	t.Log("Successfully created 3 independent labeler instances with different overrides")
}

// TestKataLabelDetection tests that the labeler correctly detects and sets kata labels on nodes
func TestKataLabelDetection(t *testing.T) {
	tests := []struct {
		name            string
		node            *corev1.Node
		kataOverride    string
		expectedKataVal string
		shouldHaveLabel bool
	}{
		{
			name: "kata node with default label true",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kata-node",
					Labels: map[string]string{
						KataRuntimeDefaultLabel: "true",
					},
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "kata node with default label enabled",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kata-node",
					Labels: map[string]string{
						KataRuntimeDefaultLabel: "enabled",
					},
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "non-kata node with default label false",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "regular-node",
					Labels: map[string]string{
						KataRuntimeDefaultLabel: "false",
					},
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueFalse,
			shouldHaveLabel: true,
		},
		{
			name: "node with custom kata label",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-kata-node",
					Labels: map[string]string{
						"custom.io/kata": "true",
					},
				},
			},
			kataOverride:    "custom.io/kata",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "node without kata labels",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "no-kata-node",
					Labels: map[string]string{},
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueFalse,
			shouldHaveLabel: true,
		},
		{
			name: "node with both default and custom kata labels - both true",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "both-kata-node",
					Labels: map[string]string{
						KataRuntimeDefaultLabel: "true",
						"custom.io/kata":        "true",
					},
				},
			},
			kataOverride:    "custom.io/kata",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
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

			// Create the node
			_, err = cli.CoreV1().Nodes().Create(ctx, tt.node, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			// Create labeler with kata override if specified
			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", "nvidia-driver-installer", tt.kataOverride, false, false,
				devicecounts.Config{}, false)
			require.NoError(t, err, "failed to create labeler")

			// Start labeler
			labelerCtx, labelerCancel := context.WithCancel(ctx)
			defer labelerCancel()

			go func() {
				_ = labeler.Run(labelerCtx)
			}()

			// Wait for informer cache to sync
			require.Eventually(t, func() bool {
				return cache.WaitForCacheSync(labelerCtx.Done(), labeler.informersSynced...)
			}, 10*time.Second, 100*time.Millisecond, "informer cache did not sync")

			// Trigger kata detection by handling node event
			err = labeler.handleNodeEvent(tt.node)
			require.NoError(t, err, "failed to handle node event")

			// Verify the kata label was set correctly
			require.Eventually(t, func() bool {
				node, err := cli.CoreV1().Nodes().Get(ctx, tt.node.Name, metav1.GetOptions{})
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				kataLabel, exists := node.Labels[KataEnabledLabel]
				if !tt.shouldHaveLabel {
					return !exists
				}

				if !exists {
					t.Logf("Node %s missing kata.enabled label", tt.node.Name)
					return false
				}

				if kataLabel != tt.expectedKataVal {
					t.Logf("Node %s has wrong kata label: got %s, want %s", tt.node.Name, kataLabel, tt.expectedKataVal)
					return false
				}

				return true
			}, 15*time.Second, 500*time.Millisecond, "kata label not set correctly on node %s", tt.node.Name)
		})
	}
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
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
						DCGMVersionLabel:     "3.x",
					},
				},
			},
			existingPods:          []*corev1.Pod{},
			shouldHaveDriverLabel: false,
			shouldHaveDCGMLabel:   false,
		},
		{
			name: "driver label retained when driver pod exists",
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
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
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
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

			ns, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"}}, metav1.CreateOptions{})
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

			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", "nvidia-driver-installer", "", false, false,
				devicecounts.Config{}, false)
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

				// Restore original labels - reconcileAllNodes() may have removed them
				// before pods were indexed during the initial sync race.
				// Uses RetryOnConflict because the labeler may concurrently update
				// the same node during its initial reconciliation.
				err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
					node, err := cli.CoreV1().Nodes().Get(ctx, tt.existingNode.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					for k, v := range tt.existingNode.Labels {
						node.Labels[k] = v
					}
					_, err = cli.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
					return err
				})
				require.NoError(t, err, "failed to restore node labels")
			}

			err = labeler.handleNodeEvent(tt.existingNode)
			require.NoError(t, err, "failed to handle node event")

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
				ObjectMeta: metav1.ObjectMeta{
					Name: "gpu-node",
					Labels: map[string]string{
						"nvidia.com/gpu.present": "true",
					},
				},
			},
			expectedDriverLabel:   "true",
			shouldHaveDriverLabel: true,
		},
		{
			name:                  "assume-driver-installed skips non-GPU node",
			assumeDriverInstalled: true,
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "control-plane-node",
					Labels: map[string]string{},
				},
			},
			shouldHaveDriverLabel: false,
		},
		{
			name:                  "assume-driver-installed preserves label on GPU node reconciliation",
			assumeDriverInstalled: true,
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "gpu-node",
					Labels: map[string]string{
						"nvidia.com/gpu.present": "true",
						DriverInstalledLabel:     "true",
					},
				},
			},
			expectedDriverLabel:   "true",
			shouldHaveDriverLabel: true,
		},
		{
			name:                  "without flag stale label is removed when no driver pods",
			assumeDriverInstalled: false,
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "gpu-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
					},
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

			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", "nvidia-driver-installer", "", tt.assumeDriverInstalled, false,
				devicecounts.Config{}, false)
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

// TestMemoryUnderNodeUpdatePressure creates nodes in envtest and rapidly updates
// their status conditions (simulating kubelet heartbeats) to detect unbounded
// memory growth in the labeler's node event handler path.
func TestMemoryUnderNodeUpdatePressure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory stress test in short mode")
	}

	const (
		nodeCount    = 200
		testDuration = 60 * time.Second
		workers      = 10
		maxGrowthMiB = 30
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	defer func() { _ = testEnv.Stop() }()

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	createNodes(t, ctx, cli, nodeCount)

	labeler, err := NewLabeler(cli, 30*time.Second, "nvidia-dcgm", "nvidia-driver-daemonset", "nvidia-driver-installer", "", false, false,
		devicecounts.Config{}, false)
	require.NoError(t, err)

	labelerCtx, labelerCancel := context.WithCancel(ctx)
	defer labelerCancel()
	go func() { _ = labeler.Run(labelerCtx) }()

	require.True(t, cache.WaitForCacheSync(labelerCtx.Done(), labeler.informersSynced...))

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	updateCtx, updateCancel := context.WithTimeout(labelerCtx, testDuration)
	defer updateCancel()

	var peakHeapInuse atomic.Uint64
	peakHeapInuse.Store(baseline.HeapInuse)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-updateCtx.Done():
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				for {
					cur := peakHeapInuse.Load()
					if ms.HeapInuse <= cur || peakHeapInuse.CompareAndSwap(cur, ms.HeapInuse) {
						break
					}
				}
			}
		}
	}()

	generateNodeUpdates(t, updateCtx, cli, nodeCount, workers)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var final runtime.MemStats
	runtime.ReadMemStats(&final)

	finalGrowthMiB := int64(final.HeapInuse-baseline.HeapInuse) / 1024 / 1024
	peakGrowthMiB := int64(peakHeapInuse.Load()-baseline.HeapInuse) / 1024 / 1024

	t.Logf("nodes=%d duration=%s baseline=%dMiB peak=%dMiB final=%dMiB peakGrowth=%dMiB finalGrowth=%dMiB",
		nodeCount, testDuration,
		baseline.HeapInuse/1024/1024, peakHeapInuse.Load()/1024/1024, final.HeapInuse/1024/1024,
		peakGrowthMiB, finalGrowthMiB)

	if peakGrowthMiB > maxGrowthMiB {
		t.Errorf("peak memory grew by %d MiB (limit %d MiB) — likely unbounded notification buffer growth",
			peakGrowthMiB, maxGrowthMiB)
	}
	if finalGrowthMiB > maxGrowthMiB {
		t.Errorf("final memory grew by %d MiB (limit %d MiB) — memory not reclaimed after GC",
			finalGrowthMiB, maxGrowthMiB)
	}
}

func createNodes(t *testing.T, ctx context.Context, cli kubernetes.Interface, count int) {
	t.Helper()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 20)

	for i := range count {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("node-%04d", i),
					Labels: map[string]string{"nvidia.com/gpu.present": "true"},
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
		}()
	}

	wg.Wait()
	t.Logf("created %d nodes", count)
}

func generateNodeUpdates(t *testing.T, ctx context.Context, cli kubernetes.Interface, nodeCount, workers int) {
	t.Helper()

	sem := make(chan struct{}, workers)

	for round := 1; ; round++ {
		var wg sync.WaitGroup
		cancelled := false

		for i := range nodeCount {
			select {
			case <-ctx.Done():
				cancelled = true
			case sem <- struct{}{}:
			}
			if cancelled {
				break
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				node, err := cli.CoreV1().Nodes().Get(ctx, fmt.Sprintf("node-%04d", i), metav1.GetOptions{})
				if err != nil {
					return
				}
				node.Status.Conditions = []corev1.NodeCondition{{
					Type:              corev1.NodeReady,
					Status:            corev1.ConditionTrue,
					LastHeartbeatTime: metav1.Now(),
					Reason:            fmt.Sprintf("round-%d", round),
				}}
				_, _ = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			}()
		}

		wg.Wait()

		if cancelled {
			return
		}
		t.Logf("heartbeat round %d complete", round)
	}
}

// errNodeLister is a NodeLister that always returns a non-NotFound error from Get,
// used to exercise the fail-closed paths in reconcileNodeLabelsInPlace and
// updateNodeLabelsForPod.
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

// TestReconcileNodeLabelsInPlace_KataAndDriverLabelsMissing_AppliesBothInOnePass
// covers a node that needs its Kata label and its driver label at the same time.
//
// The two results were combined with `needsUpdate ||
// l.updateDriverAndDCGMLabels(...)`, and || short-circuits: once the Kata label had
// been set, the driver and DCGM labels were never evaluated. A second reconcile pass
// hid this, because by then the Kata label was already correct and the short circuit
// no longer triggered — so the labels arrived one pass later than they should, and
// only if a second pass happened at all.
func TestReconcileNodeLabelsInPlace_KataAndDriverLabelsMissing_AppliesBothInOnePass(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "kata-and-driver-node",
			Labels: map[string]string{gpuPresentLabel: LabelValueTrue},
		},
	}

	// Never started, so no event handler can race the reconcile below. It only has to
	// hold the node: the device-count pass is disabled here but still lists the store.
	nodeInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	require.NoError(t, nodeInformer.GetStore().Add(node))

	l := &Labeler{
		ctx:          context.Background(),
		nodeInformer: nodeInformer,
		nodeLister:   listersv1.NewNodeLister(nodeInformer.GetIndexer()),
	}

	target := node.DeepCopy()
	changed := l.reconcileNodeLabelsInPlace(target, LabelValueTrue, "")

	assert.True(t, changed, "both labels are missing, so an update is needed")
	assert.Equal(t, LabelValueFalse, target.Labels[KataEnabledLabel])
	assert.Equal(t, LabelValueTrue, target.Labels[DriverInstalledLabel],
		"the driver label must not be skipped just because the kata label changed")
}

func TestReconcileNodeLabelsInPlace_ManagedGate(t *testing.T) {
	t.Run("strips detection labels when node is opted out", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "opted-out-node",
				Labels: map[string]string{
					managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
					DCGMVersionLabel:        "3.x",
					DriverInstalledLabel:    "true",
					KataEnabledLabel:        "true",
				},
			},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}

		target := node.DeepCopy()
		changed := l.reconcileNodeLabelsInPlace(target, "true", "3.x")

		assert.True(t, changed, "should report labels changed")
		assert.NotContains(t, target.Labels, DCGMVersionLabel, "DCGM label must be stripped")
		assert.NotContains(t, target.Labels, DriverInstalledLabel, "driver label must be stripped")
		assert.NotContains(t, target.Labels, KataEnabledLabel, "kata label must be stripped")
		// The managed label itself is owned by the ERR reconciler — labeler must not touch it.
		assert.Equal(t, managed.ManagedLabelValueFalse, target.Labels[managed.ManagedLabelKey])
	})

	t.Run("no-op when opted-out node has no detection labels", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "opted-out-clean",
				Labels: map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse},
			},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}
		target := node.DeepCopy()
		changed := l.reconcileNodeLabelsInPlace(target, "", "")
		assert.False(t, changed, "no labels to strip → no update needed")
	})

	t.Run("fail closed — returns false without mutating on lister error", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "error-node",
				Labels: map[string]string{DCGMVersionLabel: "3.x", DriverInstalledLabel: "true"},
			},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: errNodeLister{},
		}
		target := node.DeepCopy()
		changed := l.reconcileNodeLabelsInPlace(target, "true", "3.x")

		assert.False(t, changed, "lister error must not report changes")
		assert.Contains(t, target.Labels, DCGMVersionLabel, "lister error must not strip labels")
		assert.Contains(t, target.Labels, DriverInstalledLabel, "lister error must not strip labels")
	})

}

func TestUpdateNodeLabelsForPod_ManagedGate(t *testing.T) {
	t.Run("skips update when node is opted out", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "opted-out-node",
				Labels: map[string]string{
					managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
					// Suppose driver label is currently absent — update would stamp it.
				},
			},
		}
		clientset := fake.NewSimpleClientset(node)
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
			clientset:  clientset,
		}

		err := l.updateNodeLabelsForPod(node.Name, "true", "3.x")
		require.NoError(t, err, "opted-out skip must not return an error")

		// Verify the clientset was never called to update labels.
		updated, getErr := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.NotContains(t, updated.Labels, DriverInstalledLabel,
			"driver label must not be stamped on an opted-out node")
		assert.NotContains(t, updated.Labels, DCGMVersionLabel,
			"DCGM label must not be stamped on an opted-out node")
	})

	t.Run("fail closed — skips update on lister error", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "error-node", Labels: map[string]string{}},
		}
		clientset := fake.NewSimpleClientset(node)
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: errNodeLister{},
			clientset:  clientset,
		}

		err := l.updateNodeLabelsForPod(node.Name, "true", "3.x")
		require.NoError(t, err, "fail-closed skip must not propagate as an error")

		updated, getErr := clientset.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.NotContains(t, updated.Labels, DriverInstalledLabel,
			"lister error must not stamp labels on node")
	})
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

// TestUpdateNodeLabels_DriverPodDeletedDuringReconcile_DeleteWins verifies that a
// startup reconcile cannot overwrite a concurrent pod-deletion result.
func TestUpdateNodeLabels_DriverPodDeletedDuringReconcile_DeleteWins(t *testing.T) {
	const nodeName = "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				KataEnabledLabel: LabelValueFalse,
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "driver-pod",
			Namespace: "default",
			UID:       "driver-pod-uid",
			Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	l, clientset := newLabelerWithCachedDriverPods(t, node, pod)
	fakeClient := clientset.(*fake.Clientset)

	reconcileReachedPatch := make(chan struct{})
	releaseReconcile := make(chan struct{})
	var blocked atomic.Bool
	fakeClient.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if blocked.CompareAndSwap(false, true) {
			close(reconcileReachedPatch)
			<-releaseReconcile
		}

		return false, nil, nil
	})

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- l.updateNodeLabels(nodeName) }()
	<-reconcileReachedPatch

	// Informer stores remove an object before invoking its delete handler.
	require.NoError(t, l.podInformer.GetIndexer().Delete(pod))
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- l.handlePodDeleteEvent(pod) }()

	select {
	case err := <-deleteDone:
		t.Fatalf("pod deletion was not serialized with node reconciliation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseReconcile)
	require.NoError(t, <-reconcileDone)
	require.NoError(t, <-deleteDone)

	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Labels, DriverInstalledLabel)
}

// TestUpdateNodeLabels_CachedNode_UsesPatchOnly pins the API-call budget and payload
// of the label write path.
func TestUpdateNodeLabels_CachedNode_UsesPatchOnly(t *testing.T) {
	const nodeName = "gpu-node"

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            nodeName,
			ResourceVersion: "1",
			Labels:          map[string]string{gpuPresentLabel: LabelValueTrue},
		},
	})

	labeler, _ := startTransformTestLabeler(t, clientset, devicecounts.Config{})
	requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueFalse)

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
				delete(node.Labels, KataEnabledLabel)
				_, updateErr := clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, updateErr)
				requireCachedLabelAbsent(t, labeler, nodeName, KataEnabledLabel)
			},
			expectedPatch: fmt.Sprintf(
				`{"metadata":{"labels":{%q:%q}}}`,
				KataEnabledLabel,
				LabelValueFalse,
			),
		},
		{
			name: "no-op reconcile costs no API call",
			setup: func(t *testing.T) {
				requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueFalse)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			clientset.ClearActions()

			require.NoError(t, labeler.updateNodeLabels(nodeName))

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
			assert.Equal(t, LabelValueFalse, updated.Labels[KataEnabledLabel], "the desired label is present")
			assert.Equal(t, LabelValueTrue, updated.Labels[gpuPresentLabel],
				"a merge patch must leave labels it does not mention alone")
		})
	}
}

func TestUpdateNodeLabels_DelayedInformerEvent_EventuallyConverges(t *testing.T) {
	const nodeName = "delayed-node"

	initial := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				KataRuntimeDefaultLabel: LabelValueFalse,
				KataEnabledLabel:        LabelValueFalse,
			},
		},
	}

	// A controlled fake watch lets the API object advance independently of the
	// informer cache, making the stale-cache checkpoints deterministic.
	clientset := fake.NewSimpleClientset(initial)
	nodeWatch := watch.NewRaceFreeFake()
	clientset.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nodeWatch, nil
	})

	labeler, _ := startTransformTestLabeler(t, clientset, devicecounts.Config{})
	requireCachedLabel(t, labeler, nodeName, KataRuntimeDefaultLabel, LabelValueFalse)

	// Change the API object while withholding its watch event. The informer is synced,
	// but its node projection deliberately remains one generation behind.
	liveNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	liveNode.Labels[KataRuntimeDefaultLabel] = LabelValueTrue
	liveNode, err = clientset.CoreV1().Nodes().Update(t.Context(), liveNode, metav1.UpdateOptions{})
	require.NoError(t, err)

	cachedNode, err := labeler.getNodeFromCache(nodeName)
	require.NoError(t, err)
	assert.Equal(t, LabelValueFalse, cachedNode.Labels[KataRuntimeDefaultLabel])
	assert.Equal(t, LabelValueFalse, liveNode.Labels[KataEnabledLabel])

	// A reconcile against the stale projection computes the stale derived value and
	// correctly emits no write.
	clientset.ClearActions()
	require.NoError(t, labeler.updateNodeLabels(nodeName))
	assert.Empty(t, clientset.Actions())

	// Deliver the delayed input event. Its handler must reconcile from the new cache
	// generation and correct the derived label.
	nodeWatch.Modify(liveNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, KataRuntimeDefaultLabel, LabelValueTrue)

	cachedNode, err = labeler.getNodeFromCache(nodeName)
	require.NoError(t, err)
	assert.Equal(t, LabelValueFalse, cachedNode.Labels[KataEnabledLabel])

	require.Eventually(t, func() bool {
		node, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
		return getErr == nil && node.Labels[KataEnabledLabel] == LabelValueTrue
	}, 10*time.Second, 10*time.Millisecond)

	finalNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	nodeWatch.Modify(finalNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueTrue)
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
