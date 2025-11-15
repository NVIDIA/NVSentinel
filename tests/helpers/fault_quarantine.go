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
	CELKeyConfigMapBackup
)

type faultQuarantineConfig struct {
	LabelPrefix    string               `toml:"label-prefix"`
	CircuitBreaker circuitBreakerConfig `toml:"circuitBreaker"`
	RuleSets       []map[string]any     `toml:"rule-sets"`
}

type circuitBreakerConfig struct {
	Percentage int    `toml:"percentage"`
	Duration   string `toml:"duration"`
}

type QuarantineTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
}

type QuarantineAssertion struct {
	ExpectTaint      *v1.Taint
	ExpectCordoned   bool
	ExpectAnnotation bool
}

func ApplyQuarantineConfig(ctx context.Context, t *testing.T, c *envconf.Config, configMapPath string) context.Context {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	t.Log("Backing up current fault-quarantine configmap")

	backupData, err := BackupConfigMap(ctx, client, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)

	ctx = context.WithValue(ctx, CELKeyConfigMapBackup, backupData)

	t.Logf("Applying test configmap: %s", configMapPath)
	err = createConfigMapFromFilePath(ctx, client, configMapPath, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)

	t.Log("Restarting fault-quarantine deployment to load configuration")
	err = RestartDeployment(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)

	return ctx
}

func RestoreQuarantineConfig(ctx context.Context, t *testing.T, c *envconf.Config) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	backupDataVal := ctx.Value(CELKeyConfigMapBackup)
	if backupDataVal != nil {
		backupData := backupDataVal.([]byte)

		t.Log("Restoring fault-quarantine configmap from memory")

		err = createConfigMapFromBytes(ctx, client, backupData, "fault-quarantine", NVSentinelNamespace)
		require.NoError(t, err)

		t.Log("Restarting fault-quarantine deployment to load restored configuration")
		err = RestartDeployment(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
		require.NoError(t, err)
	}
}

func SetupQuarantineTest(
	ctx context.Context, t *testing.T, c *envconf.Config, configMapPath string,
) (context.Context, *QuarantineTestContext) {
	ctx, testCtx, _ := SetupQuarantineTestWithOptions(ctx, t, c, configMapPath, nil)
	return ctx, testCtx
}

// QuarantineSetupOptions provides options for setting up quarantine tests.
type QuarantineSetupOptions struct {
	CircuitBreakerPercentage int
	CircuitBreakerDuration   string
	CircuitBreakerState      string
	DryRun                   *bool
	SkipRestart              bool
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

	backupData, err := BackupConfigMap(ctx, client, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)

	testCtx.ConfigMapBackup = backupData

	t.Logf("Applying test configmap: %s", configMapPath)
	err = createConfigMapFromFilePath(ctx, client, configMapPath, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)

	if opts != nil {
		if opts.CircuitBreakerPercentage > 0 {
			t.Logf("Updating circuit breaker in ConfigMap: %d%%, duration: %s",
				opts.CircuitBreakerPercentage, opts.CircuitBreakerDuration)
			err = updateCircuitBreakerConfigInConfigMap(ctx, t, client,
				opts.CircuitBreakerPercentage, opts.CircuitBreakerDuration)
			require.NoError(t, err)
		}

		if opts.DryRun != nil {
			t.Logf("Will set dry-run mode to: %v", *opts.DryRun)
			argUpdates := map[string]string{
				"--dry-run=": fmt.Sprintf("--dry-run=%v", *opts.DryRun),
			}
			originalDeployment = modifyFaultQuarantineDeploymentArgs(ctx, t, client, argUpdates)
		}

		if opts.CircuitBreakerState != "" {
			updateCircuitBreakerStateConfigMap(ctx, t, client, opts.CircuitBreakerState)
		}
	}

	if opts == nil || !opts.SkipRestart {
		t.Log("Restarting fault-quarantine deployment to load all configuration changes")
		err = RestartDeployment(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
		require.NoError(t, err)
	}

	nodeName := SelectTestNodeFromUnusedPool(ctx, t, client)
	testCtx.NodeName = nodeName
	ctx = context.WithValue(ctx, CELKeyNodeName, nodeName)
	ctx = context.WithValue(ctx, CELKeyConfigMapBackup, testCtx.ConfigMapBackup)

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
			if err := client.Resources().Update(ctx, node); err != nil {
				t.Logf("Failed to manually uncordon node: %v", err)
			}

			return false
		}

		return true
	}, EventuallyWaitTimeout, WaitInterval)
	t.Logf("Node %s cleaned successfully", nodeName)

	backupDataVal := ctx.Value(CELKeyConfigMapBackup)
	if backupDataVal != nil {
		backupData := backupDataVal.([]byte)

		t.Log("Restoring fault-quarantine configmap from memory")

		err = createConfigMapFromBytes(ctx, client, backupData, "fault-quarantine", NVSentinelNamespace)
		assert.NoError(t, err)
	}

	t.Log("Restarting fault-quarantine deployment to load restored configuration")
	err = RestartDeployment(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
	assert.NoError(t, err)

	return ctx
}

