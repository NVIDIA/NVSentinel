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

	// extrrSyslogDaemonSetName and extrrGPUHealthMonitorDaemonSetName are the
	// DaemonSet names whose nodeSelector gates are verified by the managed=false
	// eviction check. GPUHealthMonitorDaemonSetName lives in gpu_health_monitor_test.go
	// which is amd64_group only, so we define our own here.
	extrrSyslogDaemonSetName        = "syslog-health-monitor-regular"
	extrrGPUHealthMonitorDaemonSetName = "gpu-health-monitor-dcgm-4.x"
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
		nodeName        string
		monitorNodeName string
		crName          = "extrr-lifecycle-happy"
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err = helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err)
		t.Logf("using node %s for ExtRR lifecycle test", nodeName)

		// Find a KWOK node that currently has a syslog-health-monitor pod running.
		// Monitors use nodeSelector on detection labels and only run on KWOK nodes
		// (which carry GPU labels); the real kind worker lacks these. We use this
		// node to verify the full DaemonSet eviction + rescheduling lifecycle.
		pods, err := helpers.ListDaemonSetPods(ctx, client, helpers.NVSentinelNamespace, extrrSyslogDaemonSetName)
		require.NoError(t, err)
		for _, pod := range pods {
			if pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName != "" {
				monitorNodeName = pod.Spec.NodeName
				break
			}
		}
		require.NotEmpty(t, monitorNodeName,
			"expected at least one running %s pod to identify a monitor-hosting node",
			extrrSyslogDaemonSetName)
		t.Logf("using KWOK node %s for health-monitor eviction checks", monitorNodeName)

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

	// ADR-040: while managed=false is set the labeler must strip detection labels
	// and health-monitor DaemonSet pods must self-evict. We exercise this on two
	// levels:
	//
	//   1. Label stripping on the ExtRR-released node (real kind worker). We stamp
	//      the labels manually because DCGM / driver fake DaemonSets only schedule
	//      on KWOK fake nodes that carry GPU labels, so the real worker would never
	//      have them organically in the test environment.
	//
	//   2. Full DaemonSet eviction on a KWOK node (found in Setup). These nodes
	//      already carry detection labels set by the labeler and have syslog /
	//      gpu-health-monitor pods running. We apply managed=false to simulate
	//      what the ERR reconciler does, verify pod eviction, then verify pods
	//      return after managed=false is removed.
	feature.Assess("labeler strips detection labels and health-monitor pods evict while node is released",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			// ── Part 1: label stripping on the real ExtRR-targeted node ──────
			require.NoError(t, helpers.SetNodeLabel(ctx, client, nodeName,
				"nvsentinel.dgxc.nvidia.com/dcgm.version", "4.x"),
				"failed to stamp dcgm.version on ExtRR node")
			require.NoError(t, helpers.SetNodeLabel(ctx, client, nodeName,
				"nvsentinel.dgxc.nvidia.com/driver.installed", "true"),
				"failed to stamp driver.installed on ExtRR node")

			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					return false
				}
				_, hasDCGM := node.Labels["nvsentinel.dgxc.nvidia.com/dcgm.version"]
				_, hasDriver := node.Labels["nvsentinel.dgxc.nvidia.com/driver.installed"]
				return !hasDCGM && !hasDriver
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
				"labeler must strip detection labels while managed=false is set on %s", nodeName)
			t.Logf("detection labels stripped from %s — labeler gate on ExtRR node confirmed", nodeName)

			// ── Part 2: DaemonSet pod eviction on the monitor-hosting KWOK node ─
			t.Logf("applying managed=false to KWOK node %s to trigger monitor self-eviction", monitorNodeName)
			require.NoError(t, helpers.SetNodeLabel(ctx, client, monitorNodeName, managedLabelKey, "false"),
				"failed to apply managed=false to KWOK node")

			for _, dsName := range []string{extrrSyslogDaemonSetName, extrrGPUHealthMonitorDaemonSetName} {
				dsName := dsName
				require.Eventually(t, func() bool {
					pods, err := helpers.ListDaemonSetPods(ctx, client, helpers.NVSentinelNamespace, dsName)
					if err != nil {
						t.Logf("listing pods for %s: %v", dsName, err)
						return false
					}
					for _, pod := range pods {
						if pod.Spec.NodeName == monitorNodeName {
							t.Logf("%s pod %s still on %s (phase=%s)",
								dsName, pod.Name, monitorNodeName, pod.Status.Phase)
							return false
						}
					}
					return true
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
					"%s pod must evict from %s after managed=false is set", dsName, monitorNodeName)
				t.Logf("%s: pod evicted from %s — DaemonSet self-eviction confirmed", dsName, monitorNodeName)
			}

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

			// Remove managed=false from the KWOK node and verify monitors reschedule.
			t.Logf("removing managed=false from KWOK node %s; monitors must reschedule", monitorNodeName)
			require.NoError(t, helpers.RemoveNodeLabel(ctx, client, monitorNodeName, managedLabelKey),
				"failed to remove managed=false from KWOK node")

			for _, dsName := range []string{extrrSyslogDaemonSetName, extrrGPUHealthMonitorDaemonSetName} {
				dsName := dsName
				require.Eventually(t, func() bool {
					pods, err := helpers.ListDaemonSetPods(ctx, client, helpers.NVSentinelNamespace, dsName)
					if err != nil {
						t.Logf("listing pods for %s: %v", dsName, err)
						return false
					}
					for _, pod := range pods {
						if pod.Spec.NodeName == monitorNodeName && pod.Status.Phase == corev1.PodRunning {
							return true
						}
					}
					return false
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
					"%s pod must reschedule on %s after managed=false is removed", dsName, monitorNodeName)
				t.Logf("%s: pod rescheduled on %s — monitor restoration confirmed", dsName, monitorNodeName)
			}

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
		// Remove managed=false from the KWOK node in case the test failed mid-way.
		if monitorNodeName != "" {
			if err := helpers.RemoveNodeLabel(ctx, client, monitorNodeName, managedLabelKey); err != nil {
				t.Logf("removing managed label from %s: %v", monitorNodeName, err)
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
