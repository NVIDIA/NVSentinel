// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	nodeDrainerName            = "node-drainer"
	nodeDrainerContainerName   = "node-drainer"
	drainTemplateVolumeName    = "drain-template"
	drainTemplateConfigMapName = "drain-template"
	drainTemplateMountPath     = "/etc/drain-template"
	drainTemplateFixture       = "data/custom-drain-template.yaml"
	drainRequestAPIGroup       = "nvsentinel.nvidia.com"
	drainRequestVersion        = "v1alpha1"
	drainRequestKind           = "DrainRequest"
	drainRequestResource       = "drainrequests"
	// drainRequestNodeLabel matches node-drainer's customdrain.NodeNameLabelKey.
	drainRequestNodeLabel        = "nvsentinel.nvidia.com/node-name"
	drainCompleteConditionType   = "Complete"
	drainCompleteConditionStatus = "True"
	drainCompleteReason          = "DrainFinished"
	drainCompleteMessage         = "test drain plugin marked the drain complete"
	conditionTypeField           = "type"
	// conditionStatusField is the condition's status key. The NVCRE helper owns that string.
	conditionStatusField         = nvcreStatusField
	conditionReasonField         = "reason"
	conditionLastTransitionField = "lastTransitionTime"
)

var drainRequestGVK = schema.GroupVersionKind{
	Group:   drainRequestAPIGroup,
	Version: drainRequestVersion,
	Kind:    drainRequestKind,
}

// CustomDrainBackup is the cluster state SetupCustomDrain changes.
// TeardownCustomDrain puts that state back. A field stays at its zero value when that step did not run,
// so teardown is safe when setup stops part way through.
type CustomDrainBackup struct {
	Rules           []rbacv1.PolicyRule
	RulesSaved      bool
	Deployment      *appsv1.Deployment
	TemplateCreated bool
}

// SetupCustomDrain gives node-drainer the template file, the DrainRequest permission, and the volume
// mount that the Helm chart adds only when custom drain is enabled at install time.
// The caller keeps backup and passes it to TeardownCustomDrain.
func SetupCustomDrain(ctx context.Context, t *testing.T, client klient.Client, backup *CustomDrainBackup) {
	t.Helper()

	rules, err := GetClusterRoleRules(ctx, client, nodeDrainerName)
	require.NoError(t, err)

	backup.Rules = rules
	backup.RulesSaved = true

	t.Log("Granting node-drainer permission to use DrainRequest")

	err = SetClusterRoleRules(ctx, client, nodeDrainerName, withDrainRequestRule(rules))
	require.NoError(t, err)

	t.Log("Creating the drain template ConfigMap")

	err = createConfigMapFromFilePath(ctx, client, drainTemplateFixture, drainTemplateConfigMapName, NVSentinelNamespace)
	require.NoError(t, err)

	backup.TemplateCreated = true

	t.Log("Mounting the drain template into node-drainer")

	err = retry.RetryOnConflict(ConflictBackoff, func() error {
		current := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, nodeDrainerName, NVSentinelNamespace, current); err != nil {
			return err
		}

		if backup.Deployment == nil {
			backup.Deployment = current.DeepCopy()
		}

		if err := addDrainTemplateVolume(current); err != nil {
			return err
		}

		return client.Resources().Update(ctx, current)
	})
	require.NoError(t, err)

	WaitForDeploymentRollout(ctx, t, client, nodeDrainerName, NVSentinelNamespace)
}

// TeardownCustomDrain puts back the deployment, the ClusterRole, and the template ConfigMap
// that SetupCustomDrain changed, and deletes DrainRequests left for nodeName.
func TeardownCustomDrain(
	ctx context.Context, t *testing.T, client klient.Client, backup *CustomDrainBackup, nodeName string,
) {
	t.Helper()

	if backup == nil {
		return
	}

	restored := false

	if backup.Deployment != nil {
		t.Log("Restoring the node-drainer deployment")

		err := restoreDeploymentSpec(ctx, client, backup.Deployment)
		assert.NoError(t, err)

		restored = err == nil
	}

	if backup.RulesSaved {
		t.Log("Restoring the node-drainer ClusterRole")
		assert.NoError(t, SetClusterRoleRules(ctx, client, nodeDrainerName, backup.Rules))
	}

	// Without a saved Deployment, setup never mounted the template, so the ConfigMap is unused.
	templateUnmounted := restored || backup.Deployment == nil

	if backup.TemplateCreated && templateUnmounted {
		t.Log("Deleting the drain template ConfigMap")
		assert.NoError(t, deleteTemplateConfigMap(ctx, client))
	} else if backup.TemplateCreated {
		t.Log("Leaving the drain template ConfigMap because the deployment still mounts it")
	}

	deleteDrainRequests(ctx, t, client, nodeName)

	if restored {
		WaitForDeploymentRollout(ctx, t, client, nodeDrainerName, NVSentinelNamespace)
	}
}

// WaitForDrainRequest returns the DrainRequest that node-drainer created for nodeName.
func WaitForDrainRequest(
	ctx context.Context, t *testing.T, client klient.Client, nodeName string,
) *unstructured.Unstructured {
	t.Helper()
	t.Logf("Waiting for a DrainRequest for node %s", nodeName)

	var found *unstructured.Unstructured

	require.Eventually(t, func() bool {
		items, err := listDrainRequests(ctx, client, nodeName)
		if err != nil {
			t.Logf("list DrainRequests: %v", err)

			return false
		}

		if len(items) == 0 {
			return false
		}

		found = items[0].DeepCopy()

		return true
	}, EventuallyWaitTimeout, WaitInterval, "DrainRequest should exist for node %s", nodeName)

	return found
}

