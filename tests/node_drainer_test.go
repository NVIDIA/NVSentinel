//go:build amd64_group
// +build amd64_group

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

package tests

import (
	"context"
	"testing"

	"tests/helpers"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestDrainControllerBasicFlow tests the DrainController's basic drain flow.
func TestDrainControllerBasicFlow(t *testing.T) {
	feature := features.New("TestDrainControllerBasicFlow").
		WithLabel("suite", "drain-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		workloadNamespace := "drain-test"
		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		require.NoError(t, err)

		// Create test pods on the node
		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		return ctx
	})

	feature.Assess("DrainController transitions Quarantined event to Draining", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create a fatal event
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithMessage("XID error occurred").
			WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent: %s", created.Name)

		ctx = context.WithValue(ctx, keyHealthEventName, created.Name)

		// Wait for QuarantineController to process first
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Wait for DrainController to start draining
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDraining)
		t.Log("DrainController started draining (phase=Draining)")

		return ctx
	})

	feature.Assess("DrainController transitions to Drained after pods evicted", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		eventName := ctx.Value(keyHealthEventName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Manually delete pods to simulate eviction completion
		t.Log("Manually draining pods to simulate eviction")
		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		// Wait for DrainController to complete drain
		event := helpers.WaitForHealthEventPhase(ctx, t, client, eventName, nvsentinelv1alpha1.PhaseDrained)
		t.Logf("DrainController completed drain (phase=%s)", event.Status.Phase)

		// Verify PodsDrained condition is set
		helpers.AssertPodsDrainedCondition(t, event)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		// Uncordon node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		helpers.DeleteNamespace(ctx, t, client, namespaceName)
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestDrainSkipOverride tests that drain can be skipped via override.
func TestDrainSkipOverride(t *testing.T) {
	feature := features.New("TestDrainSkipOverride").
		WithLabel("suite", "drain-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		workloadNamespace := "drain-skip-test"
		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		require.NoError(t, err)

		// Create test pods on the node
		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		return ctx
	})

	feature.Assess("Event with skip drain override skips drain phase", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Get current pod names
		pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), nodeName)
		require.NoError(t, err)
		var podNames []string
		for _, pod := range pods {
			if pod.Namespace == namespaceName {
				podNames = append(podNames, pod.Name)
			}
		}

		// Create event with skip drain override
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			WithSkipDrain(true). // Skip drain
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent with skip drain: %s", created.Name)

		// Wait for quarantine (drain is skipped, but quarantine should still happen)
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseQuarantined)

		// Verify event never reaches Draining phase
		helpers.AssertHealthEventNeverReachesPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDraining)

		// Verify pods are NOT evicted
		if len(podNames) > 0 {
			helpers.AssertPodsNeverDeleted(ctx, t, client, namespaceName, podNames)
		}

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		// Uncordon node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		helpers.DeleteNamespace(ctx, t, client, namespaceName)
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestDrainWithKubeSystemExclusion tests that kube-system pods are not evicted.
func TestDrainWithKubeSystemExclusion(t *testing.T) {
	feature := features.New("TestDrainWithKubeSystemExclusion").
		WithLabel("suite", "drain-controller")

	var kubeSystemPodNames []string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		// Record existing kube-system pods on this node
		pods, err := helpers.GetPodsOnNode(ctx, client.Resources(), nodeName)
		require.NoError(t, err)
		for _, pod := range pods {
			if pod.Namespace == "kube-system" && pod.Status.Phase == v1.PodRunning {
				kubeSystemPodNames = append(kubeSystemPodNames, pod.Name)
			}
		}
		t.Logf("Found %d kube-system pods on node %s", len(kubeSystemPodNames), nodeName)

		// Create user workload namespace
		workloadNamespace := "drain-exclusion-test"
		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		require.NoError(t, err)

		// Create test pods
		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		return ctx
	})

	feature.Assess("kube-system pods are not evicted during drain", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create fatal event
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)

		// Wait for drain to start
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDraining)

		// Verify kube-system pods are NOT deleted
		if len(kubeSystemPodNames) > 0 {
			helpers.AssertPodsNeverDeleted(ctx, t, client, "kube-system", kubeSystemPodNames)
			t.Logf("Verified %d kube-system pods were not evicted", len(kubeSystemPodNames))
		}

		// Manually drain user workload to complete the drain
		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		// Wait for drain to complete
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDrained)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		// Uncordon node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		helpers.DeleteNamespace(ctx, t, client, namespaceName)
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestDrainPhaseSequence tests the full phase sequence through drain.
func TestDrainPhaseSequence(t *testing.T) {
	feature := features.New("TestDrainPhaseSequence").
		WithLabel("suite", "drain-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		workloadNamespace := "drain-sequence-test"
		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		require.NoError(t, err)

		podTemplate := helpers.NewGPUPodSpec(workloadNamespace, 1)
		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, []string{nodeName}, podTemplate)

		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		ctx = context.WithValue(ctx, keyNodeName, nodeName)
		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		return ctx
	})

	feature.Assess("HealthEvent progresses through New → Quarantined → Draining → Drained", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		client, err := c.NewClient()
		require.NoError(t, err)

		// Create fatal event
		event := helpers.NewHealthEventCRD(nodeName).
			WithSource("e2e-test").
			WithCheckName("GpuXidError").
			WithFatal(true).
			WithHealthy(false).
			WithErrorCodes("79").
			Build()

		created := helpers.CreateHealthEventCRD(ctx, t, client, event)
		t.Logf("Created HealthEvent: %s", created.Name)

		// Define expected phase sequence
		sequence := helpers.ExpectedPhaseSequence{
			nvsentinelv1alpha1.PhaseQuarantined,
			nvsentinelv1alpha1.PhaseDraining,
		}

		// Wait for sequence up to Draining
		helpers.WaitForHealthEventPhaseSequence(ctx, t, client, created.Name, sequence)

		// Manually drain to complete
		helpers.DrainRunningPodsInNamespace(ctx, t, client, namespaceName)

		// Wait for Drained
		helpers.WaitForHealthEventPhase(ctx, t, client, created.Name, nvsentinelv1alpha1.PhaseDrained)

		t.Log("Successfully verified phase sequence: New → Quarantined → Draining → Drained")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)
		namespaceName := ctx.Value(keyNamespace).(string)

		// Uncordon node
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		if err == nil && node.Spec.Unschedulable {
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		helpers.DeleteNamespace(ctx, t, client, namespaceName)
		helpers.DeleteAllHealthEventCRDs(ctx, t, client)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
