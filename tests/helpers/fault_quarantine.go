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
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
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
	ctx, testCtx, _ := SetupQuarantineTestWithOptions(ctx, t, c, configMapPath, nil)
	return ctx, testCtx
}

// QuarantineSetupOptions provides options for setting up quarantine tests.
type QuarantineSetupOptions struct {
	// CircuitBreakerPercentage sets the CB percentage threshold (0 to skip)
	CircuitBreakerPercentage int
	// CircuitBreakerDuration sets the CB duration (empty to skip)
	CircuitBreakerDuration string
	// CircuitBreakerState sets the initial CB state (empty to skip)
	CircuitBreakerState string
	// DryRun sets dry-run mode (nil to skip, otherwise pointer to bool value)
	DryRun *bool
	// SkipRestart skips the deployment restart (useful when chaining operations)
	SkipRestart bool
}

// SetupQuarantineTestWithOptions sets up a quarantine test with additional configuration options.
// This allows combining multiple deployment modifications into a single rollout.
// Returns (context, testContext, originalDeployment) - originalDeployment is nil if no deployment changes were made.
func SetupQuarantineTestWithOptions(ctx context.Context, t *testing.T, c *envconf.Config,
	configMapPath string, opts *QuarantineSetupOptions) (context.Context, *QuarantineTestContext, *appsv1.Deployment) {

	client, err := c.NewClient()
	require.NoError(t, err)

	testCtx := &QuarantineTestContext{}
	var originalDeployment *appsv1.Deployment

	t.Log("Backing up current fault-quarantine configmap")
	backupPath, err := BackupConfigMap(ctx, client, "fault-quarantine-config", NVSentinelNamespace)
	require.NoError(t, err)
	t.Logf("Backup created at: %s", backupPath)
	testCtx.ConfigMapBackupPath = backupPath

	t.Logf("Applying test configmap: %s", configMapPath)
	err = createConfigMapFromFilePath(ctx, client, configMapPath, "fault-quarantine-config", NVSentinelNamespace)
	require.NoError(t, err)

	argUpdates := make(map[string]string)
	if opts != nil {
		if opts.CircuitBreakerPercentage > 0 {
			t.Logf("Will set circuit breaker threshold: %d%%, duration: %s",
				opts.CircuitBreakerPercentage, opts.CircuitBreakerDuration)
			argUpdates["--circuit-breaker-percentage="] = fmt.Sprintf("--circuit-breaker-percentage=%d", opts.CircuitBreakerPercentage)
			argUpdates["--circuit-breaker-duration="] = fmt.Sprintf("--circuit-breaker-duration=%s", opts.CircuitBreakerDuration)
		}
		if opts.DryRun != nil {
			t.Logf("Will set dry-run mode to: %v", *opts.DryRun)
			argUpdates["--dry-run="] = fmt.Sprintf("--dry-run=%v", *opts.DryRun)
		}
	}

	if len(argUpdates) > 0 {
		originalDeployment = modifyFaultQuarantineDeploymentArgs(ctx, t, client, argUpdates)
	}

	if opts != nil && opts.CircuitBreakerState != "" {
		updateCircuitBreakerStateConfigMap(ctx, t, client, opts.CircuitBreakerState)
	}

	if opts == nil || !opts.SkipRestart {
		t.Log("Restarting fault-quarantine deployment to load all configuration changes")
		err = RestartDeployment(ctx, t, client, "nvsentinel-fault-quarantine", NVSentinelNamespace)
		require.NoError(t, err)
	}

	nodeName := SelectTestNodeFromUnusedPool(ctx, t, client)
	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, CELKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, CELKeyConfigMapBackupPath, testCtx.ConfigMapBackupPath)

	return ctx, testCtx, originalDeployment
}

func TeardownQuarantineTest(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	client, err := c.NewClient()
	require.NoError(t, err)

	nodeNameVal := ctx.Value(CELKeyNodeName)
	if nodeNameVal == nil {
		t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
		return ctx
	}
	nodeName := nodeNameVal.(string)

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

	backupPathVal := ctx.Value(CELKeyConfigMapBackupPath)
	if backupPathVal != nil {
		backupPath := backupPathVal.(string)
		t.Logf("Restoring configmap from: %s", backupPath)
		err = createConfigMapFromFilePath(ctx, client, backupPath, "fault-quarantine-config", NVSentinelNamespace)
		assert.NoError(t, err)

		os.Remove(backupPath)
	}

	t.Log("Restarting fault-quarantine deployment to load restored configuration")
	err = RestartDeployment(ctx, t, client, "nvsentinel-fault-quarantine", NVSentinelNamespace)
	assert.NoError(t, err)

	return ctx
}

