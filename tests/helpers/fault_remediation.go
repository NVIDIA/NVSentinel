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

	t.Log("Selecting test node from unused node pool")
	nodes, err := GetAllNodesNames(ctx, client)
	require.NoError(t, err)
	require.NotEmpty(t, nodes)

	startIdx := int(float64(len(nodes)) * 0.50)
	if startIdx >= len(nodes) {
		startIdx = len(nodes) - 1
	}
	unusedNodes := nodes[startIdx:]

	var nodeName string
	for _, name := range unusedNodes {
		node, err := GetNodeByName(ctx, client, name)
		if err != nil {
			continue
		}
		if !node.Spec.Unschedulable {
			nodeName = name
			break
		}
	}
	if nodeName == "" {
		nodeName = unusedNodes[0]
		t.Logf("No uncordoned node found, using: %s", nodeName)
	} else {
		t.Logf("Selected uncordoned node: %s", nodeName)
	}

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

	nodeName := ctx.Value(FRKeyNodeName).(string)
	testNamespace := ctx.Value(FRKeyNamespace).(string)

	t.Logf("Cleaning up node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	node, err := GetNodeByName(ctx, client, nodeName)
	if err == nil {
		if node.Spec.Unschedulable {
			t.Log("Manually uncordoning node")
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
		}

		if node.Labels != nil {
			delete(node.Labels, NVSentinelStateLabelKey)
			client.Resources().Update(ctx, node)
		}

		if node.Annotations != nil {
			delete(node.Annotations, "latestFaultRemediationState")
			client.Resources().Update(ctx, node)
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
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]interface{}{
				"name": crdName,
			},
			"spec": map[string]interface{}{
				"group": RebootNodeCRDGroup,
				"versions": []interface{}{
					map[string]interface{}{
						"name":    RebootNodeCRDVersion,
						"served":  true,
						"storage": true,
						"schema": map[string]interface{}{
							"openAPIV3Schema": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"spec": map[string]interface{}{
										"type": "object",
										"properties": map[string]interface{}{
											"nodeName": map[string]interface{}{
												"type": "string",
											},
										},
									},
									"status": map[string]interface{}{
										"type":                                 "object",
										"x-kubernetes-preserve-unknown-fields": true,
									},
								},
							},
						},
					},
				},
				"scope": "Cluster",
				"names": map[string]interface{}{
					"plural":   RebootNodeCRDPlural,
					"singular": "rebootnode",
					"kind":     "RebootNode",
					"shortNames": []interface{}{
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

func WaitForNoRebootNodeCR(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	t.Logf("Waiting to verify no RebootNode CR exists for node %s", nodeName)
	time.Sleep(30 * time.Second)

	var crList unstructured.UnstructuredList
	crList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   RebootNodeCRDGroup,
		Version: RebootNodeCRDVersion,
		Kind:    "RebootNodeList",
	})

	err := client.Resources().List(ctx, &crList)
	if err != nil {
		t.Logf("Failed to list RebootNode CRs: %v", err)
		return
	}

	for _, cr := range crList.Items {
		spec, found, err := unstructured.NestedMap(cr.Object, "spec")
		if !found || err != nil {
			continue
		}

		crNodeName, found, err := unstructured.NestedString(spec, "nodeName")
		if !found || err != nil {
			continue
		}

		require.NotEqual(t, nodeName, crNodeName, "RebootNode CR should not exist for node %s", nodeName)
	}

	t.Logf("Verified no RebootNode CR exists for node %s", nodeName)
}

func WaitForNodeRemediationLabel(ctx context.Context, t *testing.T, client klient.Client, nodeName, expectedValue string) {
	t.Logf("Waiting for node %s to have remediation label: %s=%s", nodeName, NVSentinelStateLabelKey, expectedValue)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}
		if node.Labels == nil {
			return false
		}
		value, exists := node.Labels[NVSentinelStateLabelKey]
		if !exists {
			return false
		}
		return value == expectedValue
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s has remediation label %s=%s", nodeName, NVSentinelStateLabelKey, expectedValue)
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
	err := RestartDeployment(ctx, client, "nvsentinel-fault-remediation", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for fault-remediation deployment to be ready")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-remediation", "nvsentinel", deployment)
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

	t.Log("Step 3: Verify MongoDB drain status (persists even after label changes)")
	WaitForMongoHealthEventStatus(ctx, t, client, nodeName, "Quarantined", "Succeeded")

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

	statusMap := map[string]interface{}{
		"state": status,
	}
	err = unstructured.SetNestedMap(cr.Object, statusMap, "status")
	require.NoError(t, err)

	err = client.Resources().Update(ctx, &cr)
	require.NoError(t, err)
	t.Logf("Updated RebootNode CR %s status to: %s", crName, status)
}

func GetLatestHealthEvent(ctx context.Context, t *testing.T, client klient.Client, nodeName string) map[string]interface{} {
	mongoEvent, err := QueryMongoHealthEvent(ctx, t, client, nodeName)
	if err != nil {
		t.Logf("Failed to query MongoDB health event: %v", err)
		return nil
	}

	result := make(map[string]interface{})

	statusMap := make(map[string]interface{})
	if mongoEvent.HealthEventStatus.NodeQuarantined != "" {
		statusMap["nodequarantined"] = mongoEvent.HealthEventStatus.NodeQuarantined
	}
	if mongoEvent.HealthEventStatus.UserPodsEvictionStatus.Status != "" {
		statusMap["nodedrained"] = mongoEvent.HealthEventStatus.UserPodsEvictionStatus.Status
	}
	if mongoEvent.HealthEventStatus.FaultRemediated != nil {
		statusMap["faultremediated"] = *mongoEvent.HealthEventStatus.FaultRemediated
	}
	if mongoEvent.HealthEventStatus.LastRemediationTimestamp != nil &&
		mongoEvent.HealthEventStatus.LastRemediationTimestamp.Date != "" {
		statusMap["lastremediationtimestamp"] = mongoEvent.HealthEventStatus.LastRemediationTimestamp.Date
	}

	result["healtheventstatus"] = statusMap
	return result
}

func WaitForMongoRemediationStatus(ctx context.Context, t *testing.T, client klient.Client, nodeName string, expectedRemediated bool) {
	t.Logf("Waiting for MongoDB faultRemediated status to be %v for node %s", expectedRemediated, nodeName)
	require.Eventually(t, func() bool {
		event := GetLatestHealthEvent(ctx, t, client, nodeName)
		if event == nil {
			return false
		}

		status, ok := event["healtheventstatus"].(map[string]interface{})
		if !ok {
			return false
		}

		remediated, ok := status["faultremediated"].(bool)
		if !ok {
			return false
		}

		return remediated == expectedRemediated
	}, WaitTimeout, WaitInterval)
	t.Logf("MongoDB faultRemediated status is %v for node %s", expectedRemediated, nodeName)
}
