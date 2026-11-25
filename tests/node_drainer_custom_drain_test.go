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
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	customDrainNamespace = "custom-drain-test"
	// customDrainMessage is stored on the health event. The datastore check matches it.
	customDrainMessage             = "GPU fallen off the bus during custom drain test"
	faultRemediationDeploymentName = "fault-remediation"
	// customDrainIdleWait covers one read of an unfinished DrainRequest.
	// node-drainer reads that object about every 30 seconds, and the test pod must still be there.
	customDrainIdleWait = 45 * time.Second
)

func TestNodeDrainerCustomDrain(t *testing.T) {
	feature := features.New("TestNodeDrainerCustomDrain").
		WithLabel("suite", "node-drainer")

	state := &customDrainTestState{}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// Keep the context from before SetupNodeDrainerTest adds its own values.
		// Teardown uses this one, so it does not restore the config a second time.
		state.baseCtx = ctx

		// t.Cleanup runs even when setup stops the test. The feature teardown calls the same
		// function, and the done flag makes the second call a no-op.
		t.Cleanup(func() { state.teardown(ctx, t, c) })

		// fault-remediation treats AlreadyDrained as ready for a reboot. A reboot can delete the
		// test pod, so this test cannot tell that deletion apart from node-drainer deleting it.
		state.faultRemediationReplicas = deploymentReplicas(ctx, t, client, faultRemediationDeploymentName)
		state.restoreFaultRemediation = true

		t.Log("Scaling fault-remediation to 0")

		err = helpers.ScaleDeployment(ctx, t, client, faultRemediationDeploymentName, helpers.NVSentinelNamespace, 0)
		require.NoError(t, err)
		helpers.WaitForDeploymentRollout(ctx, t, client, faultRemediationDeploymentName, helpers.NVSentinelNamespace)

		helpers.SetupCustomDrain(ctx, t, client, &state.backup)

		state.configBackup, err = helpers.BackupConfigMap(ctx, client, "node-drainer", helpers.NVSentinelNamespace)
		require.NoError(t, err)

		newCtx, nodeDrainerCtx := helpers.SetupNodeDrainerTest(
			ctx, t, c, "data/nd-custom-drain.yaml", customDrainNamespace)
		state.testCtx = nodeDrainerCtx

		state.podNames = helpers.CreatePodsFromTemplate(
			newCtx, t, client, "data/busybox-pods.yaml", state.testCtx.NodeName, customDrainNamespace)
		helpers.WaitForPodsRunning(newCtx, t, client, customDrainNamespace, state.podNames)

		return newCtx
	})

	feature.Assess("custom drain completes when the DrainRequest is marked done", func(
		ctx context.Context, t *testing.T, c *envconf.Config,
	) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		event := helpers.NewHealthEvent(state.testCtx.NodeName).
			WithErrorCode("79").
			WithMessage(customDrainMessage)
		helpers.SendHealthEvent(ctx, t, event)

		drainRequest := helpers.WaitForDrainRequest(ctx, t, client, state.testCtx.NodeName)
		requireDrainRequest(t, drainRequest, state.testCtx.NodeName, customDrainNamespace, state.podNames[0])

		t.Log("Checking that the test pod stays while the DrainRequest is unfinished")
		require.Never(t, func() bool {
			return podDeleted(ctx, client, customDrainNamespace, state.podNames[0])
		}, customDrainIdleWait, helpers.WaitInterval, "node-drainer deleted the test pod")

		helpers.CompleteDrainRequest(ctx, t, client, drainRequest.GetName())

		require.Eventually(t, func() bool {
			_, _, method, ok := helpers.HealthEventDrainScope(
				ctx, t, client.RESTConfig(), client, state.testCtx.NodeName, customDrainMessage)

			return ok && method == "Custom"
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"health event should reach AlreadyDrained with method Custom")

		helpers.WaitForNoDrainRequest(ctx, t, client, state.testCtx.NodeName)

		pod := &v1.Pod{}
		require.NoError(t, client.Resources().Get(ctx, state.podNames[0], customDrainNamespace, pod))
		require.Nil(t, pod.DeletionTimestamp, "node-drainer deleted the test pod after the drain completed")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		state.teardown(ctx, t, c)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// customDrainTestState is the cluster state this test changes.
// teardown puts it back, including when setup stops the test.
type customDrainTestState struct {
	testCtx                  *helpers.NodeDrainerTestContext
	backup                   helpers.CustomDrainBackup
	configBackup             []byte
	baseCtx                  context.Context
	podNames                 []string
	restoreFaultRemediation  bool
	faultRemediationReplicas int32
	done                     bool
}

func (s *customDrainTestState) teardown(ctx context.Context, t *testing.T, c *envconf.Config) {
	t.Helper()

	if s.done {
		return
	}

	if s.baseCtx != nil {
		ctx = s.baseCtx
	}

	client, err := c.NewClient()
	require.NoError(t, err)

	s.done = true

	// Deferred calls run last one first, and each still runs when an earlier step stops the test.
	// fault-remediation is restored last. The template volume stays until the custom-drain config is gone.
	defer restoreFaultRemediationDeployment(ctx, t, client, s.restoreFaultRemediation, s.faultRemediationReplicas)
	defer func() {
		nodeName := ""
		if s.testCtx != nil {
			nodeName = s.testCtx.NodeName
		}

		helpers.TeardownCustomDrain(ctx, t, client, &s.backup, nodeName)
	}()

	if len(s.configBackup) > 0 {
		configCtx := context.WithValue(ctx, helpers.FRKeyNodeDrainerConfigMapBackup, s.configBackup)
		helpers.RestoreNodeDrainerConfig(configCtx, t, c)
	}

	if s.testCtx == nil {
		return
	}

	nodeCtx := context.WithValue(ctx, helpers.NDKeyNodeName, s.testCtx.NodeName)
	nodeCtx = context.WithValue(nodeCtx, helpers.NDKeyTestNamespace, s.testCtx.TestNamespace)

	helpers.TeardownNodeDrainer(nodeCtx, t, c)
}

func restoreFaultRemediationDeployment(
	ctx context.Context, t *testing.T, client klient.Client, restore bool, replicas int32,
) {
	t.Helper()

	if !restore {
		return
	}

	t.Logf("Restoring fault-remediation to %d replicas", replicas)

	err := helpers.ScaleDeployment(ctx, t, client, faultRemediationDeploymentName, helpers.NVSentinelNamespace, replicas)
	require.NoError(t, err)
	helpers.WaitForDeploymentRollout(ctx, t, client, faultRemediationDeploymentName, helpers.NVSentinelNamespace)
}

func requireDrainRequest(
	t *testing.T, drainRequest *unstructured.Unstructured, nodeName, namespace, podName string,
) {
	t.Helper()

	gotNode, found, err := unstructured.NestedString(drainRequest.Object, "spec", "nodeName")
	require.NoError(t, err)
	require.True(t, found, "DrainRequest spec.nodeName is missing")
	require.Equal(t, nodeName, gotNode)

	pods, found, err := unstructured.NestedStringSlice(drainRequest.Object, "spec", "podsToDrain", namespace)
	require.NoError(t, err)
	require.True(t, found, "DrainRequest spec.podsToDrain is missing namespace %s", namespace)
	require.Contains(t, pods, podName)
}

// podDeleted reports a pod that is gone or terminating. Other GET errors return false, so require.Never does not
// report a short API server error as a deletion.
func podDeleted(ctx context.Context, client klient.Client, namespace, name string) bool {
	pod := &v1.Pod{}

	err := client.Resources().Get(ctx, name, namespace, pod)
	if err != nil {
		return apierrors.IsNotFound(err)
	}

	return pod.DeletionTimestamp != nil
}

func deploymentReplicas(ctx context.Context, t *testing.T, client klient.Client, name string) int32 {
	t.Helper()

	deployment := &appsv1.Deployment{}
	require.NoError(t, client.Resources().Get(ctx, name, helpers.NVSentinelNamespace, deployment))

	if deployment.Spec.Replicas == nil {
		return 1
	}

	return *deployment.Spec.Replicas
}
