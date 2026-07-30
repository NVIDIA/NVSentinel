//go:build arm64_group
// +build arm64_group

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

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

// TestExtRRManagedOptOutSuppressesHealthMonitor proves the ADR-040 opt-out gate
// end-to-end through the real integration path we care about:
//
//	ExtRR created  ->  janitor stamps nvsentinel.dgxc.nvidia.com/managed=false on the Node
//	               ->  kubernetes-object-monitor sees the opt-out and suppresses emission
//	ExtRR Complete=True  ->  managed label cleared  ->  emission resumes
//
// The observable contract is the downstream node event: the monitor still detects
// the condition and writes its match annotation while opted out, but the gate in
// publisher.PublishHealthEvent short-circuits the publish, so no health event
// reaches the platform connector and no node event is produced. This mirrors the
// STORE_ONLY suppression contract asserted by TestKubernetesObjectMonitorWithStoreOnlyStrategy.
func TestExtRRManagedOptOutSuppressesHealthMonitor(t *testing.T) {
	feature := features.New("ExtRR managed=false suppresses health-monitor emission").
		WithLabel("suite", "managed-optout").
		WithLabel("component", "kubernetes-object-monitor")

	const (
		crName          = "extrr-managed-optout"
		nodeEventType   = "node-test-condition"
		nodeEventReason = "node-test-conditionIsNotHealthy"
	)

	var nodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("using node %s for managed opt-out test", nodeName)

		// Clean slate so the baseline assertion cannot pass on a stale event.
		require.NoError(t, helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, nodeEventType, nodeEventReason))

		return ctx
	})

	// Baseline: with the node under NVSentinel management, an unhealthy condition
	// must produce a node event. This proves the mechanism works, so the later
	// suppression assertion cannot pass simply because the monitor is broken.
	feature.Assess("baseline: condition triggers node event when managed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("setting %s=False on node %s", testConditionType, nodeName)
		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		helpers.WaitForNodeEvent(ctx, t, client, nodeName, v1.Event{
			Type:   nodeEventType,
			Reason: nodeEventReason,
		})
		t.Log("baseline node event observed")

		// Recover: clear the condition and wait for the monitor to drop its match
		// annotation so the next unhealthy transition is a fresh one.
		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			ann, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]

			return !exists || ann == ""
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		require.NoError(t, helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, nodeEventType, nodeEventReason))

		return ctx
	})

	// Opt out via ExtRR and assert the same condition no longer produces a node event.
	feature.Assess("ExtRR opt-out suppresses node event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateExtRRCR(ctx, client, crName, nodeName, "managed-optout")
		require.NoError(t, err)

		helpers.WaitForExtRRCondition(ctx, t, client, crName, "NVSentinelOwnershipReleased", "True")

		// Confirm the janitor actually stamped the opt-out label before we rely on it.
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			return node.Labels[managedLabelKey] == "false"
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "node must carry managed=false after ExtRR apply")

		t.Logf("re-triggering %s=False on opted-out node %s", testConditionType, nodeName)
		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		// The monitor still detects the match and writes its annotation; only the
		// publish is gated. Wait for the annotation so we know reconciliation ran
		// before asserting the (absent) downstream event.
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			ann, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]

			return exists && ann != ""
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "monitor must still detect the match while opted out")

		t.Log("match annotation present; asserting the node event was suppressed")
		helpers.EnsureNodeEventNotPresent(ctx, t, client, nodeName, nodeEventType, nodeEventReason)

		return ctx
	})

	// Complete the ExtRR (clears managed=false) and prove emission resumes.
	feature.Assess("completing ExtRR restores emission", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// Clear the condition first so the monitor drops its match state while still
		// opted out, then complete the ExtRR to lift the gate.
		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)
		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return false
			}

			ann, exists := node.Annotations[helpers.K8sObjectMonitorAnnotationKey]

			return !exists || ann == ""
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

		require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
			"True", "RemediationSucceeded", "node returned to service"))
		helpers.WaitForNodeReleaseStateCleared(ctx, t, client, nodeName)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err)
		_, hasLabel := node.Labels[managedLabelKey]
		require.False(t, hasLabel, "managed label must be gone after ExtRR completion")

		require.NoError(t, helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, nodeEventType, nodeEventReason))

		t.Logf("re-triggering %s=False on re-managed node %s", testConditionType, nodeName)
		helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionFalse)

		helpers.WaitForNodeEvent(ctx, t, client, nodeName, v1.Event{
			Type:   nodeEventType,
			Reason: nodeEventReason,
		})
		t.Log("emission resumed after ExtRR completion")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		if nodeName != "" {
			helpers.SetNodeConditionStatus(ctx, t, client, nodeName, v1.NodeConditionType(testConditionType), v1.ConditionTrue)
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)

		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}

			_ = helpers.DeleteExistingNodeEvents(ctx, t, client, nodeName, nodeEventType, nodeEventReason)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
