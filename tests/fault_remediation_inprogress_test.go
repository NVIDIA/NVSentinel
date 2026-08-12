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
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

// inProgressRetryTimeout bounds how long we wait for fault-remediation to retry an
// event that was received while an equivalent maintenance CR was still InProgress.
// The retry requeue fires every 30s, and the janitor then needs to complete the new
// CR, so a few minutes is enough headroom without stalling a failing run for the
// default 10 minutes.
const inProgressRetryTimeout = 6 * time.Minute

// TestEventDuringInProgressCRIsRetriedAfterCompletion is the E2E regression test for
// issue #1536: a remediation-ready health event that arrives while an equivalent
// maintenance CR is still InProgress must not be dropped permanently. Once the CR
// reaches a terminal state, fault-remediation must reconsider the event and, because
// the event was created after the running remediation session began, create a new CR.
//
// Flow:
//  1. Scale janitor-provider to 0 so maintenance CRs stay InProgress deterministically:
//     the janitor treats the unreachable CSP provider as a transient error and retries
//     without failing the CR. (The janitor deployment itself must stay up because it
//     serves the RebootNode validating webhook; scaling it down would block CR creation.)
//  2. Send fatal event A — FQ quarantines, ND drains, FR creates CR-1 (stays InProgress).
//  3. Send fatal event B for a different GPU (distinct entity so the platform connector
//     does not deduplicate it) — FR evaluates it against the in-progress CR-1, which is
//     confirmed via the fault-remediation logs.
//  4. Scale janitor-provider back to 1 — CR-1 completes.
//  5. Verify FR automatically retries event B and a second CR is created and completed.
//     Without the fix, event B is checkpointed on the skip path and never retried, so
//     the second CR never appears and this test times out at step 5.
func TestEventDuringInProgressCRIsRetriedAfterCompletion(t *testing.T) {
	feature := features.New("TestEventDuringInProgressCRIsRetriedAfterCompletion").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("event received while CR is InProgress is remediated after the CR completes",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := testCtx.NodeName

			t.Log("Step 1: Scaling janitor-provider to 0 so the first maintenance CR stays InProgress")
			require.NoError(t, helpers.ScaleDeployment(ctx, t, client, "janitor-provider", helpers.NVSentinelNamespace, 0))
			helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
				"janitor-provider", helpers.NVSentinelNamespace, 2*time.Minute)

			t.Log("Step 2: Triggering first fault; CR-1 should be created and stay InProgress")
			helpers.TriggerFullRemediationFlow(ctx, t, client, nodeName, 15)

			cr1 := waitForRebootNodeCRForNode(ctx, t, client, nodeName)
			t.Logf("CR-1 created: %s", cr1.GetName())

			completionTime, found, _ := unstructured.NestedString(cr1.Object, "status", "completionTime")
			require.True(t, !found || completionTime == "",
				"CR-1 must still be InProgress for this scenario, got completionTime=%q", completionTime)

			t.Log("Step 3: Sending second fatal event while CR-1 is InProgress")

			const eventBMessage = "issue-1536: post-reboot XID 79 while CR-1 still InProgress"

			// A different GPU entity keeps the platform connector from deduplicating
			// the event against event A, while the recommended action still maps to
			// the same "restart" equivalence group covered by CR-1.
			eventB := helpers.NewHealthEvent(nodeName).
				WithErrorCode("79").
				WithEntitiesImpacted([]helpers.EntityImpacted{{EntityType: "GPU", EntityValue: "1"}}).
				WithMessage(eventBMessage).
				WithRecommendedAction(15)
			helpers.SendHealthEvent(ctx, t, eventB)

			t.Log("Waiting for fault-quarantine to ingest the second event")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					return false
				}

				return strings.Contains(node.Annotations[helpers.QuarantineHealthEventAnnotationKey], eventBMessage)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
				"quarantine annotation should contain the second event's message")

			// Both the fixed and the unfixed reconciler log this line when they find
			// the covering CR still InProgress, so waiting on it guarantees event B was
			// evaluated inside the InProgress window in either build.
			t.Log("Waiting for fault-remediation to evaluate the second event against in-progress CR-1")
			waitForFaultRemediationLogLine(ctx, t, client,
				"CR exists and is in progress", cr1.GetName())

			t.Log("Step 4: Scaling janitor-provider back to 1 so CR-1 completes")
			require.NoError(t, helpers.ScaleDeployment(ctx, t, client, "janitor-provider", helpers.NVSentinelNamespace, 1))
			helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
				"janitor-provider", helpers.NVSentinelNamespace, 2*time.Minute)

			helpers.WaitForCRByName(ctx, t, client, cr1.GetName(), helpers.RebootNodeGVK)
			t.Logf("CR-1 %s completed", cr1.GetName())

			t.Log("Step 5: Waiting for the retried event to produce and complete a second CR")
			require.Eventually(t, func() bool {
				crNames, err := helpers.GetRebootNodeCRsForNode(ctx, client, nodeName)
				if err != nil {
					t.Logf("failed to list completed CRs: %v", err)
					return false
				}

				t.Logf("Completed CRs for node %s: %v", nodeName, crNames)

				return len(crNames) >= 2
			}, inProgressRetryTimeout, helpers.WaitInterval,
				"event received while CR-1 was InProgress must be retried after CR-1 completes "+
					"and produce a second CR (issue #1536)")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Restore janitor-provider loudly: leaving it at 0 replicas would make every later
		// test that depends on maintenance CRs completing fail with unrelated symptoms.
		providerRestored := false

		client, err := c.NewClient()
		if err != nil {
			t.Errorf("teardown could not build a client; janitor-provider may be left scaled to 0: %v", err)
		} else if scaleErr := helpers.ScaleDeployment(ctx, t, client, "janitor-provider",
			helpers.NVSentinelNamespace, 1); scaleErr != nil {
			t.Errorf("failed to restore janitor-provider to 1 replica: %v", scaleErr)
		} else {
			providerRestored = true
		}

		if testCtx != nil && testCtx.NodeName != "" {
			// Clear the second event's failure (GPU 1); TeardownFaultRemediation's
			// generic healthy event only covers the default GPU 0 entity.
			helpers.RecoverEntityFailure(ctx, t, testCtx.NodeName, "GPU", "1", "79")
		}

		ctx = helpers.TeardownFaultRemediation(ctx, t, c)

		// Verify the provider actually came back. This runs after the remaining cleanup
		// because the rollout helper fails the test fatally on timeout, which would
		// otherwise skip the cleanup steps above.
		if providerRestored {
			helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client,
				"janitor-provider", helpers.NVSentinelNamespace, 2*time.Minute)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// waitForRebootNodeCRForNode waits for any RebootNode CR targeting the node to exist,
