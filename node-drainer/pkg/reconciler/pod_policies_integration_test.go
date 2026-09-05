// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package reconciler_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/reconciler"
)

func podPolicyTestConfig() config.TomlConfig {
	return config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: time.Second},
		DeleteAfterTimeoutMinutes: 5,
		NotReadyTimeoutMinutes:    5,
		SystemNamespaces:          "^system-",
		PartialDrainEnabled:       true,
		UserNamespaces:            []config.UserNamespace{{Name: "workloads", Mode: config.ModeImmediateEvict}},
		PodDrainPolicies: []config.PodDrainPolicy{
			{Name: "protected", Namespace: "workloads", PodSelector: "protected=yes", Mode: config.ModeAllowCompletion},
			{Name: "replaceable", Namespace: "workloads", PodSelector: "role in (worker,overlap)", Mode: config.ModeImmediateEvict},
			{Name: "bounded", Namespace: "workloads", PodSelector: "role=bounded", Mode: config.ModeDeleteAfterTimeout},
		},
	}
}

func createPolicyPod(t *testing.T, setup *testSetup, namespace, name, node string,
	labels map[string]string, gpu string) *v1.Pod {
	t.Helper()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       v1.PodSpec{NodeName: node, Containers: []v1.Container{{Name: "worker", Image: "busybox"}}},
	}
	if gpu != "" {
		pod.Annotations = map[string]string{model.PodDeviceAnnotationName: `{"devices":{"nvidia.com/gpu":["` + gpu + `"]}}`}
		pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	}
	created, err := setup.client.CoreV1().Pods(namespace).Create(setup.ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	created.Status.Phase = v1.PodRunning
	created, err = setup.client.CoreV1().Pods(namespace).UpdateStatus(setup.ctx, created, metav1.UpdateOptions{})
	require.NoError(t, err)
	return created
}

func requirePolicyPodRunning(t *testing.T, setup *testSetup, namespace, name string) {
	t.Helper()
	pod, err := setup.client.CoreV1().Pods(namespace).Get(setup.ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Nil(t, pod.DeletionTimestamp, "pod %s/%s must not be evicted by another policy", namespace, name)
	require.Equal(t, v1.PodRunning, pod.Status.Phase)
}

func finishPolicyPodDeletion(t *testing.T, setup *testSetup, name string) {
	t.Helper()
	pod, err := setup.client.CoreV1().Pods("workloads").Get(setup.ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return
	}
	require.NoError(t, err)
	require.NotNil(t, pod.DeletionTimestamp, "the drainer must have requested eviction before simulating kubelet completion")
	require.NoError(t, setup.client.CoreV1().Pods("workloads").Delete(setup.ctx, name,
		metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}))
}

