//go:build amd64_group && mongodb
// +build amd64_group,mongodb

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

// IMPORTANT — Direct MongoDB manipulation:
// These tests delete resume tokens directly from MongoDB via mongosh to force
// the modules to rely on their cold-start DB queries (not the change stream)
// to discover events that arrived while they were offline. Without token
// deletion, a valid resume token would let the change stream replay missed
// events, making it impossible to verify that the cold-start code path works.
//
// This pattern (direct MongoDB manipulation) is specific to cold-start and
// stale-resume-token tests and should NOT be used as a general pattern.
// Normal E2E tests should interact with MongoDB indirectly through the
// application's APIs (e.g., sending health events via the simple-health-client,
// checking node labels/annotations via the Kubernetes API).

package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

// TestFaultRemediationColdStart verifies that fault-remediation correctly
// discovers and processes health events via its cold-start DB query when
// the change stream cannot replay them.
//
// Flow:
//  1. Scale FR to 0 (offline, change stream stops)
//  2. Send a fatal health event — FQ quarantines, ND drains the node
//  3. Wait for drain-succeeded (event is now ready for FR to process)
//  4. Delete FR's resume token from MongoDB (stream can't replay)
//  5. Scale FR back to 1 — cold start query is the only path to find it
//  6. Verify FR creates a maintenance CR
func TestFaultRemediationColdStart(t *testing.T) {
	feature := features.New("TestFaultRemediationColdStart").
		WithLabel("suite", "cold-start")

	var (
		testCtx  *helpers.RemediationTestContext
		mongoPod string
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")

		client, err := c.NewClient()
		require.NoError(t, err)

		mongoPod = helpers.GetMongoDBPrimaryPodName(ctx, t, client)
		t.Logf("Using MongoDB pod: %s", mongoPod)

		return newCtx
	})

	feature.Assess("FR processes missed event via cold-start query", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		restConfig := client.RESTConfig()
		nodeName := testCtx.NodeName

		// Step 1: Scale FR to 0.
		t.Log("Step 1: Scaling fault-remediation to 0")
		err = helpers.ScaleDeployment(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace, 0)
		require.NoError(t, err)

		helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
			"fault-remediation", helpers.NVSentinelNamespace, 2*time.Minute)

		// Step 2: Send a fatal event with a unique message while FR is down.
		const frColdStartMessage = "cold-start-fr-test XID 79 injected while FR offline"

		t.Log("Step 2: Sending fatal event while FR is offline")
		fatalEvent := helpers.NewHealthEvent(nodeName).
			WithErrorCode("79").
			WithMessage(frColdStartMessage).
			WithRecommendedAction(15)
		helpers.SendHealthEvent(ctx, t, fatalEvent)

		// Wait for FQ to quarantine (cordon) the node.
		t.Log("Waiting for node to be cordoned by FQ")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			return node.Spec.Unschedulable
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		// Verify the quarantine annotation contains our unique message,
		// confirming FQ ingested the specific event we sent.
		t.Log("Verifying quarantine annotation contains our event message")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			ann := node.Annotations[helpers.QuarantineHealthEventAnnotationKey]

			return strings.Contains(ann, frColdStartMessage)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"quarantine annotation should contain the cold-start event message")

		// Step 3: Wait for ND to drain the node (happens without FR).
		t.Log("Step 3: Waiting for drain-succeeded")
		helpers.WaitForNodeLabel(ctx, t, client, nodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)

		// Step 4: Delete FR's resume token so the change stream starts
		// fresh from "now" and cannot replay the missed event.
		t.Log("Step 4: Deleting FR resume token from MongoDB")
		helpers.DeleteResumeToken(ctx, t, restConfig, mongoPod, "fault-remediation")

		// Step 5: Scale FR back to 1. The change stream starts from
		// "now" (no resume token), so the only way FR can find the
		// quarantined+drained event is via its cold-start DB query.
		t.Log("Step 5: Scaling fault-remediation back to 1")
		err = helpers.ScaleDeployment(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace, 1)
		require.NoError(t, err)

		helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
			"fault-remediation", helpers.NVSentinelNamespace, 2*time.Minute)

		// Step 6: Verify FR created a maintenance CR via cold start
		// and that the node's remediation annotation references it.
		t.Log("Step 6: Waiting for maintenance CR")
		cr := helpers.WaitForCR(ctx, t, client, nodeName, helpers.RebootNodeGVK)
		crName := cr.GetName()
		t.Logf("CR created via cold start: %s", crName)

		// Verify the CR targets the correct node.
		crNodeName, _, _ := unstructured.NestedString(cr.Object, "spec", "nodeName")
		assert.Equal(t, nodeName, crNodeName, "CR spec.nodeName should match the test node")

		// Verify the FR remediation annotation on the node references this CR.
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)

		annotationValue, exists := node.Annotations["latestFaultRemediationState"]
		require.True(t, exists, "node should have the FR remediation annotation after cold start")
		assert.Contains(t, annotationValue, crName,
			"FR remediation annotation should reference the CR created via cold start")
		t.Logf("FR annotation verified: contains CR %s", crName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err == nil {
			_ = helpers.ScaleDeployment(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace, 1)
		}

		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

// TestNodeDrainerColdStart verifies that node-drainer correctly discovers
// and processes health events via its cold-start DB query when the change
// stream cannot replay them.
//
// Flow:
//  1. Create test pods on the node
//  2. Scale ND to 0 (offline, change stream stops)
//  3. Send a fatal event — FQ quarantines the node (event is Quarantined+NotStarted)
//  4. Delete ND's resume token from MongoDB (stream can't replay)
//  5. Scale ND back to 1 — cold start query is the only path to find it
//  6. Verify ND starts draining and completes
func TestNodeDrainerColdStart(t *testing.T) {
	feature := features.New("TestNodeDrainerColdStart").
		WithLabel("suite", "cold-start")

	var (
		ndCtx    *helpers.NodeDrainerTestContext
		mongoPod string
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, ndCtx = helpers.SetupNodeDrainerTest(ctx, t, c, "data/nd-all-modes.yaml", "cold-start-nd-test")

		client, err := c.NewClient()
		require.NoError(t, err)

		mongoPod = helpers.GetMongoDBPrimaryPodName(ctx, t, client)
		t.Logf("Using MongoDB pod: %s", mongoPod)

		return newCtx
	})

	feature.Assess("ND processes missed event via cold-start query", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		restConfig := client.RESTConfig()
		nodeName := ndCtx.NodeName

		// Step 1: Create test pods that will need draining.
		t.Log("Step 1: Creating test pods")
		podNames := helpers.CreatePodsFromTemplate(ctx, t, client, "data/busybox-pods.yaml", nodeName, "cold-start-nd-test")
		helpers.WaitForPodsRunning(ctx, t, client, "cold-start-nd-test", podNames)

		// Step 2: Scale ND to 0.
		t.Log("Step 2: Scaling node-drainer to 0")
		err = helpers.ScaleDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace, 0)
		require.NoError(t, err)

		helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
			"node-drainer", helpers.NVSentinelNamespace, 2*time.Minute)

		// Step 3: Send a fatal event with a unique message while ND is down.
		// FQ quarantines the node, but ND can't process it. The event
		// sits in the DB as Quarantined + NotStarted.
		const ndColdStartMessage = "cold-start-nd-test XID 79 injected while ND offline"

		t.Log("Step 3: Sending fatal event while ND is offline")
		fatalEvent := helpers.NewHealthEvent(nodeName).
			WithErrorCode("79").
			WithMessage(ndColdStartMessage)
		helpers.SendHealthEvent(ctx, t, fatalEvent)

		t.Log("Waiting for node to be cordoned by FQ")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			return node.Spec.Unschedulable
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		// Verify the quarantine annotation contains our unique message,
		// confirming FQ ingested the specific event we sent.
		t.Log("Verifying quarantine annotation contains our event message")
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			ann := node.Annotations[helpers.QuarantineHealthEventAnnotationKey]

			return strings.Contains(ann, ndColdStartMessage)
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
			"quarantine annotation should contain the cold-start event message")

		// Step 4: Delete ND's resume token so the change stream starts
		// fresh and cannot replay the missed event.
		t.Log("Step 4: Deleting ND resume token from MongoDB")
		helpers.DeleteResumeToken(ctx, t, restConfig, mongoPod, "node-drainer")

		// Step 5: Verify no draining label exists yet (ND is still down).
		t.Log("Step 5: Confirming node is NOT draining while ND is offline")
		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)

		labelVal := node.Labels[statemanager.NVSentinelStateLabelKey]
		assert.NotEqual(t, helpers.DrainingLabelValue, labelVal,
			"node must not be draining while ND is offline")

		// Step 6: Scale ND back to 1. The change stream starts from
		// "now", so the only way ND can find the Quarantined+NotStarted
		// event is via its cold-start DB query.
		t.Log("Step 6: Scaling node-drainer back to 1")

		err = helpers.ScaleDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace, 1)
		require.NoError(t, err)

		helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
			"node-drainer", helpers.NVSentinelNamespace, 2*time.Minute)

		// Step 7: Verify ND processed the event via cold start.
		// On KWOK nodes the drain may complete instantly (no real pods to evict
		// in monitored namespaces), jumping straight to drain-succeeded and
		// skipping the draining label. Accept either label as proof that ND
		// picked up and processed the missed event.
		t.Log("Step 7: Waiting for drain-succeeded label (cold start processed the event)")
		helpers.WaitForNodeLabel(ctx, t, client, nodeName,
			statemanager.NVSentinelStateLabelKey, helpers.DrainSucceededLabelValue)
		t.Log("ND processed the missed event via cold start — drain-succeeded")

		// Verify the quarantine annotation still contains our specific message,
		// confirming it was our event that was processed.
		node, err = helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)

		ann := node.Annotations[helpers.QuarantineHealthEventAnnotationKey]
		assert.Contains(t, ann, ndColdStartMessage,
			"quarantine annotation should still reference our specific cold-start event")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err == nil {
			_ = helpers.ScaleDeployment(ctx, t, client, "node-drainer", helpers.NVSentinelNamespace, 1)
		}

		return helpers.TeardownNodeDrainer(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