// regardless of completion, and returns it. helpers.WaitForCR is not usable here
// because it waits for the CR to complete, and this test deliberately holds CRs in
// the InProgress state.
func waitForRebootNodeCRForNode(
	ctx context.Context, t *testing.T, client klient.Client, nodeName string,
) *unstructured.Unstructured {
	t.Helper()

	var result *unstructured.Unstructured

	require.Eventually(t, func() bool {
		crList, err := helpers.ListAllCRs(ctx, client, helpers.RebootNodeGVK)
		if err != nil {
			t.Logf("failed to list RebootNode CRs: %v", err)
			return false
		}

		for i := range crList.Items {
			item := &crList.Items[i]

			crNodeName, found, _ := unstructured.NestedString(item.Object, "spec", "nodeName")
			if found && crNodeName == nodeName {
				result = item
				return true
			}
		}

		return false
	}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
		"a RebootNode CR should be created for node %s", nodeName)

	return result
}

// waitForFaultRemediationLogLine waits until a running fault-remediation pod has a log
// line containing all of the given substrings.
func waitForFaultRemediationLogLine(
	ctx context.Context, t *testing.T, client klient.Client, substrings ...string,
) {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(client.RESTConfig())
	require.NoError(t, err, "failed to create kubernetes clientset")

	require.Eventually(t, func() bool {
		pods, err := clientset.CoreV1().Pods(helpers.NVSentinelNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=fault-remediation",
			FieldSelector: "status.phase=Running",
		})
		if err != nil || len(pods.Items) == 0 {
			return false
		}

		for _, pod := range pods.Items {
			logs, err := clientset.CoreV1().Pods(helpers.NVSentinelNamespace).
				GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
			if err != nil {
				continue
			}

			if logsContainLineWithAll(logs, substrings) {
				return true
			}
		}

		return false
	}, helpers.EventuallyWaitTimeout, helpers.WaitInterval,
		"fault-remediation logs should contain a line with all of %v", substrings)
}

func logsContainLineWithAll(logs []byte, substrings []string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		matches := true

		for _, substring := range substrings {
			if !strings.Contains(line, substring) {
				matches = false
				break
			}
		}

		if matches {
			return true
		}
	}

	return false
}