func TestReconcilerPodPoliciesMixedModesAndRestart(t *testing.T) {
	setup := setupConfiguredTest(t, podPolicyTestConfig(), false)
	const node = "policy-node"
	createNode(setup.ctx, t, setup.client, node)
	waitForNodeInInformer(t, setup.informersInstance, node)
	for _, ns := range []string{"workloads", "unmanaged", "system-workloads"} {
		createNamespace(setup.ctx, t, setup.client, ns)
	}
	createPolicyPod(t, setup, "workloads", "immediate", node, map[string]string{"role": "worker"}, "")
	createPolicyPod(t, setup, "workloads", "fallback", node, nil, "")
	createPolicyPod(t, setup, "workloads", "protected", node, map[string]string{"role": "overlap", "protected": "yes"}, "")
	createPolicyPod(t, setup, "workloads", "bounded", node, map[string]string{"role": "bounded"}, "")
	createPolicyPod(t, setup, "unmanaged", "outside-policy-namespace", node, map[string]string{"role": "worker"}, "")
	createPolicyPod(t, setup, "system-workloads", "excluded", node, map[string]string{"role": "worker"}, "")
	createPolicyPod(t, setup, "workloads", "other-node", "other-node", map[string]string{"role": "worker"}, "")
	require.Eventually(t, func() bool {
		pods, err := setup.informersInstance.FindEvictablePodsInNamespaceAndNode("workloads", node, nil)
		return err == nil && len(pods) == 4
	}, 10*time.Second, 50*time.Millisecond)

	opts := healthEventOptions{nodeName: node, nodeQuarantined: model.Quarantined, createdAt: time.Now()}
	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
	require.ErrorContains(t, err, "immediate eviction")
	finishPolicyPodDeletion(t, setup, "immediate")
	finishPolicyPodDeletion(t, setup, "fallback")
	requirePolicyPodRunning(t, setup, "workloads", "protected")
	requirePolicyPodRunning(t, setup, "workloads", "bounded")
	requirePolicyPodRunning(t, setup, "workloads", "other-node")
	requirePolicyPodRunning(t, setup, "unmanaged", "outside-policy-namespace")
	requirePolicyPodRunning(t, setup, "system-workloads", "excluded")

	require.Eventually(t, func() bool {
		err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
		return err != nil && containsPolicyWait(err.Error())
	}, 10*time.Second, 50*time.Millisecond)
	requirePolicyPodRunning(t, setup, "workloads", "protected")
	requirePolicyPodRunning(t, setup, "workloads", "bounded")

	// Recreate the reconciler and replay the same event after its deadline. The
	// timeout must be based on event creation, and only the bounded pod may go.
	restarted, err := reconciler.NewReconciler(setup.reconciler.Config, false, setup.client,
		setup.informersInstance, &mockDataStore{}, setup.healthEventStore, nil, nil)
	require.NoError(t, err)
	opts.createdAt = time.Now().Add(-6 * time.Minute)
	_ = processHealthEvent(setup.ctx, t, restarted, setup.mockCollection, setup.healthEventStore, opts)
	require.Eventually(t, func() bool {
		_, err := setup.client.CoreV1().Pods("workloads").Get(setup.ctx, "bounded", metav1.GetOptions{})
		return errors.IsNotFound(err)
	}, 10*time.Second, 50*time.Millisecond)
	requirePolicyPodRunning(t, setup, "workloads", "protected")
	require.Eventually(t, func() bool {
		err := processHealthEvent(setup.ctx, t, restarted, setup.mockCollection, setup.healthEventStore, opts)
		return err != nil && strings.Contains(err.Error(), "waiting for pods to complete: 1")
	}, 10*time.Second, 50*time.Millisecond)
	protected, err := setup.client.CoreV1().Pods("workloads").Get(setup.ctx, "protected", metav1.GetOptions{})
	require.NoError(t, err)
	protected.Status.Phase = v1.PodSucceeded
	_, err = setup.client.CoreV1().Pods("workloads").UpdateStatus(setup.ctx, protected, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return processHealthEvent(setup.ctx, t, restarted, setup.mockCollection, setup.healthEventStore, opts) == nil
	}, 10*time.Second, 50*time.Millisecond)
	assertNodeLabel(t, setup.client, setup.ctx, node, statemanager.DrainSucceededLabelValue)
	requirePolicyPodRunning(t, setup, "unmanaged", "outside-policy-namespace")
}

func containsPolicyWait(message string) bool {
	return strings.Contains(message, "waiting for 1 pods to complete or timeout")
}

func TestReconcilerPodPoliciesPartialDrainAndForce(t *testing.T) {
	setup := setupConfiguredTest(t, podPolicyTestConfig(), false)
	const node = "partial-policy-node"
	createNode(setup.ctx, t, setup.client, node)
	waitForNodeInInformer(t, setup.informersInstance, node)
	createNamespace(setup.ctx, t, setup.client, "workloads")
	createPolicyPod(t, setup, "workloads", "affected", node, map[string]string{"protected": "yes"}, "GPU-0")
	createPolicyPod(t, setup, "workloads", "healthy-gpu", node, map[string]string{"role": "worker"}, "GPU-1")
	createPolicyPod(t, setup, "workloads", "cpu-only", node, map[string]string{"role": "worker"}, "")
	target := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-0"}
	require.Eventually(t, func() bool {
		pods, err := setup.informersInstance.FindEvictablePodsInNamespaceAndNode("workloads", node, target)
		return err == nil && len(pods) == 1
	}, 10*time.Second, 50*time.Millisecond)
	opts := healthEventOptions{nodeName: node, nodeQuarantined: model.Quarantined,
		recommendedAction: protos.RecommendedAction_COMPONENT_RESET, entitiesImpacted: []*protos.Entity{target}}
	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
	require.ErrorContains(t, err, "waiting for pods to complete: 1")
	requirePolicyPodRunning(t, setup, "workloads", "affected")
	opts.drainForce = true
	err = processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
	require.ErrorContains(t, err, "immediate eviction")
	finishPolicyPodDeletion(t, setup, "affected")
	requirePolicyPodRunning(t, setup, "workloads", "healthy-gpu")
	requirePolicyPodRunning(t, setup, "workloads", "cpu-only")
}

