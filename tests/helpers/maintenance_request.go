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

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// NodeRunningLifecycleManager returns the node hosting the lifecycle-manager
// pod. The MaintenanceRequest controller publishes from that pod, so every
// other node is one it can only report on by presenting an allowlisted token.
func NodeRunningLifecycleManager(ctx context.Context, c klient.Client) (string, error) {
	var pods v1.PodList

	err := c.Resources(NVSentinelNamespace).List(ctx, &pods,
		resources.WithLabelSelector(LifecycleManagerLabelSelector))
	if err != nil {
		return "", fmt.Errorf("failed to list lifecycle-manager pods: %w", err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != "" && pod.DeletionTimestamp == nil {
			return pod.Spec.NodeName, nil
		}
	}

	return "", fmt.Errorf("no scheduled lifecycle-manager pod found in namespace %s", NVSentinelNamespace)
}

// SelectMaintenanceTargetNode returns a clean, uncordoned worker node that is
// not avoidNode.
//
// Excluding avoidNode is the whole point: platform-connector pins a caller to
// its own node unless that caller is on the cross-node allowlist and presents
// its projected token, so an MR naming the publisher's own node would pass even
// with the token wiring removed. Failing loudly beats quietly falling back to
// the same node and turning the cross-node assertion into a no-op.
func SelectMaintenanceTargetNode(
	ctx context.Context, t *testing.T, c klient.Client, avoidNode string,
) string {
	t.Helper()

	names, err := AllRealNodeNames(ctx, c)
	require.NoError(t, err, "failed to list real worker nodes")

	var skipped []string

	for _, name := range names {
		if name == avoidNode {
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

		t.Logf("Selected maintenance target %s; lifecycle-manager runs on %s, so this is a cross-node publish",
			name, avoidNode)

		return name
	}

	require.FailNow(t,
		"no usable maintenance target node",
		"need a clean uncordoned worker other than %s (lifecycle-manager's node); skipped: %v",
		avoidNode, skipped)

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
