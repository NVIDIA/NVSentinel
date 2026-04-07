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

package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// THIS TEST MUST RUN LAST IN THE SUITE — it modifies the MongoDB ResumeTokens collection.
//
// NOTE: This test directly manipulates MongoDB via mongosh (see helpers/mongodb.go).
// This pattern is specific to testing resume token recovery and should NOT be copied
// for other tests. Normal E2E tests should interact with MongoDB indirectly through
// the application's APIs (health events via simple-health-client, node state via the
// Kubernetes API, etc.).

// TestStaleResumeTokenRecovery verifies that fault-remediation recovers from a
// stale resume token without manual intervention.
//
// Background (GitHub issue #955): when a quiet cluster's oplog rolls over, the
// stored resume token becomes invalid. Without recovery logic the module enters
// an unrecoverable crash loop because:
//   - openChangeStreamWithRetry retries with the same bad token for 300s
//   - the liveness probe kills the pod at ~75s (health endpoint isn't up yet)
//   - the next restart hits the same stale token → CrashLoopBackOff
//
// The fix detects ChangeStreamHistoryLost errors, deletes the stale token, and
// opens a fresh stream. This test proves that path works end-to-end:
//
//  1. Insert a fake stale resume token into MongoDB
//  2. Restart fault-remediation
//  3. Assert the pod recovers and becomes ready (rollout completes)
//  4. Verify the stale token was cleaned up from MongoDB
func TestStaleResumeTokenRecovery(t *testing.T) {
	feature := features.New("TestStaleResumeTokenRecovery").
		WithLabel("suite", "stale-resume-token")

	var mongoPod string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Finding MongoDB pod")
		mongoPod = helpers.GetMongoDBPrimaryPodName(ctx, t, client)
		t.Logf("Using MongoDB pod: %s", mongoPod)

		// Clean up any leftover stale token from a previous failed run.
		// This makes the test idempotent — if a prior run crashed mid-test
		// and left the token in MongoDB, we clear it before starting.
		restConfig := client.RESTConfig()
		t.Log("Cleaning up any leftover stale resume token from previous runs")
		helpers.DeleteResumeToken(ctx, t, restConfig, mongoPod, "fault-remediation")

		// Force a fresh rollout to ensure fault-remediation is healthy.
		// A previous failed run may have left a stuck rollout with a
		// crash-looping pod; just waiting wouldn't recover from that.
		t.Log("Restarting fault-remediation to ensure clean state")
		err = helpers.RestartDeployment(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace)
		require.NoError(t, err, "fault-remediation must be healthy before test can proceed")
		t.Log("fault-remediation is healthy")

		return ctx
	})

	feature.Assess("fault-remediation recovers from stale resume token", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		restConfig := client.RESTConfig()

		// Step 1: Insert a stale resume token
		t.Log("Step 1: Inserting stale resume token for fault-remediation")
		helpers.InsertStaleResumeToken(ctx, t, restConfig, mongoPod, "fault-remediation")

		tokenDoc := helpers.GetResumeTokenDoc(ctx, t, restConfig, mongoPod, "fault-remediation")
		require.NotContains(t, tokenDoc, "NOT_FOUND", "stale token should have been inserted")
		t.Logf("Stale resume token inserted: %s", tokenDoc)

		// Step 2: Restart fault-remediation — should detect the bad token,
		// delete it, and open a fresh change stream. The pod should become ready.
		// We use a tight 2-minute timeout to fail fast on crash loops.
		t.Log("Step 2: Restarting fault-remediation (expecting recovery within 2 minutes)")
		err = triggerDeploymentRestart(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace)
		require.NoError(t, err)

		helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
			"fault-remediation", helpers.NVSentinelNamespace, 2*time.Minute)
		t.Log("Deployment rollout completed — fault-remediation recovered successfully")

		// Step 3: Verify the stale token was cleaned up from MongoDB.
		// After recovery the module should have deleted the bad token and
		// saved a fresh one (or no token if no events were processed yet).
		t.Log("Step 3: Verifying stale token was cleaned up")
		tokenDoc = helpers.GetResumeTokenDoc(ctx, t, restConfig, mongoPod, "fault-remediation")
		assert.NotContains(t, tokenDoc,
			helpers.StaleTokenMarker,
			"the original stale token should have been deleted during recovery")
		t.Logf("Resume token after recovery: %s", tokenDoc)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		restConfig := client.RESTConfig()

		// Delete any leftover stale token, then force a fresh rollout.
		// This mirrors the Setup cleanup so the cluster is left healthy
		// regardless of whether the test passed or failed.
		t.Log("Teardown: Cleaning up stale token and restarting fault-remediation")
		helpers.DeleteResumeToken(ctx, t, restConfig, mongoPod, "fault-remediation")

		err = helpers.RestartDeployment(ctx, t, client, "fault-remediation", helpers.NVSentinelNamespace)
		if err != nil {
			t.Logf("Warning: teardown restart failed: %v", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// triggerDeploymentRestart updates the pod template annotation to trigger a rollout,
// but does NOT wait for the rollout to complete (unlike RestartDeployment).
func triggerDeploymentRestart(
	ctx context.Context, t *testing.T, client klient.Client, name, namespace string,
) error {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, name, namespace, deployment); err != nil {
			return err
		}

		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

		return client.Resources().Update(ctx, deployment)
	})
	if err != nil {
		return fmt.Errorf("failed to trigger rollout restart for %s/%s: %w", namespace, name, err)
	}

	t.Logf("Triggered rollout restart for %s/%s (not waiting for completion)", namespace, name)
	return nil
}
