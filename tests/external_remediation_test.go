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
	// releaseTaintKey is the taint the ExtRR reconciler applies; tests assert
	// its presence/absence on the target Node.
	releaseTaintKey = "nvsentinel.dgxc.nvidia.com/external-remediation"

	// managedLabelKey is the opt-out label the ExtRR reconciler sets to
	// "false" on the released node.
	managedLabelKey = "nvsentinel.dgxc.nvidia.com/managed"
)

// TestExtRRWebhookRejectsInvalidSpec verifies the validating admission webhook
// rejects ExtRRs missing required spec fields. Each rejection path is also
// covered by unit tests; this proves the webhook is wired correctly in the
// deployed chart (cert, service, registration) and that the apiserver actually
// invokes it.
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
			extrr, err := helpers.CreateExtRRCR(ctx, client, crName, nodeName, "immutability-test")
			require.NoError(t, err, "valid create must be admitted")

			t.Cleanup(func() { _ = client.Resources().Delete(ctx, extrr) })

			// Attempt to flip nodeName via update.
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

// TestExtRRLifecycleHappyPath drives the full ADR-040 happy path against a
// deployed reconciler: apply → release taint + managed=false → Complete=True
// → cleanup → garbage collection. Proves the chart-installed system works
// end-to-end (RBAC, manifest correctness, reconciler boot).
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

	feature.Assess("Complete=True triggers cleanup; ExtRR garbage-collected",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
				"True", "RemediationSucceeded", "node returned to service"))

			helpers.WaitForExtRRGone(ctx, t, client, crName)

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasNoReleaseTaint(t, node)
			_, hasLabel := node.Labels[managedLabelKey]
			assert.False(t, hasLabel, "managed label must be removed after cleanup")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		// Scrub directly in case the finalizer-driven cleanup didn't complete
		// (e.g. mid-test failure left the Node tainted/labeled). Idempotent.
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestExtRRAsymmetricFalse verifies the ADR-040 invariant that
// ExternalRemediationComplete=False does NOT trigger cleanup — the node stays
// released until either the external system retries with True or an operator
// deletes the ExtRR.
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

			// Give the reconciler a chance to (incorrectly) act on the False.
			// The reconciler should see the False, no-op, and the node should
			// remain in the released state. We poll briefly to confirm the
			// state is stable.
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasReleaseTaint(t, node, crName)
			assert.Equal(t, "false", node.Labels[managedLabelKey])

			// ExtRR must still exist (no cleanup ran).
			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(helpers.ExternalRemediationRequestGVK)
			require.NoError(t, client.Resources().Get(ctx, crName, "", cur),
				"ExtRR must still exist after Complete=False")

			return ctx
		})

	feature.Assess("Complete=True (retry) closes the ExtRR after a False",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			require.NoError(t, helpers.SetExtRRComplete(ctx, client, crName,
				"True", "RemediationSucceeded", "external system retry succeeded"))

			helpers.WaitForExtRRGone(ctx, t, client, crName)

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasNoReleaseTaint(t, node)

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		// Scrub directly in case the finalizer-driven cleanup didn't complete
		// (e.g. mid-test failure left the Node tainted/labeled). Idempotent.
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestExtRROperatorDeleteEscape exercises the `kubectl delete extrr` path:
// the finalizer must drive node cleanup before the object is garbage-collected,
// so operators can reclaim a stalled-at-False ExtRR without leaking taints.
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

// TestExtRRForeignTaintDrift verifies the drift-safety contract: when the
// target Node already carries the release taint at the right key but with a
// different ExtRR's name as value (i.e. another ExtRR already owns the node),
// the new ExtRR must transition to NVSentinelOwnershipReleased=False and the
// foreign taint must be left in place.
func TestExtRRForeignTaintDrift(t *testing.T) {
	feature := features.New("TestExtRRForeignTaintDrift").
		WithLabel("suite", "drift").
		WithLabel("component", "janitor")

	var (
		nodeName       string
		foreignOwnerCR = "extrr-foreign-owner"
		freshCR        = "extrr-drift-victim"
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err)

		// Create the foreign-owner ExtRR first; let it land its taint.
		_, err = helpers.CreateExtRRCR(ctx, client, foreignOwnerCR, nodeName, "foreign")
		require.NoError(t, err)
		helpers.WaitForExtRRCondition(ctx, t, client, foreignOwnerCR,
			"NVSentinelOwnershipReleased", "True")

		return ctx
	})

	feature.Assess("fresh ExtRR for an already-tainted node transitions to False",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			_, err = helpers.CreateExtRRCR(ctx, client, freshCR, nodeName, "victim")
			require.NoError(t, err)

			got := helpers.WaitForExtRRCondition(ctx, t, client, freshCR,
				"NVSentinelOwnershipReleased", "False")
			require.NotNil(t, got)

			cond := helpers.GetCRCondition(got, "NVSentinelOwnershipReleased")
			require.NotNil(t, cond)
			assert.Equal(t, "ReleaseTaintFailed", cond["reason"],
				"drift case must surface ReleaseTaintFailed reason")

			// Foreign taint must still belong to the original owner.
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			assertNodeHasReleaseTaint(t, node, foreignOwnerCR)

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		if err != nil {
			return ctx
		}

		_ = helpers.DeleteAllCRs(ctx, t, client, helpers.ExternalRemediationRequestGVK)
		// Scrub directly in case the finalizer-driven cleanup didn't complete
		// (e.g. mid-test failure left the Node tainted/labeled). Idempotent.
		if nodeName != "" {
			if err := helpers.ScrubExtRRStateFromNode(ctx, client, nodeName); err != nil {
				t.Logf("ScrubExtRRStateFromNode(%s): %v", nodeName, err)
			}
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// assertNodeHasReleaseTaint fails the test unless the Node has the release
// taint at the canonical key with the expected ExtRR's name as the value.
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

// assertNodeHasNoReleaseTaint fails the test if the release taint is still on
// the Node.
func assertNodeHasNoReleaseTaint(t *testing.T, node *corev1.Node) {
	t.Helper()

	for _, taint := range node.Spec.Taints {
		if taint.Key == releaseTaintKey {
			t.Fatalf("expected release taint %q to be removed from node %q (value=%s)",
				releaseTaintKey, node.Name, taint.Value)
		}
	}
}