// AssertNodeNeverQuarantined asserts that a node is never quarantined within the default timeout period.
// Checks that node is not cordoned and optionally not annotated with quarantine annotation.
func AssertNodeNeverQuarantined(
	ctx context.Context, t *testing.T, client klient.Client, nodeName string, checkAnnotation bool,
) {
	t.Helper()
	t.Logf("Asserting node %s is never quarantined", nodeName)
	require.Never(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			t.Logf("Error getting node: %v", err)
			return false
		}

		if node.Spec.Unschedulable {
			t.Logf("Node %s was cordoned (should not be quarantined)!", nodeName)
			return true
		}

		if checkAnnotation && node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				t.Logf("Node %s has quarantine annotation (should not be quarantined)!", nodeName)
				return true
			}
		}

		return false
	}, NeverWaitTimeout, WaitInterval, "node %s should not be quarantined", nodeName)
}

// SendHealthyEventsAsync sends healthy events to multiple nodes and waits for quarantine cleanup on all of them.
func SendHealthyEventsAsync(ctx context.Context, t *testing.T, client klient.Client, nodeNames []string) {
	t.Helper()
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
	}, EventuallyWaitTimeout, WaitInterval)
	t.Logf("All %d nodes cleaned up successfully", len(nodeNames))
}

func AssertQuarantineState( //nolint:cyclop,gocognit // Test helper with complex validation logic
	ctx context.Context, t *testing.T, client klient.Client, nodeName string, expected QuarantineAssertion,
) {
	t.Helper()
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
		} else if node.Annotations != nil {
			if _, exists := node.Annotations["quarantineHealthEvent"]; exists {
				t.Logf("node %s has quarantineHealthEvent annotation but shouldn't", nodeName)
				return false
			}
		}

		return true
	}, EventuallyWaitTimeout, WaitInterval)

	t.Logf("Assertion passed for node %s", nodeName)
}

func SetCircuitBreakerState(ctx context.Context, t *testing.T, c *envconf.Config, state string) {
	t.Helper()
	t.Logf("Setting circuit breaker state to: %s", state)

	client, err := c.NewClient()
	require.NoError(t, err)

	updateCircuitBreakerStateConfigMap(ctx, t, client, state)

	t.Log("Restarting fault-quarantine deployment to pick up CB state")
	err = RestartDeployment(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
	require.NoError(t, err)
}

func GetCircuitBreakerState(ctx context.Context, t *testing.T, c *envconf.Config) string {
	t.Helper()
	t.Logf("Getting circuit breaker state from configmap")

	client, err := c.NewClient()
	require.NoError(t, err)

	cm := &v1.ConfigMap{}

	err = client.Resources().Get(ctx, "circuit-breaker", NVSentinelNamespace, cm)
	if err != nil {
		t.Logf("failed to get circuit breaker state from configmap: %v", err)
		return ""
	}

	if cm.Data == nil {
		t.Logf("circuit breaker state configmap is empty")
		return ""
	}

	t.Logf("circuit breaker state: %s", cm.Data["status"])

	return cm.Data["status"]
}

// RestoreFQDeployment restores the fault-quarantine deployment to its original configuration.
func RestoreFQDeployment(ctx context.Context, t *testing.T, client klient.Client, original *appsv1.Deployment) {
	t.Helper()
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
	WaitForDeploymentRollout(ctx, t, client, "fault-quarantine", NVSentinelNamespace)
}

// modifyFaultQuarantineDeploymentArgs is a generic helper to modify fault-quarantine deployment args.
// argUpdates is a map of arg prefix -> new value (e.g., "--dry-run=" -> "--dry-run=true")
// Returns the original deployment before modifications.
func modifyFaultQuarantineDeploymentArgs(ctx context.Context, t *testing.T, client klient.Client,
	argUpdates map[string]string) *appsv1.Deployment {
	t.Helper()

	var originalDeployment *appsv1.Deployment

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, "fault-quarantine", NVSentinelNamespace, deployment); err != nil {
			return err
		}

		if originalDeployment == nil {
			originalDeployment = deployment.DeepCopy()
		}

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
	t.Helper()
	t.Logf("Updating circuit breaker state configmap to: %s", state)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "circuit-breaker",
			Namespace: NVSentinelNamespace,
		},
		Data: map[string]string{
			"status": state,
		},
	}

	existingCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "circuit-breaker",
			Namespace: NVSentinelNamespace,
		},
	}
	_ = client.Resources().Delete(ctx, existingCM)

	err := client.Resources().Create(ctx, cm)
	require.NoError(t, err, "failed to create CB state configmap")
}

// updateCircuitBreakerConfigInConfigMap updates the circuit breaker percentage and duration
// in the fault-quarantine ConfigMap.
func updateCircuitBreakerConfigInConfigMap(
	ctx context.Context, t *testing.T, client klient.Client, percentage int, duration string,
) error {
	t.Helper()

	err := UpdateConfigMapTOMLField(ctx, client, "fault-quarantine", NVSentinelNamespace, "config.toml",
		func(cfg *faultQuarantineConfig) error {
			cfg.CircuitBreaker.Percentage = percentage
			cfg.CircuitBreaker.Duration = duration

			return nil
		})
	if err != nil {
		return fmt.Errorf("failed to update circuit breaker config: %w", err)
	}

	t.Logf("Updated circuit breaker config in ConfigMap: percentage=%d, duration=%s", percentage, duration)

	return nil
}
