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

package helpers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

type RemediationTestContextKey int

const (
	FRKeyNodeName RemediationTestContextKey = iota
	FRKeyNamespace
	FRKeyNodeDrainerConfigMapBackup
)

const (
	RebootNodeCRDGroup   = "janitor.dgxc.nvidia.com"
	RebootNodeCRDVersion = "v1alpha1"
	RebootNodeCRDPlural  = "rebootnodes"
)

type RemediationTestContext struct {
	NodeName      string
	TestNamespace string
}

func SetupFaultRemediationTest(
	ctx context.Context, t *testing.T, c *envconf.Config, testNamespace string,
) (context.Context, *RemediationTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &RemediationTestContext{
		TestNamespace: testNamespace,
	}

	t.Log("Cleaning up existing rebootnode CRs")
	err = DeleteAllCRs(ctx, t, client, RebootNodeGVK)
	require.NoError(t, err)

	nodeName := SelectTestNodeFromUnusedPool(ctx, t, client)
	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, FRKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, FRKeyNamespace, testNamespace)

	if testNamespace != "" {
		t.Logf("Creating test namespace: %s", testNamespace)
		err = CreateNamespace(ctx, client, testNamespace)
		require.NoError(t, err)
	}

	return ctx, testCtx
}

//nolint:cyclop // Test teardown function with multiple cleanup steps
func TeardownFaultRemediation(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	nodeNameVal := ctx.Value(FRKeyNodeName)
	if nodeNameVal == nil {
		t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
		return ctx
	}

	nodeName := nodeNameVal.(string)

	testNamespaceVal := ctx.Value(FRKeyNamespace)

	testNamespace := ""
	if testNamespaceVal != nil {
		testNamespace = testNamespaceVal.(string)
	}

	t.Logf("Cleaning up node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, getErr := GetNodeByName(ctx, client, nodeName)
		if getErr != nil {
			return getErr
		}

		modified := false

		if node.Labels != nil {
			if _, exists := node.Labels[statemanager.NVSentinelStateLabelKey]; exists {
				delete(node.Labels, statemanager.NVSentinelStateLabelKey)

				modified = true
			}
		}

		if node.Annotations != nil {
			if _, exists := node.Annotations["latestFaultRemediationState"]; exists {
				delete(node.Annotations, "latestFaultRemediationState")

				modified = true
			}
		}

		if modified {
			return client.Resources().Update(ctx, node)
		}

		return nil
	})
	require.NoError(t, err)

	if testNamespace != "" {
		t.Logf("Cleaning up test namespace: %s", testNamespace)

		if err := DeleteNamespace(ctx, t, client, testNamespace); err != nil {
			t.Logf("Failed to delete test namespace %s: %v", testNamespace, err)
		}
	}

	t.Log("Cleaning up rebootnode CRs")

	err = DeleteAllCRs(ctx, t, client, RebootNodeGVK)
	if err != nil {
		t.Logf("Warning: Failed to cleanup rebootnode CRs: %v", err)
	}

	return ctx
}

// GetRebootNodeCRsForNode returns all RebootNode CR names for a specific node
// Only returns CRs that have a completionTime (i.e., completed CRs)
func GetRebootNodeCRsForNode(ctx context.Context, client klient.Client, nodeName string) ([]string, error) {
	crs := &unstructured.UnstructuredList{}
	crs.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   RebootNodeCRDGroup,
		Version: RebootNodeCRDVersion,
		Kind:    "RebootNodeList",
	})

	err := client.Resources().List(ctx, crs)
	if err != nil {
		return nil, err
	}

	var crList []string

	for _, cr := range crs.Items {
		spec, found, _ := unstructured.NestedMap(cr.Object, "spec")
		if !found {
			continue
		}

		crNodeName, found, _ := unstructured.NestedString(spec, "nodeName")
		if found && crNodeName == nodeName {
			// Only include CRs that have completed
			completionTime, found, _ := unstructured.NestedString(cr.Object, "status", "completionTime")
			if found && completionTime != "" {
				crList = append(crList, cr.GetName())
			}
		}
	}

	return crList, nil
}

func TriggerFullRemediationFlow(
	ctx context.Context, t *testing.T, client klient.Client, nodeName string, recommendedAction int,
) {
	t.Helper()
	t.Logf("Triggering full remediation flow for node %s", nodeName)

	t.Log("Step 1: Send fatal health event to trigger quarantine")

	fatalEvent := NewHealthEvent(nodeName).
		WithErrorCode("79").
		WithMessage("XID 79 fatal error").
		WithRecommendedAction(recommendedAction)
	SendHealthEvent(ctx, t, fatalEvent)

	t.Log("Step 2: Wait for node to be cordoned (quarantined)")
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}

		return node.Spec.Unschedulable
	}, EventuallyWaitTimeout, WaitInterval)
	t.Log("Node cordoned successfully")

	t.Log("Full remediation flow trigger completed")
}
