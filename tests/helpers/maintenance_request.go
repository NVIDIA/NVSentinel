// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

// MaintenanceRequestGVK identifies the cluster-scoped CR the lifecycle-manager
// MaintenanceRequest controller reconciles.
var MaintenanceRequestGVK = schema.GroupVersionKind{
	Group:   "nvsentinel.dgxc.nvidia.com",
	Version: "v1",
	Kind:    "MaintenanceRequest",
}

const (
	// MaintenanceRequestFinalizer holds a deleted MaintenanceRequest alive until
	// the controller has emitted the clearing event. Its removal is the only
	// externally visible proof that the clearing publish succeeded.
	MaintenanceRequestFinalizer = "nvsentinel.dgxc.nvidia.com/maintenance-request-cleanup"

	// MaintenanceRequestEmittedCondition goes True once the controller has
	// published the opening health event to platform-connector.
	MaintenanceRequestEmittedCondition = "HealthEventEmitted"

	// LifecycleManagerLabelSelector matches the lifecycle-manager pod.
	LifecycleManagerLabelSelector = "app.kubernetes.io/name=lifecycle-manager"

	maintenanceRequestPollInterval = 1 * time.Second
	maintenanceRequestCleanupWait  = 30 * time.Second
)

// CreateMaintenanceRequest creates a cluster-scoped MaintenanceRequest naming
// nodeName.
//
// The field values are the ones the validating webhook insists on: a non-zero
// version, a node that exists, and isHealthy false, since an MR describes work
// that is about to start rather than a recovery. recommendedAction is NONE so
// the event stops at quarantine instead of pulling a reboot template into an
// E2E run. startTime is left unset, which the webhook reads as "now"; any value
// it accepts must be in the future, and a future timestamp would race the test.
func CreateMaintenanceRequest(
	ctx context.Context, c klient.Client, crName, nodeName, checkName string,
) (*unstructured.Unstructured, error) {
	mr := &unstructured.Unstructured{}
	mr.SetGroupVersionKind(MaintenanceRequestGVK)
	mr.SetName(crName)

	healthEvent := map[string]any{
		"version":           int64(1),
		"agent":             "lifecycle-manager",
		"componentClass":    "Node",
		"checkName":         checkName,
		"nodeName":          nodeName,
		"isHealthy":         false,
		"isFatal":           false,
		"recommendedAction": "NONE",
		fieldMessageKey:     fmt.Sprintf("e2e maintenance request for node %s", nodeName),
	}

	if err := unstructured.SetNestedMap(mr.Object, healthEvent, "spec", "healthEvent"); err != nil {
		return nil, fmt.Errorf("failed to set spec.healthEvent: %w", err)
	}

	if err := c.Resources().Create(ctx, mr); err != nil {
		return nil, fmt.Errorf("failed to create MaintenanceRequest %s: %w", crName, err)
	}

	return mr, nil
}

// WaitForMaintenanceRequestEmitted waits for the controller to report that it
// published the opening health event, and returns the MR as it then stood.
func WaitForMaintenanceRequestEmitted(
	ctx context.Context, t *testing.T, c klient.Client, crName string,
) *unstructured.Unstructured {
	t.Helper()

	mr := &unstructured.Unstructured{}
	mr.SetGroupVersionKind(MaintenanceRequestGVK)

	require.Eventually(t, func() bool {
		if err := c.Resources().Get(ctx, crName, "", mr); err != nil {
			t.Logf("failed to get MaintenanceRequest %s: %v", crName, err)
			return false
		}

		cond := GetCRCondition(mr, MaintenanceRequestEmittedCondition)
		if cond == nil {
			t.Logf("MaintenanceRequest %s has no %s condition yet",
				crName, MaintenanceRequestEmittedCondition)

			return false
		}

		t.Logf("MaintenanceRequest %s condition %s: status=%v reason=%v message=%v",
			crName, MaintenanceRequestEmittedCondition,
			cond["status"], cond["reason"], cond["message"])

		return cond["status"] == "True"
	}, EventuallyWaitTimeout, maintenanceRequestPollInterval,
		"MaintenanceRequest %s should report %s=True", crName, MaintenanceRequestEmittedCondition)

	return mr
}

// NodesRunningLifecycleManager returns the nodes that host lifecycle-manager
// pods. Excluding all of them ensures that the test target differs from the
// active leader's node, including during a rollout or with multiple replicas.
func NodesRunningLifecycleManager(ctx context.Context, c klient.Client) ([]string, error) {
	var pods v1.PodList

	err := c.Resources(NVSentinelNamespace).List(ctx, &pods,
		resources.WithLabelSelector(LifecycleManagerLabelSelector))
	if err != nil {
		return nil, fmt.Errorf("failed to list lifecycle-manager pods: %w", err)
	}

	nodes := make(map[string]struct{})

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != "" && pod.DeletionTimestamp == nil {
			nodes[pod.Spec.NodeName] = struct{}{}
		}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no scheduled lifecycle-manager pod found in namespace %s", NVSentinelNamespace)
	}

	nodeNames := make([]string, 0, len(nodes))
	for nodeName := range nodes {
		nodeNames = append(nodeNames, nodeName)
	}

	return nodeNames, nil
}

