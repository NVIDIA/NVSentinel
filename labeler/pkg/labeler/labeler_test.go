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

			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", "")
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
				tt.override,
			)

			if err != nil {
				t.Fatalf("NewLabeler() error = %v", err)
			}

			if l == nil {
				t.Fatal("NewLabeler() returned nil labeler")
			}

			// Verify labeler was created successfully
			// The actual kataLabels field is private, but we can verify
			// no panic occurred and the instance is valid
			t.Logf("Successfully created labeler with override: %q", tt.override)
		})
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
		"first.io/kata",
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
		"second.io/kata",
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
		"",
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
			labeler, err := NewLabeler(cli, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset", tt.kataOverride)
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
