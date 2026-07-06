//go:build arm64_group
// +build arm64_group

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	releaseTaintKey = "nvsentinel.dgxc.nvidia.com/external-remediation"
	managedLabelKey = "nvsentinel.dgxc.nvidia.com/managed"
)

// TestExtRRWebhookRejectsInvalidSpec proves the webhook is wired through the
// chart (cert + service + registration) and the apiserver invokes it.
func TestExtRRWebhookRejectsInvalidSpec(t *testing.T) {
	feature := features.New("TestExtRRWebhookRejectsInvalidSpec").
		WithLabel("suite", "webhook").
		WithLabel("component", "janitor")

	feature.Assess("rejects ExtRR with nil spec", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateMalformedExtRR(ctx, client, "extrr-nil-spec", nil)
		require.Error(t, err, "creating an ExtRR without a spec must be rejected")
		assert.Contains(t, err.Error(), "spec is required")

		return ctx
	})

	feature.Assess("rejects ExtRR with nil spec.healthEvent", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateMalformedExtRR(ctx, client, "extrr-nil-he", map[string]interface{}{})
		require.Error(t, err, "creating an ExtRR without spec.healthEvent must be rejected")
		assert.Contains(t, err.Error(), "spec.healthEvent is required")

		return ctx
	})

	feature.Assess("rejects ExtRR with empty spec.healthEvent.nodeName", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		_, err = helpers.CreateExtRRCR(ctx, client, "extrr-empty-node", "", "empty-node-test")
		require.Error(t, err, "creating an ExtRR without nodeName must be rejected")
		assert.Contains(t, err.Error(), "nodeName is required")

		return ctx
	})

	feature.Assess("rejects update changing spec.healthEvent.nodeName (immutable)",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName, err := helpers.GetRealNodeName(ctx, client)
			require.NoError(t, err)

			crName := "extrr-immutable-node"
			_, err = helpers.CreateExtRRCR(ctx, client, crName, nodeName, "immutability-test")
			require.NoError(t, err, "valid create must be admitted")

			t.Cleanup(func() {
				_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
				_ = helpers.ScrubExtRRStateFromNode(ctx, client, nodeName)
			})

			// Wait for the reconciler to settle (Released=True → branch 6
			// no-op). Otherwise the apiserver returns 409 conflict on our
			// Update — the client's resourceVersion becomes a precondition,
			// and a stale rv short-circuits the request before admission
			// webhooks run.
			helpers.WaitForExtRRCondition(ctx, t, client, crName,
				"NVSentinelOwnershipReleased", "True")

			extrr := &unstructured.Unstructured{}
			extrr.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", extrr))
			require.NoError(t, unstructured.SetNestedField(
				extrr.Object, "different-node", "spec", "healthEvent", "nodeName"))

			err = client.Resources().Update(ctx, extrr)
			require.Error(t, err, "changing nodeName must be rejected by the webhook")
			assert.Contains(t, err.Error(), "nodeName cannot be changed")

			return ctx
		})

	testEnv.Test(t, feature.Feature())
}