// SelectMaintenanceTargetNode returns a clean, uncordoned worker node that is
// not hosting a lifecycle-manager pod.
//
// Excluding every lifecycle-manager node is the whole point:
// platform-connector pins a caller to its own node unless that caller is on the
// cross-node allowlist and presents its projected token. An MR naming the
// leader's own node would pass even with the token wiring removed.
func SelectMaintenanceTargetNode(
	ctx context.Context, t *testing.T, c klient.Client, lifecycleManagerNodes []string,
) string {
	t.Helper()

	names, err := AllRealNodeNames(ctx, c)
	require.NoError(t, err, "failed to list real worker nodes")

	excludedNodes := make(map[string]struct{}, len(lifecycleManagerNodes))
	for _, nodeName := range lifecycleManagerNodes {
		excludedNodes[nodeName] = struct{}{}
	}

	var skipped []string

	for _, name := range names {
		if _, excluded := excludedNodes[name]; excluded {
			skipped = append(skipped, fmt.Sprintf("%s (hosts lifecycle-manager)", name))
			continue
		}

		node, err := GetNodeByName(ctx, c, name)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (unreadable: %v)", name, err))
			continue
		}

		if node.Spec.Unschedulable {
			skipped = append(skipped, fmt.Sprintf("%s (cordoned)", name))
			continue
		}

		if hasQuarantineResidue(node) {
			skipped = append(skipped, fmt.Sprintf("%s (leftover quarantine state)", name))
			continue
		}

		t.Logf("Selected maintenance target %s; lifecycle-manager runs on %v, so this is a cross-node publish",
			name, lifecycleManagerNodes)

		return name
	}

	require.FailNow(t,
		"no usable maintenance target node",
		"need a clean uncordoned worker outside lifecycle-manager nodes %v; skipped: %v",
		lifecycleManagerNodes, skipped)

	return ""
}

// DeleteMaintenanceRequestIfPresent removes the named MR and waits for it to
// leave the API, which only happens once the controller has released its
// finalizer.
func DeleteMaintenanceRequestIfPresent(ctx context.Context, t *testing.T, c klient.Client, crName string) {
	t.Helper()

	mr := &unstructured.Unstructured{}
	mr.SetGroupVersionKind(MaintenanceRequestGVK)
	mr.SetName(crName)

	err := DeleteCR(ctx, t, c, mr, true)
	require.NoError(t, err, "failed to delete MaintenanceRequest %s", crName)
}

// CleanupMaintenanceRequest deletes an MR without stopping the rest of test
// teardown. If normal finalization stalls, it removes the finalizers so node
// and ConfigMap cleanup can still run.
func CleanupMaintenanceRequest(ctx context.Context, t *testing.T, c klient.Client, crName string) {
	t.Helper()

	mr := &unstructured.Unstructured{}
	mr.SetGroupVersionKind(MaintenanceRequestGVK)
	mr.SetName(crName)

	err := c.Resources().Delete(ctx, mr)
	if apierrors.IsNotFound(err) {
		return
	}

	if !assert.NoError(t, err, "failed to request deletion of MaintenanceRequest %s", crName) {
		return
	}

	removed := assert.Eventually(t, func() bool {
		err := c.Resources().Get(ctx, crName, "", mr)

		return apierrors.IsNotFound(err)
	}, maintenanceRequestCleanupWait, maintenanceRequestPollInterval,
		"MaintenanceRequest %s should complete normal finalization during teardown", crName)
	if removed {
		return
	}

	t.Logf("MaintenanceRequest %s is still terminating; removing finalizers for test cleanup", crName)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(MaintenanceRequestGVK)

		if getErr := c.Resources().Get(ctx, crName, "", current); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return nil
			}

			return getErr
		}

		current.SetFinalizers(nil)

		return c.Resources().Update(ctx, current)
	})
	if !assert.NoError(t, err, "failed to remove finalizers from MaintenanceRequest %s", crName) {
		return
	}

	assert.Eventually(t, func() bool {
		err := c.Resources().Get(ctx, crName, "", mr)

		return apierrors.IsNotFound(err)
	}, maintenanceRequestCleanupWait, maintenanceRequestPollInterval,
		"MaintenanceRequest %s should be removed after finalizers are cleared", crName)
}
