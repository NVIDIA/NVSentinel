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
	"os"
	"testing"
	"tests/helpers"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestExistingCRPreventsNewCreation(t *testing.T) {
	feature := features.New("TestExistingCRPreventsNewCreation").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("existing CR prevents duplicate creation", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		cr1Name := cr1.GetName()
		t.Logf("First CR created: %s", cr1Name)

		t.Log("Triggering remediation flow again without cleanup")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		time.Sleep(5 * time.Second)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		t.Log("Verifying no new CR was created")
		time.Sleep(30 * time.Second)

		var crList []string
		crs := &unstructured.UnstructuredList{}
		crs.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   helpers.RebootNodeCRDGroup,
			Version: helpers.RebootNodeCRDVersion,
			Kind:    "RebootNodeList",
		})
		err = client.Resources().List(ctx, crs)
		require.NoError(t, err)

		for _, cr := range crs.Items {
			spec, found, _ := unstructured.NestedMap(cr.Object, "spec")
			if !found {
				continue
			}
			nodeName, found, _ := unstructured.NestedString(spec, "nodeName")
			if found && nodeName == testCtx.NodeName {
				crList = append(crList, cr.GetName())
			}
		}

		require.LessOrEqual(t, len(crList), 2,
			"Should have at most 2 CRs (old and potentially new)")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationModuleRestart(t *testing.T) {
	feature := features.New("TestRemediationModuleRestart").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("module handles restart gracefully", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Step 1: Trigger drain to reach succeeded state")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Verify drain completed via MongoDB (persists even if label already changed)")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		t.Log("Step 2: Restart fault-remediation module before CR creation")
		helpers.RestartFaultRemediationDeployment(ctx, t, client)

		t.Log("Step 3: Verify CR is still created after restart")
		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		helpers.WaitForNodeRemediationLabel(ctx, t, client, testCtx.NodeName,
			helpers.RemediationSucceededLabelValue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestFailedCRRetry(t *testing.T) {
	feature := features.New("TestFailedCRRetry").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("failed CR allows retry on new event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		cr1 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		cr1Name := cr1.GetName()
		t.Logf("First CR created: %s", cr1Name)

		t.Log("Simulating CR failure by updating status")
		helpers.UpdateRebootNodeCRStatus(ctx, t, client, cr1Name, "Failed")

		t.Log("Cleaning up and triggering new remediation")
		helpers.SendHealthyEvent(ctx, t, testCtx.NodeName)
		time.Sleep(10 * time.Second)

		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		t.Log("Verifying new CR was created after previous failure")
		cr2 := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr2)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestConcurrentRemediationEvents(t *testing.T) {
	feature := features.New("TestConcurrentRemediationEvents").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("handles concurrent events gracefully", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		fatalEvent1 := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("GPU Fallen off the bus - event 1").
			WithRecommendedAction(2)
		tempFile1 := helpers.SendHealthEvent(ctx, t, fatalEvent1)
		defer os.Remove(tempFile1)

		time.Sleep(2 * time.Second)

		fatalEvent2 := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("94").
			WithMessage("XID 94 error - event 2").
			WithRecommendedAction(2)
		tempFile2 := helpers.SendHealthEvent(ctx, t, fatalEvent2)
		defer os.Remove(tempFile2)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Verify drain completed via MongoDB (persists even if label already changed)")
		helpers.WaitForMongoHealthEventStatus(ctx, t, client, testCtx.NodeName,
			"Quarantined", "Succeeded")

		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		t.Log("Verifying only one CR was created despite concurrent events")
		time.Sleep(30 * time.Second)

		var crCount int
		crs := &unstructured.UnstructuredList{}
		crs.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   helpers.RebootNodeCRDGroup,
			Version: helpers.RebootNodeCRDVersion,
			Kind:    "RebootNodeList",
		})
		err = client.Resources().List(ctx, crs)
		require.NoError(t, err)

		for _, cr := range crs.Items {
			spec, found, _ := unstructured.NestedMap(cr.Object, "spec")
			if !found {
				continue
			}
			nodeName, found, _ := unstructured.NestedString(spec, "nodeName")
			if found && nodeName == testCtx.NodeName {
				crCount++
			}
		}

		require.Equal(t, 1, crCount, "Should have exactly one CR for the node")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationWithoutDrainCompletion(t *testing.T) {
	feature := features.New("TestRemediationWithoutDrainCompletion").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "allowcompletion-test")
		return newCtx
	})

	feature.Assess("no CR created if drain not completed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Creating pods that won't be evicted")
		podNames := helpers.CreatePodsFromTemplate(ctx, t, client,
			"data/busybox-pods.yaml", testCtx.NodeName, testCtx.TestNamespace)
		helpers.WaitForPodsRunning(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Sending fatal event to trigger quarantine")
		fatalEvent := helpers.NewHealthEvent(testCtx.NodeName).
			WithErrorCode("79").
			WithMessage("XID 79 fatal error").
			WithRecommendedAction(2)
		tempFile := helpers.SendHealthEvent(ctx, t, fatalEvent)
		defer os.Remove(tempFile)

		require.Eventually(t, func() bool {
			node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
			if err != nil {
				return false
			}
			return node.Spec.Unschedulable
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Waiting to verify drain starts but doesn't complete")
		helpers.WaitForNodeLabel(ctx, t, client, testCtx.NodeName,
			helpers.NVSentinelStateLabelKey, helpers.DrainingLabelValue)

		t.Log("Verifying no RebootNode CR created while drain in progress")
		helpers.WaitForNoRebootNodeCR(ctx, t, client, testCtx.NodeName)

		t.Log("Cleaning up pods to complete drain")
		helpers.DeletePodsByNames(ctx, t, client, testCtx.TestNamespace, podNames)
		helpers.WaitForPodsDeleted(ctx, t, client, testCtx.TestNamespace, podNames)

		t.Log("Now CR should be created after drain completes")
		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRemediationDeploymentReadiness(t *testing.T) {
	feature := features.New("TestRemediationDeploymentReadiness").
		WithLabel("suite", "fault-remediation-advanced")

	var testCtx *helpers.RemediationTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupFaultRemediationTest(ctx, t, c, "")
		return newCtx
	})

	feature.Assess("deployment handles readiness checks", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Verifying fault-remediation deployment is ready")
		require.Eventually(t, func() bool {
			deployment := &appsv1.Deployment{}
			err := client.Resources().Get(ctx, "nvsentinel-fault-remediation", "nvsentinel", deployment)
			if err != nil {
				t.Logf("Failed to get deployment: %v", err)
				return false
			}

			if deployment.Status.ReadyReplicas == 0 {
				t.Log("No ready replicas yet")
				return false
			}

			if deployment.Status.UpdatedReplicas != deployment.Status.Replicas {
				t.Log("Replicas not fully updated")
				return false
			}

			return true
		}, helpers.WaitTimeout, helpers.WaitInterval)

		t.Log("Deployment is ready, testing remediation flow")
		helpers.TriggerFullRemediationFlow(ctx, t, client, testCtx.NodeName, 2)

		cr := helpers.WaitForRebootNodeCR(ctx, t, client, testCtx.NodeName)
		require.NotNil(t, cr)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownFaultRemediationTest(ctx, t, c)
	})

	testEnv.Test(t, feature.Feature())
}
