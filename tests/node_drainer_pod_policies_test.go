//go:build amd64_group
// +build amd64_group

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

package tests

import (
	"context"
	"testing"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

// TestNodeDrainerPodPolicies follows a health event through quarantine, eviction,
// restart recovery and completion, with three drain modes in one namespace.
func TestNodeDrainerPodPolicies(t *testing.T) {
	feature := features.New("TestNodeDrainerPodPolicies").WithLabel("suite", "node-drainer")
	const namespace = "pod-policies-test"
	var testCtx *helpers.NodeDrainerTestContext
	pods := make(map[string][]string)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		ctx, testCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-pod-policies.yaml", namespace)
		for _, workload := range []struct {
			name   string
			labels map[string]string
		}{
			{"immediate", map[string]string{"role": "worker"}},
			{"protected", map[string]string{"role": "overlap", "protected": "yes"}},
			{"bounded", map[string]string{"role": "bounded"}},
			{"fallback", nil},
		} {
			names := []string{workload.name}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: workload.name, Namespace: namespace, Labels: workload.labels,
				},
				Spec: v1.PodSpec{NodeName: testCtx.NodeName,
					Containers: []v1.Container{{Name: "workload", Image: "busybox:latest", Command: []string{"sleep", "3600"}}}},
			}
			require.NoError(t, client.Resources().Create(ctx, pod))
			pods[workload.name] = names
			helpers.WaitForPodsRunning(ctx, t, client, namespace, names)
		}
		// Populate the fresh informer with the final labels before sending the event.
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace))
		return ctx
	})

	feature.Assess("policies isolate workloads and survive a drainer restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		assertRunning := func(workload string) {
			for _, name := range pods[workload] {
				pod := &v1.Pod{}
				require.NoError(t, client.Resources().Get(ctx, name, namespace, pod))
				require.Nil(t, pod.DeletionTimestamp, "%s must not be evicted by another policy", workload)
				require.Equal(t, v1.PodRunning, pod.Status.Phase)
			}
		}
		helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").WithMessage("pod policy drain integration test"))
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName, statemanager.NVSentinelStateLabelKey, helpers.DrainingLabelValue)
		helpers.WaitForPodsDeleted(ctx, t, client, namespace, pods["immediate"])
		helpers.WaitForPodsDeleted(ctx, t, client, namespace, pods["fallback"])
		assertRunning("protected")
		assertRunning("bounded")

		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace))
		helpers.WaitForPodsDeleted(ctx, t, client, namespace, pods["bounded"])
		assertRunning("protected")
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.Equal(t, helpers.DrainingLabelValue, node.Labels[statemanager.NVSentinelStateLabelKey],
			"the drain must wait for the protected workload")

		// KWOK simulates the workload finishing; the drainer must observe completion
		// without sending an eviction or deletion for this policy.
		for _, name := range pods["protected"] {
			require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				pod := &v1.Pod{}
				if err := client.Resources().Get(ctx, name, namespace, pod); err != nil {
					return err
				}
				pod.Status.Phase = v1.PodSucceeded
				return client.Resources().UpdateStatus(ctx, pod)
			}))
		}
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)
		for _, name := range pods["protected"] {
			pod := &v1.Pod{}
			require.NoError(t, client.Resources().Get(ctx, name, namespace, pod))
			require.Nil(t, pod.DeletionTimestamp)
			require.Equal(t, v1.PodSucceeded, pod.Status.Phase)
		}
		return ctx
	})
	feature.Teardown(helpers.TeardownNodeDrainer)
	testEnv.Test(t, feature.Feature())
}