// SendHealthyEventAndWaitForCleanup sends a healthy event and waits for quarantine-specific cleanup
// (uncordoned, taints removed, quarantine annotations cleared, nvsentinel-state label cleared).
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

		// Also wait for nvsentinel-state label to be cleared
		if node.Labels != nil {
			if _, exists := node.Labels[NVSentinelStateLabelKey]; exists {
				t.Logf("Node %s still has nvsentinel-state label", nodeName)
				return false
			}
		}

		return true
	}, WaitTimeout, WaitInterval)
	t.Logf("Node %s cleaned up successfully", nodeName)
}

// SendHealthyEventsAsync sends healthy events to multiple nodes and waits for quarantine cleanup on all of them.
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

	// Use helper to update CB state configmap
	updateCircuitBreakerStateConfigMap(ctx, t, client, state)

	// Restart deployment to pick up the new CB state
	t.Log("Restarting fault-quarantine deployment to pick up CB state")
	err = RestartDeployment(ctx, t, client, "nvsentinel-fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)
}

func GetCircuitBreakerState(ctx context.Context, t *testing.T, c *envconf.Config) string {
	client, err := c.NewClient()
	require.NoError(t, err)

	cm := &v1.ConfigMap{}
	err = client.Resources().Get(ctx, "fault-quarantine-circuit-breaker", NVSentinelNamespace, cm)
	if err != nil {
		return ""
	}

	if cm.Data == nil {
		return ""
	}

	return cm.Data["status"]
}

// RestoreFQDeployment restores the fault-quarantine deployment to its original configuration.
func RestoreFQDeployment(ctx context.Context, t *testing.T, client klient.Client, original *appsv1.Deployment) {
	t.Log("Restoring original fault-quarantine deployment")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, original.Name, original.Namespace, current); err != nil {
			return err
		}

		current.Spec = original.Spec
		return client.Resources().Update(ctx, current)
	})
	assert.NoError(t, err, "failed to restore deployment")

	t.Log("Waiting for rollout to complete with restored config")
	WaitForDeploymentRollout(ctx, t, client, "nvsentinel-fault-quarantine", NVSentinelNamespace)
}

// modifyFaultQuarantineDeploymentArgs is a generic helper to modify fault-quarantine deployment args.
// argUpdates is a map of arg prefix -> new value (e.g., "--dry-run=" -> "--dry-run=true")
// Returns the original deployment before modifications.
func modifyFaultQuarantineDeploymentArgs(ctx context.Context, t *testing.T, client klient.Client,
	argUpdates map[string]string) *appsv1.Deployment {

	var originalDeployment *appsv1.Deployment

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, "nvsentinel-fault-quarantine", NVSentinelNamespace, deployment); err != nil {
			return err
		}

		// Capture original deployment on first iteration
		if originalDeployment == nil {
			originalDeployment = deployment.DeepCopy()
		}

		// Find fault-quarantine container and update args
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name == "fault-quarantine" {
				newArgs := []string{}
				for _, arg := range container.Args {
					updated := false
					for prefix, newValue := range argUpdates {
						if strings.HasPrefix(arg, prefix) {
							newArgs = append(newArgs, newValue)
							updated = true
							break
						}
					}
					if !updated {
						newArgs = append(newArgs, arg)
					}
				}
				deployment.Spec.Template.Spec.Containers[i].Args = newArgs
				break
			}
		}

		return client.Resources().Update(ctx, deployment)
	})
	require.NoError(t, err, "failed to modify deployment args")

	return originalDeployment
}

// updateCircuitBreakerStateConfigMap updates the CB state configmap (without restarting deployment).
func updateCircuitBreakerStateConfigMap(ctx context.Context, t *testing.T, client klient.Client, state string) {
	t.Logf("Updating circuit breaker state configmap to: %s", state)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fault-quarantine-circuit-breaker",
			Namespace: NVSentinelNamespace,
		},
		Data: map[string]string{
			"status": state,
		},
	}

	// Delete existing CM (ignore errors)
	existingCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fault-quarantine-circuit-breaker",
			Namespace: NVSentinelNamespace,
		},
	}
	_ = client.Resources().Delete(ctx, existingCM)

	err := client.Resources().Create(ctx, cm)
	require.NoError(t, err, "failed to create CB state configmap")
}