// TestExtRRLifecycleHappyPath: apply → release → Complete=True → Node scrubbed.
func TestExtRRLifecycleHappyPath(t *testing.T) {
	feature := features.New("TestExtRRLifecycleHappyPath").
		WithLabel("suite", "lifecycle").
		WithLabel("component", "janitor")

	var (
		nodeName string
		crName   = "extrr-lifecycle-happy"
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err)
		t.Logf("using node %s for ExtRR lifecycle test", nodeName)

		return ctx
	})

	feature.Assess("apply releases the node (taint + managed=false)",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			_, err = helpers.CreateExtRRCR(ctx, client, crName, nodeName, "happy")
			require.NoError(t, err)

			got := helpers.WaitForExtRRCondition(ctx, t, client, crName,
				"NVSentinelOwnershipReleased", "True")
			require.NotNil(t, got)

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasReleaseTaint(t, node, crName)
			assert.Equal(t, "false", node.Labels[managedLabelKey],
				"managed label must be set to false after apply")

			return ctx
		})

	feature.Assess("Complete=True scrubs the Node; ExtRR stays as historical record",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
				"True", "RemediationSucceeded", "node returned to service"))

			// Per ADR-040 the ExtRR stays alive after cleanup; assert the Node
			// state, not CR garbage collection.
			helpers.WaitForNodeReleaseStateCleared(ctx, t, client, nodeName)

			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", cur),
				"ExtRR must remain in the cluster as a historical record after Complete=True")

			finalizers, _, _ := unstructured.NestedStringSlice(cur.Object, "metadata", "finalizers")
			assert.Contains(t, finalizers,
				"nvsentinel.dgxc.nvidia.com/external-remediation-cleanup",
				"cleanup finalizer must remain attached after Complete=True cleanup")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		// Belt-and-suspenders in case the finalizer-driven cleanup didn't complete.
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestExtRRAsymmetricFalse: ADR-040 Complete=False is a no-op; only a True
// retry or operator delete closes the ExtRR.
func TestExtRRAsymmetricFalse(t *testing.T) {
	feature := features.New("TestExtRRAsymmetricFalse").
		WithLabel("suite", "lifecycle").
		WithLabel("component", "janitor")

	var (
		nodeName string
		crName   = "extrr-asymmetric-false"
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err)

		_, err = helpers.CreateExtRRCR(ctx, client, crName, nodeName, "asym-false")
		require.NoError(t, err)
		helpers.WaitForExtRRCondition(ctx, t, client, crName,
			"NVSentinelOwnershipReleased", "True")

		return ctx
	})

	feature.Assess("Complete=False leaves taint + managed=false in place",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
				"False", "RemediationFailed", "external system gave up"))

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasReleaseTaint(t, node, crName)
			assert.Equal(t, "false", node.Labels[managedLabelKey])

			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", cur),
				"ExtRR must still exist after Complete=False")

			return ctx
		})

	feature.Assess("Complete=True (retry) scrubs the Node after a False",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
				"True", "RemediationSucceeded", "external system retry succeeded"))

			// True after False follows the same Node-cleanup + ExtRR-stays contract.
			helpers.WaitForNodeReleaseStateCleared(ctx, t, client, nodeName)

			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", cur),
				"ExtRR must remain after Complete=True retry following an earlier False")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		// Belt-and-suspenders in case the finalizer-driven cleanup didn't complete.
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestExtRROperatorDeleteEscape: kubectl delete on a stalled-at-False ExtRR
// must drive node cleanup before the apiserver garbage-collects it.
func TestExtRROperatorDeleteEscape(t *testing.T) {
	feature := features.New("TestExtRROperatorDeleteEscape").
		WithLabel("suite", "lifecycle").
		WithLabel("component", "janitor")

	var (
		nodeName string
		crName   = "extrr-operator-delete"
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err)

		_, err = helpers.CreateExtRRCR(ctx, client, crName, nodeName, "operator-delete")
		require.NoError(t, err)
		helpers.WaitForExtRRCondition(ctx, t, client, crName,
			"NVSentinelOwnershipReleased", "True")

		// Park at Complete=False so the only way to close is operator-delete.
		require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
			"False", "RemediationFailed", "stalled"))

		return ctx
	})

	feature.Assess("delete drives cleanup, removes taint + managed label, garbage-collects ExtRR",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", cur))
			require.NoError(t, client.Resources().Delete(ctx, cur))

			helpers.WaitForExtRRGone(ctx, t, client, crName)

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasNoReleaseTaint(t, node)
			_, hasLabel := node.Labels[managedLabelKey]
			assert.False(t, hasLabel, "managed label must be removed after operator-delete cleanup")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func assertNodeHasReleaseTaint(t *testing.T, node *corev1.Node, expectedOwner string) {
	t.Helper()

	for _, taint := range node.Spec.Taints {
		if taint.Key == releaseTaintKey {
			assert.Equal(t, expectedOwner, taint.Value,
				"release taint value must be the ExtRR's name (drift-safety)")
			return
		}
	}

	t.Fatalf("expected release taint %q on node %q, not present", releaseTaintKey, node.Name)
}

func assertNodeHasNoReleaseTaint(t *testing.T, node *corev1.Node) {
	t.Helper()

	for _, taint := range node.Spec.Taints {
		if taint.Key == releaseTaintKey {
			t.Fatalf("expected release taint %q to be removed from node %q (value=%s)",
				releaseTaintKey, node.Name, taint.Value)
		}
	}
}
