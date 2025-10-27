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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

type RemediationTestContextKey int

const (
	FRKeyNodeName RemediationTestContextKey = iota
	FRKeyNamespace
)

const (
	RemediatingLabelValue          = "remediating"
	RemediationSucceededLabelValue = "remediation-succeeded"
	RemediationFailedLabelValue    = "remediation-failed"
	JanitorNamespace               = "dgxc-janitor"
	RebootNodeCRDGroup             = "janitor.dgxc.nvidia.com"
	RebootNodeCRDVersion           = "v1alpha1"
	RebootNodeCRDPlural            = "rebootnodes"
)

type RemediationTestContext struct {
	NodeName      string
	JanitorNS     string
	TestNamespace string
}

func SetupFaultRemediationTest(ctx context.Context, t *testing.T, c *envconf.Config, testNamespace string) (context.Context, *RemediationTestContext) {
	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &RemediationTestContext{
		JanitorNS:     JanitorNamespace,
		TestNamespace: testNamespace,
	}

	t.Log("Ensuring janitor namespace exists")
	err = CreateNamespace(ctx, client, JanitorNamespace)
	require.NoError(t, err)

	t.Log("Ensuring rebootnode CRD exists")
	EnsureRebootNodeCRD(ctx, t, client)

	t.Log("Cleaning up existing rebootnode CRs")
	CleanupRebootNodeCRs(ctx, t, client)

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

func TeardownFaultRemediationTest(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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

	node, err := GetNodeByName(ctx, client, nodeName)
	if err == nil {
		if node.Spec.Unschedulable {
			t.Log("Manually uncordoning node")
			node.Spec.Unschedulable = false
			err = client.Resources().Update(ctx, node)
			require.NoError(t, err)
		}

		if node.Labels != nil {
			delete(node.Labels, NVSentinelStateLabelKey)
			err = client.Resources().Update(ctx, node)
			require.NoError(t, err)
		}

		if node.Annotations != nil {
			delete(node.Annotations, "latestFaultRemediationState")
			err = client.Resources().Update(ctx, node)
			require.NoError(t, err)
		}
	}

	if testNamespace != "" {
		t.Logf("Cleaning up test namespace: %s", testNamespace)
		DeleteNamespace(ctx, t, client, testNamespace)
	}

	t.Log("Cleaning up rebootnode CRs")
	CleanupRebootNodeCRs(ctx, t, client)

	return ctx
}

func EnsureRebootNodeCRD(ctx context.Context, t *testing.T, client klient.Client) {
	crdName := "rebootnodes.janitor.dgxc.nvidia.com"
	var crd unstructured.Unstructured
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})

	err := client.Resources().Get(ctx, crdName, "", &crd)
	if err == nil {
		t.Log("RebootNode CRD already exists")
		return
	}

	if !apierrors.IsNotFound(err) {
		t.Logf("Error checking CRD: %v", err)
		return
	}

	t.Log("RebootNode CRD not found, attempting to create")
	crdDef := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]any{
				"name": crdName,
			},
			"spec": map[string]any{
				"group": RebootNodeCRDGroup,
				"versions": []any{
					map[string]any{
						"name":    RebootNodeCRDVersion,
						"served":  true,
						"storage": true,
						"schema": map[string]any{
							"openAPIV3Schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"spec": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"nodeName": map[string]any{
												"type": "string",
											},
										},
									},
									"status": map[string]any{
										"type":                                 "object",
										"x-kubernetes-preserve-unknown-fields": true,
									},
								},
							},
						},
					},
				},
				"scope": "Cluster",
				"names": map[string]any{
					"plural":   RebootNodeCRDPlural,
					"singular": "rebootnode",
					"kind":     "RebootNode",
					"shortNames": []any{
						"rn",
					},
				},
			},
		},
	}

	err = client.Resources().Create(ctx, crdDef)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Logf("Warning: Failed to create RebootNode CRD: %v", err)
		return
	}

	time.Sleep(2 * time.Second)
	t.Log("Successfully ensured RebootNode CRD exists")
}

func CleanupRebootNodeCRs(ctx context.Context, t *testing.T, client klient.Client) {
	var crList unstructured.UnstructuredList
	crList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   RebootNodeCRDGroup,
		Version: RebootNodeCRDVersion,
		Kind:    "RebootNodeList",
	})

	err := client.Resources().List(ctx, &crList)
	if err != nil {
		t.Logf("Warning: Failed to list RebootNode CRs: %v", err)
		return
	}

	if len(crList.Items) == 0 {
		t.Log("No existing RebootNode CRs to clean up")
		return
	}

	t.Logf("Found %d RebootNode CRs, deleting...", len(crList.Items))
	for _, cr := range crList.Items {
		crName := cr.GetName()
		err := client.Resources().Delete(ctx, &cr)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: Failed to delete RebootNode CR %s: %v", crName, err)
		} else {
			t.Logf("Deleted RebootNode CR: %s", crName)
		}
	}

	time.Sleep(2 * time.Second)
}

