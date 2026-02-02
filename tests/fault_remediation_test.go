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
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

func TestNewCRsAreCreatedAfterFaultsAreRemediated(t *testing.T) {
	feature := features.New("TestNewCRsAreCreatedAfterFaultsAreRemediated").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("new CR should be created on the same node after success", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// Trigger first fault and wait for CR
		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 15)
		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		t.Logf("First CR created and completed: %s", cr1.GetName())

		// Send healthy event to clear the fault
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		time.Sleep(10 * time.Second)

		// Trigger second fault
		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 15)

		// Verify that 2 CRs were created
		require.Eventually(t, func() bool {
			crList, err := helpers.GetRebootNodeCRsForNode(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return len(crList) == 2
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "should have 2 completed CRs")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediation(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