// CompleteDrainRequest sets the Complete condition, the same way a drain plugin does when it finishes.
func CompleteDrainRequest(ctx context.Context, t *testing.T, client klient.Client, name string) {
	t.Helper()
	t.Logf("Marking DrainRequest %s complete", name)

	err := retry.RetryOnConflict(ConflictBackoff, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(drainRequestGVK)

		if err := client.Resources().Get(ctx, name, NVSentinelNamespace, current); err != nil {
			return err
		}

		conditions := []interface{}{drainCompleteCondition()}
		if err := unstructured.SetNestedSlice(current.Object, conditions, "status", "conditions"); err != nil {
			return err
		}

		return client.Resources().UpdateStatus(ctx, current)
	})
	require.NoError(t, err)
}

// WaitForNoDrainRequest waits until node-drainer has deleted the DrainRequest for nodeName.
func WaitForNoDrainRequest(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	t.Helper()
	t.Logf("Waiting for the DrainRequest for node %s to be deleted", nodeName)

	require.Eventually(t, func() bool {
		items, err := listDrainRequests(ctx, client, nodeName)
		if err != nil {
			t.Logf("list DrainRequests: %v", err)

			return false
		}

		return len(items) == 0
	}, EventuallyWaitTimeout, WaitInterval, "DrainRequest should be deleted for node %s", nodeName)
}

func withDrainRequestRule(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	for _, rule := range rules {
		if ruleGrantsDrainRequests(rule) {
			return rules
		}
	}

	updated := make([]rbacv1.PolicyRule, len(rules), len(rules)+1)
	copy(updated, rules)

	updated = append(updated, rbacv1.PolicyRule{
		APIGroups: []string{drainRequestAPIGroup},
		Resources: []string{drainRequestResource, drainRequestResource + "/status"},
		Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
	})

	return updated
}

func ruleGrantsDrainRequests(rule rbacv1.PolicyRule) bool {
	return containsString(rule.APIGroups, drainRequestAPIGroup) &&
		containsString(rule.Resources, drainRequestResource)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func addDrainTemplateVolume(deployment *appsv1.Deployment) error {
	podSpec := &deployment.Spec.Template.Spec

	if !volumePresent(podSpec.Volumes, drainTemplateVolumeName) {
		podSpec.Volumes = append(podSpec.Volumes, v1.Volume{
			Name: drainTemplateVolumeName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{Name: drainTemplateConfigMapName},
				},
			},
		})
	}

	for i := range podSpec.Containers {
		container := &podSpec.Containers[i]
		if container.Name != nodeDrainerContainerName {
			continue
		}

		if !mountPresent(container.VolumeMounts, drainTemplateVolumeName) {
			container.VolumeMounts = append(container.VolumeMounts, v1.VolumeMount{
				Name:      drainTemplateVolumeName,
				MountPath: drainTemplateMountPath,
				ReadOnly:  true,
			})
		}

		return nil
	}

	return fmt.Errorf("container %q not found in deployment %s", nodeDrainerContainerName, deployment.Name)
}

func volumePresent(volumes []v1.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name {
			return true
		}
	}

	return false
}

func mountPresent(mounts []v1.VolumeMount, name string) bool {
	for _, mount := range mounts {
		if mount.Name == name {
			return true
		}
	}

	return false
}

func restoreDeploymentSpec(ctx context.Context, client klient.Client, original *appsv1.Deployment) error {
	err := retry.RetryOnConflict(ConflictBackoff, func() error {
		current := &appsv1.Deployment{}
		if err := client.Resources().Get(ctx, original.Name, original.Namespace, current); err != nil {
			return err
		}

		current.Spec = original.Spec

		return client.Resources().Update(ctx, current)
	})
	if err != nil {
		return fmt.Errorf("restore deployment %s: %w", original.Name, err)
	}

	return nil
}

func deleteTemplateConfigMap(ctx context.Context, client klient.Client) error {
	configMap := &v1.ConfigMap{}
	configMap.Name = drainTemplateConfigMapName
	configMap.Namespace = NVSentinelNamespace

	err := client.Resources().Delete(ctx, configMap)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("delete drain template configmap: %w", err)
	}

	return nil
}

func deleteDrainRequests(ctx context.Context, t *testing.T, client klient.Client, nodeName string) {
	t.Helper()

	if nodeName == "" {
		return
	}

	items, err := listDrainRequests(ctx, client, nodeName)
	if err != nil {
		assert.NoError(t, err)

		return
	}

	for i := range items {
		err := client.Resources().Delete(ctx, &items[i])
		if apierrors.IsNotFound(err) {
			continue
		}

		assert.NoError(t, err, "delete DrainRequest %s", items[i].GetName())
	}
}

func listDrainRequests(
	ctx context.Context, client klient.Client, nodeName string,
) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(drainRequestGVK)

	selector := fmt.Sprintf("%s=%s", drainRequestNodeLabel, nodeName)

	err := client.Resources(NVSentinelNamespace).List(ctx, list, resources.WithLabelSelector(selector))
	if err != nil {
		return nil, fmt.Errorf("list DrainRequests for node %s: %w", nodeName, err)
	}

	items := make([]unstructured.Unstructured, len(list.Items))
	copy(items, list.Items)

	return items, nil
}

func drainCompleteCondition() map[string]any {
	return map[string]any{
		conditionTypeField:           drainCompleteConditionType,
		conditionStatusField:         drainCompleteConditionStatus,
		conditionReasonField:         drainCompleteReason,
		fieldMessageKey:              drainCompleteMessage,
		conditionLastTransitionField: time.Now().UTC().Format(time.RFC3339),
	}
}