// GetRebootNodeCRsForNode returns all RebootNode CR names for a specific node
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
			crList = append(crList, cr.GetName())
		}
	}

	return crList, nil
}

func WaitForNoRebootNodeCR(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	t.Logf("Waiting to verify no RebootNode CR exists for node %s", nodeName)
	time.Sleep(30 * time.Second)

	crList, err := GetRebootNodeCRsForNode(ctx, client, nodeName)
	if err != nil {
		t.Logf("Failed to list RebootNode CRs: %v", err)
		return
	}

	require.Empty(t, crList, "RebootNode CR should not exist for node %s", nodeName)
	t.Logf("Verified no RebootNode CR exists for node %s", nodeName)
}

func (h *HealthEventTemplate) WithRecommendedAction(action int) *HealthEventTemplate {
	h.RecommendedAction = action
	return h
}

func SendDrainCompletedEvent(ctx context.Context, t *testing.T, nodeName string, recommendedAction int) string {
	t.Logf("Sending drain completed event to node %s with recommended action %d", nodeName, recommendedAction)
	event := NewHealthEvent(nodeName).
		WithErrorCode("79").
		WithMessage("GPU Fallen off the bus - drain completed").
		WithRecommendedAction(recommendedAction)

	tempFile := SendHealthEvent(ctx, t, event)
	t.Log("Drain completed event sent successfully")
	return tempFile
}

func RestartFaultRemediationDeployment(ctx context.Context, t *testing.T, client klient.Client) {
	t.Log("Restarting fault-remediation deployment")
	err := RestartDeployment(ctx, client, "nvsentinel-fault-remediation", NVSentinelNamespace)
	require.NoError(t, err)

	t.Log("Waiting for fault-remediation deployment to be ready")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-remediation", NVSentinelNamespace, deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Fault-remediation deployment ready")
}

func GetNodeAnnotation(ctx context.Context, t *testing.T, client klient.Client, nodeName, annotationKey string) (string, bool) {
	node, err := GetNodeByName(ctx, client, nodeName)
	require.NoError(t, err)

	if node.Annotations == nil {
		return "", false
	}

	value, exists := node.Annotations[annotationKey]
	return value, exists
}

func WaitForNodeAnnotation(ctx context.Context, t *testing.T, client klient.Client, nodeName, annotationKey string) string {
	var annotationValue string

	t.Logf("Waiting for node %s to have annotation: %s", nodeName, annotationKey)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}
		if node.Annotations == nil {
			return false
		}
		value, exists := node.Annotations[annotationKey]
		if exists {
			annotationValue = value
			return true
		}
		return false
	}, WaitTimeout, WaitInterval)

	t.Logf("Node %s has annotation %s: %s", nodeName, annotationKey, annotationValue)
	return annotationValue
}

func WaitForNoNodeAnnotation(ctx context.Context, t *testing.T, client klient.Client, nodeName, annotationKey string) {
	t.Logf("Waiting for node %s to NOT have annotation: %s", nodeName, annotationKey)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return true
		}
		if node.Annotations == nil {
			return true
		}
		_, exists := node.Annotations[annotationKey]
		return !exists
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s does not have annotation %s", nodeName, annotationKey)
}

func TriggerFullRemediationFlow(ctx context.Context, t *testing.T, client klient.Client, nodeName string, recommendedAction int) {
	t.Logf("Triggering full remediation flow for node %s", nodeName)

	t.Log("Step 1: Send fatal health event to trigger quarantine")
	fatalEvent := NewHealthEvent(nodeName).
		WithErrorCode("79").
		WithMessage("XID 79 fatal error").
		WithRecommendedAction(recommendedAction)
	tempFile1 := SendHealthEvent(ctx, t, fatalEvent)
	defer os.Remove(tempFile1)

	t.Log("Step 2: Wait for node to be cordoned (quarantined)")
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, WaitTimeout, WaitInterval)
	t.Log("Node cordoned successfully")

	t.Log("Full remediation flow trigger completed")
}

func UpdateRebootNodeCRStatus(ctx context.Context, t *testing.T, client klient.Client, crName, status string) {
	var cr unstructured.Unstructured
	cr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   RebootNodeCRDGroup,
		Version: RebootNodeCRDVersion,
		Kind:    "RebootNode",
	})

	err := client.Resources().Get(ctx, crName, "", &cr)
	require.NoError(t, err)

	statusMap := map[string]any{
		"state": status,
	}
	err = unstructured.SetNestedMap(cr.Object, statusMap, "status")
	require.NoError(t, err)

	err = client.Resources().Update(ctx, &cr)
	require.NoError(t, err)
	t.Logf("Updated RebootNode CR %s status to: %s", crName, status)
}
