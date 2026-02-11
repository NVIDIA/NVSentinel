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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/coordinator"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
// source <(setup-envtest use -p env)
func TestGangController_Reconcile(t *testing.T) {
	tests := []struct {
		name            string
		pod             *corev1.Pod
		discoverer      *mockDiscoverer
		expectConfigMap bool
	}{
		{
			name:            "pod with IP belonging to gang registers peer",
			pod:             newGangPod("gang-pod-0", "default", "10.0.0.1"),
			discoverer:      newGangDiscoverer("test-gang", 2),
			expectConfigMap: true,
		},
		{
			name:            "pod not belonging to gang is skipped",
			pod:             newTestPod("regular-pod", "default", "10.0.0.2"),
			discoverer:      newNonGangDiscoverer(),
			expectConfigMap: false,
		},
		{
			name:            "pod with empty gang ID is skipped",
			pod:             newTestPod("no-gang-id-pod", "default", "10.0.0.3"),
			discoverer:      &mockDiscoverer{canHandle: true, gangID: ""},
			expectConfigMap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			te := setupTestEnv(t, ctx, tt.discoverer)
			defer te.teardown()

			te.createNamespace(t, ctx, tt.pod.Namespace)
			te.createPodWithIP(t, ctx, tt.pod)

			if tt.expectConfigMap {
				te.assertConfigMapWithPeer(t, ctx, tt.pod.Namespace, tt.discoverer.gangID, tt.pod.Name, tt.pod.Status.PodIP)
			} else {
				te.assertNoConfigMaps(t, ctx, tt.pod.Namespace)
			}
		})
	}
}

func TestGangController_TerminatingPodSkipped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	te := setupTestEnv(t, ctx, newGangDiscoverer("test-gang", 2))
	defer te.teardown()

	te.createNamespace(t, ctx, "default")

	// Create pod with finalizer but NO IP yet
	pod := newTestPod("terminating-pod", "default", "")
	pod.Finalizers = []string{"nvsentinel.nvidia.com/test-finalizer"}

	createdPod, err := te.kubeClient.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create pod")

	// Delete the pod - this sets DeletionTimestamp, but finalizer keeps it around
	err = te.kubeClient.CoreV1().Pods("default").Delete(ctx, createdPod.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "failed to delete pod")

	// Now add IP to the terminating pod - this should trigger reconcile
	// but reconcile should skip it because DeletionTimestamp is set
	createdPod, err = te.kubeClient.CoreV1().Pods("default").Get(ctx, createdPod.Name, metav1.GetOptions{})
	require.NoError(t, err, "failed to get pod")
	require.NotNil(t, createdPod.DeletionTimestamp, "pod should be terminating")

	createdPod.Status.PodIP = "10.0.0.5"
	_, err = te.kubeClient.CoreV1().Pods("default").UpdateStatus(ctx, createdPod, metav1.UpdateOptions{})
	require.NoError(t, err, "failed to update pod status")

	te.assertNoConfigMaps(t, ctx, "default")
}

type mockDiscoverer struct {
	canHandle bool
	gangID    string
	gangInfo  *types.GangInfo
}

func (m *mockDiscoverer) Name() string                       { return "mock" }
func (m *mockDiscoverer) CanHandle(_ *corev1.Pod) bool       { return m.canHandle }
func (m *mockDiscoverer) ExtractGangID(_ *corev1.Pod) string { return m.gangID }
func (m *mockDiscoverer) DiscoverPeers(_ context.Context, _ *corev1.Pod) (*types.GangInfo, error) {
	return m.gangInfo, nil
}

type testEnv struct {
	env        *envtest.Environment
	cfg        *rest.Config
	kubeClient kubernetes.Interface
	mgr        manager.Manager
	cancel     context.CancelFunc
}

func setupTestEnv(t *testing.T, ctx context.Context, discoverer *mockDiscoverer) *testEnv {
	t.Helper()

	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoError(t, err, "failed to start envtest")

	kubeClient, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create kubernetes client")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"}, // Disable metrics to avoid port conflicts
	})
	require.NoError(t, err, "failed to create manager")

	coord := gang.NewCoordinator(mgr.GetClient(), gang.DefaultCoordinatorConfig())
	gangCtrl := NewGangController(mgr.GetClient(), coord, discoverer)

	skipValidation := true
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(gangCtrl.podIPChangedPredicate()).
		WithOptions(controller.Options{SkipNameValidation: &skipValidation}).
		Complete(gangCtrl)
	require.NoError(t, err, "failed to setup controller")

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	go func() { _ = mgr.Start(mgrCtx) }()

	// Wait for cache sync
	require.Eventually(t, func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second, 100*time.Millisecond, "cache did not sync")

	return &testEnv{
		env:        env,
		cfg:        cfg,
		kubeClient: kubeClient,
		mgr:        mgr,
		cancel:     mgrCancel,
	}
}

func (te *testEnv) teardown() {
	te.cancel()
	_ = te.env.Stop()
}

func (te *testEnv) createNamespace(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	_, err := te.kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("Namespace %q: %v (may already exist)", name, err)
	}
}

func (te *testEnv) createPodWithIP(t *testing.T, ctx context.Context, pod *corev1.Pod) {
	t.Helper()
	createdPod, err := te.kubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create pod")

	createdPod.Status = pod.Status
	_, err = te.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, createdPod, metav1.UpdateOptions{})
	require.NoError(t, err, "failed to update pod status")
}

func (te *testEnv) assertConfigMapWithPeer(t *testing.T, ctx context.Context, namespace, gangID, podName, podIP string) {
	t.Helper()
	configMapName := coordinator.ConfigMapName(gangID)

	require.Eventually(t, func() bool {
		cm, err := te.kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
		if err != nil {
			t.Logf("ConfigMap %q not found: %v", configMapName, err)
			return false
		}

		for _, p := range coordinator.ParsePeers(cm.Data["peers"]) {
			if p.PodName == podName && p.PodIP == podIP {
				return true
			}
		}
		t.Logf("Peer %s/%s not found in ConfigMap", podName, podIP)
		return false
	}, 10*time.Second, 200*time.Millisecond, "peer not registered in ConfigMap")
}

func (te *testEnv) assertNoConfigMaps(t *testing.T, ctx context.Context, namespace string) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	cms, err := te.kubeClient.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, cms.Items, "expected no ConfigMaps")
}

func newTestPod(name, namespace, ip string) *corev1.Pod {
	return newTestPodWithGangVolume(name, namespace, ip, false)
}

func newGangPod(name, namespace, ip string) *corev1.Pod {
	return newTestPodWithGangVolume(name, namespace, ip, true)
}

func newTestPodWithGangVolume(name, namespace, ip string, withGangVolume bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "test", Image: "busybox"},
			},
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}

	if withGangVolume {
		pod.Spec.Volumes = []corev1.Volume{
			{
				Name: types.GangConfigVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "preflight-test-gang",
						},
					},
				},
			},
		}
	}

	return pod
}

func newGangDiscoverer(gangID string, minCount int) *mockDiscoverer {
	return &mockDiscoverer{
		canHandle: true,
		gangID:    gangID,
		gangInfo: &types.GangInfo{
			GangID:           gangID,
			ExpectedMinCount: minCount,
		},
	}
}

func newNonGangDiscoverer() *mockDiscoverer {
	return &mockDiscoverer{canHandle: false}
}
