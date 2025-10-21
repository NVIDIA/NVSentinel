// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

type QuarantineCELTestContextKey int

const (
	CELKeyNodeName QuarantineCELTestContextKey = iota
	CELKeyConfigMapBackupPath
)

type QuarantineTestContext struct {
	NodeName            string
	ConfigMapBackupPath string
}

type QuarantineAssertion struct {
	ExpectTaint      *v1.Taint
	ExpectCordoned   bool
	ExpectAnnotation bool
}

func SetupQuarantineTest(ctx context.Context, t *testing.T, c *envconf.Config, configMapPath string) (context.Context, *QuarantineTestContext) {
	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &QuarantineTestContext{}

	t.Log("Backing up current fault-quarantine configmap")
	backupPath, err := BackupConfigMap(ctx, client, "fault-quarantine-config", "nvsentinel")
	require.NoError(t, err)
	t.Logf("Backup created at: %s", backupPath)
	testCtx.ConfigMapBackupPath = backupPath

	t.Logf("Applying test configmap: %s", configMapPath)
	err = CreateConfigMapFromFilePath(ctx, client, configMapPath, "fault-quarantine-config", "nvsentinel")
	require.NoError(t, err)

	t.Log("Restarting fault-quarantine deployment")
	err = RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for deployment to be ready")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready")

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
		t.Logf("Selected uncordoned node: %s (from index %d)", nodeName, startIdx)
	}

	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, CELKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, CELKeyConfigMapBackupPath, testCtx.ConfigMapBackupPath)

	return ctx, testCtx
}

func TeardownQuarantineTest(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	client, err := c.NewClient()
	require.NoError(t, err)

	nodeName := ctx.Value(CELKeyNodeName).(string)

	t.Logf("Cleaning up node %s", nodeName)

	t.Log("Sending healthy event to clear any quarantine")
	SendHealthyEvent(ctx, t, nodeName)

	t.Log("Waiting for FQ to process healthy event and clean node")
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}

		if node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				t.Log("Waiting for FQ to clear quarantine annotation")
				return false
			}
		}

		if node.Spec.Unschedulable {
			t.Log("FQ cleared annotation but didn't uncordon, manually uncordoning")
			node.Spec.Unschedulable = false
			client.Resources().Update(ctx, node)
			return false
		}

		return true
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s cleaned successfully", nodeName)

	backupPath := ctx.Value(CELKeyConfigMapBackupPath).(string)
	t.Logf("Restoring configmap from: %s", backupPath)
	err = CreateConfigMapFromFilePath(ctx, client, backupPath, "fault-quarantine-config", "nvsentinel")
	assert.NoError(t, err)

	os.Remove(backupPath)

	t.Log("Restarting fault-quarantine deployment")
	err = RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	assert.NoError(t, err)

	t.Log("Waiting for deployment to be ready after restore")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready after restore")

	return ctx
}

func SendHealthEvent(ctx context.Context, t *testing.T, event *HealthEventTemplate) string {
	t.Logf("Sending health event to node %s: checkName=%s, isFatal=%v",
		event.NodeName, event.CheckName, event.IsFatal)
	tempFile, err := SendHealthEventWithTemplate(event.NodeName, event)
	require.NoError(t, err)
	t.Logf("Health event sent successfully")
	return tempFile
}

func SendHealthyEvent(ctx context.Context, t *testing.T, nodeName string) {
	t.Logf("Sending generic healthy event to node %s", nodeName)
	event := NewHealthEvent(nodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU").
		WithErrorCode("")

	event.ErrorCode = nil

	tempFile := SendHealthEvent(ctx, t, event)
	defer os.Remove(tempFile)
}

func SendHealthyEventAndWaitForCleanup(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	t.Logf("Sending healthy event and waiting for cleanup on node %s", nodeName)
	SendHealthyEvent(ctx, t, nodeName)

	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			t.Logf("Failed to get node %s: %v", nodeName, err)
			return false
		}
		if node.Spec.Unschedulable {
			t.Logf("Node %s still cordoned", nodeName)
			return false
		}

		hasTaints := len(node.Spec.Taints) > 1
		if hasTaints {
			t.Logf("Node %s still has taints: %+v", nodeName, node.Spec.Taints)
		}

		if node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				t.Logf("Node %s still has quarantine annotation", nodeName)
				return false
			}
		}
		return true
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s cleaned up successfully", nodeName)
}

