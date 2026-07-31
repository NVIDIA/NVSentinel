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
	"regexp"
	"strings"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// NOTE: This test reads the MongoDB ResumeTokens collection directly via mongosh
// (see helpers/mongodb.go). Direct MongoDB access is normally discouraged in E2E
// tests, but the change-stream checkpoint IS a MongoDB artifact with no
// application-level API — asserting on it requires reading the collection.
// The test is read-only with respect to ResumeTokens.

// TestFaultRemediationResumeTokenAdvances verifies that fault-remediation
// persists its MongoDB change-stream checkpoint after processing live events.
//
// Background (GitHub issue #1513): the MongoDB datastore adapter dropped the
// per-event resume token (hardcoded it to empty), and fault-remediation's
// safeMarkProcessed intentionally skips empty tokens (they normally mark
// synthesized cold-start events). As a result the fault-remediation checkpoint
// never advanced: every restart replayed the entire event history since the
// stored token (OOM/CrashLoopBackOff under large backlogs), and fresh installs
// never wrote a token at all, silently losing events that arrived while the
// pod was down.
//
// This test drives a live health event through the full pipeline
// (quarantine → drain → remediation CR) and asserts the fault-remediation
// resume token document was created/advanced as a result:
//
//  1. Record the current fault-remediation resume token document (may be absent)
//  2. Trigger the standard remediation flow on a test node
//  3. Wait for the RebootNode CR to be created and completed — proof that
//     fault-remediation processed a live change-stream event
//  4. Assert the resume token document now exists and differs from step 1
//
// On a build without the fix the token document never changes (and on a fresh
// install never appears), so step 4 times out and the test fails.
func TestFaultRemediationResumeTokenAdvances(t *testing.T) {
	feature := features.New("TestFaultRemediationResumeTokenAdvances").
		WithLabel("suite", "fault-remediation-resume-token")

	var (
		testCtx     *helpers.QuarantineTestContext
		mongoPod    string
		tokenBefore string
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Finding MongoDB pod")
		mongoPod = helpers.GetMongoDBPrimaryPodName(ctx, t, client)
		t.Logf("Using MongoDB pod: %s", mongoPod)

		// On small clusters a single quarantined node can exceed the default
		// circuit breaker percentage, and a TRIPPED breaker halts
		// fault-quarantine event dequeuing entirely — no quarantine, no
		// remediation, no test. Raise the threshold to 100% for the duration
		// of the test (the ConfigMap is backed up and restored in Teardown)
		// and reset any TRIPPED state left behind by earlier quarantine tests.
		var newCtx context.Context
		newCtx, testCtx, _ = helpers.SetupQuarantineTestWithOptions(ctx, t, c, "", &helpers.QuarantineSetupOptions{
			CircuitBreakerPercentage: 100,
			CircuitBreakerDuration:   "5m",
			CircuitBreakerState:      "CLOSED",
			CircuitBreakerCursorMode: "RESUME",
		})
		t.Logf("Selected test node: %s", testCtx.NodeName)

		// Make the node visible to TeardownFaultRemediation so it can clean
		// remediation labels/annotations and RebootNode CRs.
		newCtx = context.WithValue(newCtx, helpers.FRKeyNodeName, testCtx.NodeName)

		t.Log("Cleaning up existing rebootnode CRs")
		require.NoError(t, helpers.DeleteAllCRs(newCtx, t, client, helpers.RebootNodeGVK))

		// Record the checkpoint BEFORE driving any events so the assertion
		// can require it to advance (mere existence is not enough — a token
		// written by an earlier run must not satisfy this test).
		restConfig := client.RESTConfig()
		tokenBefore = helpers.GetResumeTokenDoc(newCtx, t, restConfig, client, mongoPod, "fault-remediation")
		require.True(t,
			strings.Contains(tokenBefore, "NOT_FOUND") || resumeTokenData(tokenBefore) != "",
			"unexpected ResumeTokens read for fault-remediation: %s", tokenBefore)
		t.Logf("fault-remediation resume token before test: %s", tokenBefore)

		return newCtx
	})

	feature.Assess("resume token advances after fault-remediation processes a live event",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)
			restConfig := client.RESTConfig()

			// Drive a live event through quarantine → drain → remediation.
			helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 15)

			// A completed CR proves fault-remediation consumed a live
			// change-stream event (the quarantined-and-drained update).
			cr := helpers.WaitForCR(ctx, t, client, testCtx.NodeName, helpers.RebootNodeGVK)
			t.Logf("Remediation CR created and completed: %s", cr.GetName())

			// The checkpoint is upserted right after each processed event;
			// give it a generous window to absorb mongosh exec latency.
			// Compare the extracted _data payloads rather than raw mongosh
			// output so transient stdout noise (warnings, partial reads)
			// can never satisfy the "token advanced" predicate: anything
			// that is not a well-formed token document parses to "" and the
			// poll simply retries.
			dataBefore := resumeTokenData(tokenBefore)

			var tokenAfter string
			require.Eventually(t, func() bool {
				tokenAfter = helpers.GetResumeTokenDoc(ctx, t, restConfig, client, mongoPod, "fault-remediation")
				dataAfter := resumeTokenData(tokenAfter)

				return dataAfter != "" && dataAfter != dataBefore
			}, 2*time.Minute, 5*time.Second,
				"fault-remediation must persist its change-stream checkpoint after processing "+
					"a live event (GitHub issue #1513: resume token dropped → replay/OOM on restart). "+
					"Token before: %s", tokenBefore)

			t.Logf("fault-remediation resume token after test: %s", tokenAfter)
			t.Log("Resume token advanced — checkpoint persistence verified")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Clears the fault (healthy event), removes remediation labels and
		// annotations from the node, and deletes RebootNode CRs.
		ctx = helpers.TeardownFaultRemediation(ctx, t, c)

		// Waits for the node to be fully clean, restores the original
		// fault-quarantine ConfigMap (default breaker percentage), and
		// restarts fault-quarantine to pick it up.
		return helpers.TeardownQuarantineTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

// resumeTokenDataRe matches the _data hex payload in a printjson'd resume
// token document, e.g. `resumeToken: { _data: '826A69...' }`.
var resumeTokenDataRe = regexp.MustCompile(`_data:\s*['"]([0-9A-Fa-f]+)['"]`)

// resumeTokenData extracts the resume token's _data payload from mongosh
// output. Returns "" for NOT_FOUND, empty, or malformed output so callers
// can treat only well-formed token documents as comparable.
func resumeTokenData(doc string) string {
	match := resumeTokenDataRe.FindStringSubmatch(doc)
	if match == nil {
		return ""
	}

	return match[1]
}