func TestReconcilerPodPoliciesDryRun(t *testing.T) {
	setup := setupConfiguredTest(t, podPolicyTestConfig(), true)
	const node = "dry-run-policy-node"
	createNode(setup.ctx, t, setup.client, node)
	waitForNodeInInformer(t, setup.informersInstance, node)
	createNamespace(setup.ctx, t, setup.client, "workloads")
	createPolicyPod(t, setup, "workloads", "worker", node, map[string]string{"role": "worker"}, "")
	createPolicyPod(t, setup, "workloads", "protected", node, map[string]string{"protected": "yes"}, "")
	require.Eventually(t, func() bool {
		pods, err := setup.informersInstance.FindEvictablePodsInNamespaceAndNode("workloads", node, nil)
		return err == nil && len(pods) == 2
	}, 10*time.Second, 50*time.Millisecond)
	opts := healthEventOptions{nodeName: node, nodeQuarantined: model.Quarantined}
	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
	require.ErrorContains(t, err, "immediate eviction")
	requirePolicyPodRunning(t, setup, "workloads", "worker")
	requirePolicyPodRunning(t, setup, "workloads", "protected")
}

func TestReconcilerPodPoliciesObserveLabelChangesWithoutNamespaceFallback(t *testing.T) {
	cfg := podPolicyTestConfig()
	cfg.UserNamespaces = nil
	setup := setupConfiguredTest(t, cfg, false)
	const node = "relabel-policy-node"
	createNode(setup.ctx, t, setup.client, node)
	waitForNodeInInformer(t, setup.informersInstance, node)
	createNamespace(setup.ctx, t, setup.client, "workloads")
	createPolicyPod(t, setup, "workloads", "selected", node, map[string]string{"protected": "yes"}, "")
	createPolicyPod(t, setup, "workloads", "unmatched", node, nil, "")
	require.Eventually(t, func() bool {
		pods, err := setup.informersInstance.FindEvictablePodsInNamespaceAndNode("workloads", node, nil)
		return err == nil && len(pods) == 2
	}, 10*time.Second, 50*time.Millisecond)
	opts := healthEventOptions{nodeName: node, nodeQuarantined: model.Quarantined}
	err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
	require.ErrorContains(t, err, "waiting for pods to complete: 1")
	pod, err := setup.client.CoreV1().Pods("workloads").Get(setup.ctx, "selected", metav1.GetOptions{})
	require.NoError(t, err)
	pod.Labels = map[string]string{"role": "worker"}
	_, err = setup.client.CoreV1().Pods("workloads").Update(setup.ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		err := processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts)
		return err != nil && strings.Contains(err.Error(), "immediate eviction")
	}, 10*time.Second, 50*time.Millisecond)
	finishPolicyPodDeletion(t, setup, "selected")
	opts.drainForce = true
	require.Eventually(t, func() bool {
		return processHealthEvent(setup.ctx, t, setup.reconciler, setup.mockCollection, setup.healthEventStore, opts) == nil
	}, 10*time.Second, 50*time.Millisecond)
	assertNodeLabel(t, setup.client, setup.ctx, node, statemanager.DrainSucceededLabelValue)
	requirePolicyPodRunning(t, setup, "workloads", "unmatched")
}