func SendHealthyEventsAsync(ctx context.Context, t *testing.T, client klient.Client, nodeNames []string) {
	t.Logf("Sending healthy events to %d nodes asynchronously", len(nodeNames))

	for _, nodeName := range nodeNames {
		SendHealthyEvent(ctx, t, nodeName)
	}

	t.Log("Waiting for all nodes to be cleaned up")
	require.Eventually(t, func() bool {
		cleanedCount := 0
		for _, nodeName := range nodeNames {
			node, err := GetNodeByName(ctx, client, nodeName)
			if err != nil {
				continue
			}
			if node.Spec.Unschedulable {
				continue
			}
			if node.Annotations != nil {
				if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
					continue
				}
			}
			cleanedCount++
		}

		if cleanedCount%5 == 0 || cleanedCount == len(nodeNames) {
			t.Logf("Nodes cleaned: %d/%d", cleanedCount, len(nodeNames))
		}

		return cleanedCount == len(nodeNames)
	}, WaitTimeout, WaitInterval)
	t.Logf("All %d nodes cleaned up successfully", len(nodeNames))
}

func AssertQuarantineState(ctx context.Context, t *testing.T, client klient.Client, nodeName string, expected QuarantineAssertion) {
	t.Logf("Asserting quarantine state on node %s: expectCordoned=%v, expectTaint=%v, expectAnnotation=%v",
		nodeName, expected.ExpectCordoned, expected.ExpectTaint != nil, expected.ExpectAnnotation)

	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			t.Logf("failed to get node %s: %v", nodeName, err)
			return false
		}

		if expected.ExpectCordoned {
			if !node.Spec.Unschedulable {
				t.Logf("waiting for node %s to be cordoned", nodeName)
				return false
			}
		} else {
			if node.Spec.Unschedulable {
				t.Logf("node %s is cordoned but shouldn't be", nodeName)
				return false
			}
		}

		if expected.ExpectTaint != nil {
			found := false
			for _, taint := range node.Spec.Taints {
				if taint.Key == expected.ExpectTaint.Key &&
					taint.Value == expected.ExpectTaint.Value &&
					taint.Effect == expected.ExpectTaint.Effect {
					found = true
					break
				}
			}
			if !found {
				t.Logf("waiting for taint %s=%s:%s on node %s",
					expected.ExpectTaint.Key, expected.ExpectTaint.Value, expected.ExpectTaint.Effect, nodeName)
				return false
			}
		}

		if expected.ExpectAnnotation {
			if node.Annotations == nil {
				t.Logf("waiting for annotations on node %s", nodeName)
				return false
			}
			if _, exists := node.Annotations["quarantineHealthEvent"]; !exists {
				t.Logf("waiting for quarantineHealthEvent annotation on node %s", nodeName)
				return false
			}
		} else {
			if node.Annotations != nil {
				if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
					t.Logf("node %s has quarantineHealthEvent annotation but shouldn't", nodeName)
					return false
				}
			}
		}

		return true
	}, WaitTimeout, WaitInterval)

	t.Logf("Assertion passed for node %s", nodeName)
}

func AssertNoQuarantine(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	AssertQuarantineState(ctx, t, client, nodeName, QuarantineAssertion{
		ExpectCordoned:   false,
		ExpectAnnotation: false,
	})
}

func AssertAnnotationContains(ctx context.Context, t *testing.T, client klient.Client, nodeName string, substrs ...string) {
	node, err := GetNodeByName(ctx, client, nodeName)
	require.NoError(t, err)
	require.NotNil(t, node.Annotations)

	annotation, exists := node.Annotations["quarantineHealthEvent"]
	require.True(t, exists)

	for _, substr := range substrs {
		assert.Contains(t, annotation, substr)
	}
}

func SetCircuitBreakerState(ctx context.Context, t *testing.T, c *envconf.Config, state string) {
	t.Logf("Setting circuit breaker state to: %s", state)
	client, err := c.NewClient()
	require.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fault-quarantine-circuit-breaker",
			Namespace: "nvsentinel",
		},
		Data: map[string]string{
			"status": state,
		},
	}

	existingCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fault-quarantine-circuit-breaker",
			Namespace: "nvsentinel",
		},
	}
	_ = client.Resources().Delete(ctx, existingCM)

	err = client.Resources().Create(ctx, cm)
	require.NoError(t, err)

	t.Log("Restarting fault-quarantine deployment to pick up CB state")
	err = RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for deployment to be ready with new CB state")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready with new CB state")
}

func GetCircuitBreakerState(ctx context.Context, t *testing.T, c *envconf.Config) string {
	client, err := c.NewClient()
	require.NoError(t, err)

	cm := &v1.ConfigMap{}
	err = client.Resources().Get(ctx, "fault-quarantine-circuit-breaker", "nvsentinel", cm)
	if err != nil {
		return ""
	}

	if cm.Data == nil {
		return ""
	}

	return cm.Data["status"]
}

func SetCircuitBreakerThreshold(ctx context.Context, t *testing.T, client klient.Client, percentage int, duration string) *appsv1.Deployment {
	t.Logf("Setting circuit breaker threshold to %d%% with duration %s", percentage, duration)

	var originalDeployment *appsv1.Deployment
	maxRetries := 5

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}

		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		require.NoError(t, err)

		if attempt == 0 {
			originalDeployment = deployment.DeepCopy()
		}

		updated := false
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name == "fault-quarantine" {
				newArgs := []string{}
				for _, arg := range container.Args {
					if strings.HasPrefix(arg, "--circuit-breaker-percentage=") {
						newArgs = append(newArgs, fmt.Sprintf("--circuit-breaker-percentage=%d", percentage))
						updated = true
					} else if strings.HasPrefix(arg, "--circuit-breaker-duration=") {
						newArgs = append(newArgs, fmt.Sprintf("--circuit-breaker-duration=%s", duration))
						updated = true
					} else {
						newArgs = append(newArgs, arg)
					}
				}
				deployment.Spec.Template.Spec.Containers[i].Args = newArgs
				t.Logf("Updated container args: %v", newArgs)
				break
			}
		}

		if !updated {
			t.Log("Warning: CB args not found in deployment, may already be set or missing")
		}

		err = client.Resources().Update(ctx, deployment)
		if err != nil {
			if apierrors.IsConflict(err) && attempt < maxRetries-1 {
				continue
			}
			require.NoError(t, err)
		}
		break
	}

	t.Log("Restarting deployment with new CB threshold")
	err := RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for deployment to be ready with new CB threshold")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready with new CB threshold")

	return originalDeployment
}

func EnsureDryRunDisabled(ctx context.Context, t *testing.T, client klient.Client) {
	t.Log("Ensuring dry-run mode is disabled on fault-quarantine deployment")

	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}

		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		require.NoError(t, err)

		needsUpdate := false
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name == "fault-quarantine" {
				newArgs := []string{}
				for _, arg := range container.Args {
					if strings.HasPrefix(arg, "--dry-run=") {
						if arg != "--dry-run=false" {
							newArgs = append(newArgs, "--dry-run=false")
							needsUpdate = true
						} else {
							newArgs = append(newArgs, arg)
						}
					} else {
						newArgs = append(newArgs, arg)
					}
				}
				if needsUpdate {
					deployment.Spec.Template.Spec.Containers[i].Args = newArgs
					t.Logf("Updated dry-run args: %v", newArgs)
				}
				break
			}
		}

		if !needsUpdate {
			t.Log("Dry-run already disabled")
			return
		}

		err = client.Resources().Update(ctx, deployment)
		if err != nil {
			if apierrors.IsConflict(err) && attempt < maxRetries-1 {
				continue
			}
			require.NoError(t, err)
		}
		break
	}

	t.Log("Restarting deployment with dry-run disabled")
	err := RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for deployment to be ready with dry-run disabled")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready with dry-run disabled")
}

func EnableDryRunMode(ctx context.Context, t *testing.T, client klient.Client) *appsv1.Deployment {
	t.Log("Enabling dry-run mode on fault-quarantine deployment")

	var originalDeployment *appsv1.Deployment
	maxRetries := 5

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}

		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		require.NoError(t, err)

		if attempt == 0 {
			originalDeployment = deployment.DeepCopy()
		}

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name == "fault-quarantine" {
				newArgs := []string{}
				for _, arg := range container.Args {
					if strings.HasPrefix(arg, "--dry-run=") {
						newArgs = append(newArgs, "--dry-run=true")
					} else {
						newArgs = append(newArgs, arg)
					}
				}
				deployment.Spec.Template.Spec.Containers[i].Args = newArgs
				break
			}
		}

		err = client.Resources().Update(ctx, deployment)
		if err != nil {
			if apierrors.IsConflict(err) && attempt < maxRetries-1 {
				continue
			}
			require.NoError(t, err)
		}
		break
	}

	t.Log("Restarting deployment with dry-run enabled")
	err := RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	require.NoError(t, err)

	t.Log("Waiting for deployment to be ready with dry-run mode")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready with dry-run mode")

	return originalDeployment
}

func RestoreFQDeployment(ctx context.Context, t *testing.T, client klient.Client, original *appsv1.Deployment) {
	t.Log("Restoring original fault-quarantine deployment")

	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}

		current := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, original.Name, original.Namespace, current)
		if err != nil {
			return
		}

		current.Spec = original.Spec
		err = client.Resources().Update(ctx, current)
		if err != nil {
			if apierrors.IsConflict(err) && attempt < maxRetries-1 {
				continue
			}
			assert.NoError(t, err)
		}
		break
	}

	t.Log("Restarting deployment with restored config")
	err := RestartDeployment(ctx, client, "nvsentinel-fault-quarantine", "nvsentinel")
	assert.NoError(t, err)

	t.Log("Waiting for deployment to be ready with restored config")
	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", "nvsentinel", deployment)
		if err != nil {
			return false
		}
		return deployment.Status.ReadyReplicas > 0 &&
			deployment.Status.UpdatedReplicas == deployment.Status.Replicas
	}, WaitTimeout, WaitInterval)
	t.Log("Deployment ready with restored config")
}
