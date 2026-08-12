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
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"
)

const (
	PreflightNamespaceLabel      = "nvsentinel.nvidia.com/preflight"
	PreflightNamespaceLabelVal   = "enabled"
	PreflightDCGMDiagName        = "preflight-dcgm-diag"
	PreflightInheritEnabledName  = "preflight-inherit-enabled"
	PreflightInheritDisabledName = "preflight-inherit-disabled"
	PreflightConfigMapName       = "preflight"
	PreflightConfigKey           = "config.yaml"
	PreflightInheritedEnvName    = "NCCL_PREFLIGHT_INHERITED"
	PreflightInheritedEnvValue   = "from-user-container"
	PreflightInheritedVolumeName = "nccl-preflight-inherited"
	PreflightInheritedMountPath  = "/workload-nccl-config"

	GangConfigMapLabelManagedBy = "nvsentinel.nvidia.com/managed-by"
	GangConfigMapManagedByVal   = "preflight"
	GangDataKeyExpectedCount    = "expected_count"
	GangDataKeyPeers            = "peers"
	GangDataKeyMasterAddr       = "master_addr"
	GangDataKeyMasterPort       = "master_port"
	GangDataKeyGangID           = "gang_id"

	KAIPodGroupAnnotation = "scheduling.run.ai/pod-group"
)

// PreflightTestContext holds state for preflight E2E tests.
type PreflightTestContext struct {
	TestNamespace            string
	NodeNames                []string
	PodNames                 []string
	PodGroupName             string
	PreflightDeploymentName  string
	PreflightConfigMapBackup []byte
}

// SetupPreflightTest sets up the full preflight E2E scenario:
//   - Checks preflight is deployed and waits for rollout
//   - Gets N real worker nodes (skips if insufficient)
//   - Creates and labels a test namespace for preflight
//   - Verifies the preflight config ConfigMap exists
//   - Creates a KAI PodGroup and GPU pods (one per node)
func SetupPreflightTest(
	ctx context.Context, t *testing.T, c *envconf.Config,
	testNamespace, podGroupName string, nodeCount int,
) (context.Context, *PreflightTestContext) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err, "create kubernetes client")

	var deployList appsv1.DeploymentList

	err = client.Resources(NVSentinelNamespace).List(ctx, &deployList,
		resources.WithLabelSelector("app.kubernetes.io/name=preflight"))
	require.NoError(t, err, "list preflight deployments")

	if len(deployList.Items) == 0 {
		t.Skipf("Preflight not deployed in %s", NVSentinelNamespace)
	}

	preflightDeployName := deployList.Items[0].Name
	WaitForDeploymentRollout(ctx, t, client, preflightDeployName, NVSentinelNamespace)

	nodeNames, err := GetRealNodeNames(ctx, client, nodeCount)
	if err != nil {
		t.Skipf("Need %d real worker nodes: %v", nodeCount, err)
	}

	t.Logf("Using worker nodes: %v", nodeNames)

	testCtx := &PreflightTestContext{
		TestNamespace:           testNamespace,
		NodeNames:               nodeNames,
		PodGroupName:            podGroupName,
		PreflightDeploymentName: preflightDeployName,
	}

	t.Cleanup(func() {
		TeardownPreflightTest(ctx, t, c, testCtx)
	})

	err = CreateNamespace(ctx, client, testNamespace)
	require.NoError(t, err, "create test namespace")

	var ns v1.Namespace

	err = client.Resources().Get(ctx, testNamespace, "", &ns)
	require.NoError(t, err)

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}

	ns.Labels[PreflightNamespaceLabel] = PreflightNamespaceLabelVal

	err = client.Resources().Update(ctx, &ns)
	require.NoError(t, err, "label namespace for preflight")

	var cm v1.ConfigMap

	err = client.Resources(NVSentinelNamespace).Get(
		ctx, PreflightConfigMapName, NVSentinelNamespace, &cm,
	)
	require.NoError(t, err,
		"preflight config ConfigMap %s should exist", PreflightConfigMapName)
	require.Contains(t, cm.Data, PreflightConfigKey)

	testCtx.PreflightConfigMapBackup, err = BackupConfigMap(
		ctx, client, PreflightConfigMapName, NVSentinelNamespace,
	)
	require.NoError(t, err, "backup preflight config ConfigMap")

	ApplyPreflightInheritanceTestConfig(ctx, t, client, preflightDeployName)

	CreateKAIPodGroup(ctx, t, client, testNamespace, podGroupName, nodeCount)
	t.Logf("Created PodGroup %s/%s with minMember=%d",
		testNamespace, podGroupName, nodeCount)

	for i, node := range nodeNames {
		name := CreateGPUPodInGang(ctx, t, client, testNamespace, node, podGroupName)
		testCtx.PodNames = append(testCtx.PodNames, name)
		t.Logf("Created gang pod %d: %s on node %s", i, name, node)
	}

	return ctx, testCtx
}

// TeardownPreflightTest cleans up pods, PodGroup, and namespace.
func TeardownPreflightTest(
	ctx context.Context, t *testing.T, c *envconf.Config,
	testCtx *PreflightTestContext,
) context.Context {
	t.Helper()

	if testCtx == nil {
		return ctx
	}

	client, err := c.NewClient()
	if err != nil {
		return ctx
	}

	if len(testCtx.PreflightConfigMapBackup) > 0 {
		if err := createConfigMapFromBytes(
			ctx,
			client,
			testCtx.PreflightConfigMapBackup,
			PreflightConfigMapName,
			NVSentinelNamespace,
		); err != nil {
			t.Logf("Warning: failed to restore preflight ConfigMap: %v", err)
		} else if testCtx.PreflightDeploymentName != "" {
			restartPreflightDeployment(ctx, t, client, testCtx.PreflightDeploymentName)
		}

		testCtx.PreflightConfigMapBackup = nil
	}

	for _, podName := range testCtx.PodNames {
		_ = DeletePod(ctx, t, client, testCtx.TestNamespace, podName, false)
	}

	if testCtx.PodGroupName != "" {
		DeleteKAIPodGroup(ctx, client, testCtx.TestNamespace, testCtx.PodGroupName)
	}

	if testCtx.TestNamespace != "" {
		_ = client.Resources().Delete(ctx, &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: testCtx.TestNamespace},
		})
	}

	return ctx
}

// ApplyPreflightInheritanceTestConfig replaces the live preflight config with
// two lightweight checks: one opts into workload env/mount inheritance and one
// opts out. The original ConfigMap is restored by TeardownPreflightTest.
func ApplyPreflightInheritanceTestConfig(
	ctx context.Context, t *testing.T, client klient.Client, deploymentName string,
) {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &v1.ConfigMap{}
		if getErr := client.Resources().Get(
			ctx, PreflightConfigMapName, NVSentinelNamespace, cm,
		); getErr != nil {
			return getErr
		}

		rawConfig := cm.Data[PreflightConfigKey]
		if rawConfig == "" {
			return fmt.Errorf("ConfigMap %s/%s missing %s",
				NVSentinelNamespace, PreflightConfigMapName, PreflightConfigKey)
		}

		var config map[string]any
		if unmarshalErr := yaml.Unmarshal([]byte(rawConfig), &config); unmarshalErr != nil {
			return fmt.Errorf("unmarshal preflight config: %w", unmarshalErr)
		}

		config["ncclEnvPatterns"] = []string{"NCCL_*"}
		config["volumeMountPatterns"] = []string{PreflightInheritedVolumeName}
		config["initContainers"] = []map[string]any{
			preflightInheritanceTestInitContainer(PreflightInheritEnabledName, true),
			preflightInheritanceTestInitContainer(PreflightInheritDisabledName, false),
		}

		updated, marshalErr := yaml.Marshal(config)
		if marshalErr != nil {
			return fmt.Errorf("marshal preflight config: %w", marshalErr)
		}

		cm.Data[PreflightConfigKey] = string(updated)

		return client.Resources().Update(ctx, cm)
	})
	require.NoError(t, err, "apply preflight inheritance test config")

	restartPreflightDeployment(ctx, t, client, deploymentName)
}

func preflightInheritanceTestInitContainer(name string, inherit bool) map[string]any {
	return map[string]any{
		"name":                    name,
		"image":                   "busybox:latest",
		"command":                 []string{"/bin/sh", "-c"},
		"args":                    []string{"true"},
		"inheritUserEnv":          inherit,
		"inheritUserVolumeMounts": inherit,
	}
}

func restartPreflightDeployment(
	ctx context.Context, t *testing.T, client klient.Client, deploymentName string,
) {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if getErr := client.Resources().Get(
			ctx, deploymentName, NVSentinelNamespace, deployment,
		); getErr != nil {
			return getErr
		}

		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}

		deployment.Spec.Template.Annotations["nvsentinel.nvidia.com/preflight-e2e-config-revision"] =
			fmt.Sprintf("%d", time.Now().UnixNano())

		return client.Resources().Update(ctx, deployment)
	})
	require.NoError(t, err, "restart preflight deployment")

	WaitForDeploymentRollout(ctx, t, client, deploymentName, NVSentinelNamespace)
}

// WaitForPodInitContainerStatuses waits until all preflight init containers
// have terminated (Completed or Error).
func WaitForPodInitContainerStatuses(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, podName string,
) *v1.Pod {
	t.Helper()

	var pod v1.Pod

	require.Eventually(t, func() bool {
		err := client.Resources().Get(ctx, podName, namespace, &pod)
		if err != nil {
			return false
		}

		return PreflightInitContainersTerminated(&pod)
	}, EventuallyWaitTimeout, WaitInterval,
		"pod %s: all preflight-* init containers should terminate",
		podName)

	return &pod
}

// PreflightInitContainersTerminated returns true once every injected preflight
// init container has reached Terminated state.
func PreflightInitContainersTerminated(pod *v1.Pod) bool {
	expected := make(map[string]struct{})

	for _, initContainer := range pod.Spec.InitContainers {
		if strings.HasPrefix(initContainer.Name, "preflight-") {
			expected[initContainer.Name] = struct{}{}
		}
	}

	if len(expected) == 0 {
		return false
	}

	terminated := 0

	for _, st := range pod.Status.InitContainerStatuses {
		if _, ok := expected[st.Name]; !ok {
			continue
		}

		if st.State.Terminated == nil {
			return false
		}

		terminated++
	}

	return terminated == len(expected)
}

// RequireInitContainer returns the named init container from a pod, failing the
// test if it is not present.
func RequireInitContainer(t *testing.T, pod v1.Pod, name string) v1.Container {
	t.Helper()

	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			return container
		}
	}

	require.Failf(t, "missing init container", "pod %s missing init container %s", pod.Name, name)

	return v1.Container{}
}

// FindEnvValue returns the value for an env var name, or an empty string when
// the env var is absent.
func FindEnvValue(envVars []v1.EnvVar, name string) string {
	for _, env := range envVars {
		if env.Name == name {
			return env.Value
		}
	}

	return ""
}

// HasVolumeMount reports whether a volume mount with the given name exists.
func HasVolumeMount(mounts []v1.VolumeMount, name string) bool {
	for _, mount := range mounts {
		if mount.Name == name {
			return true
		}
	}

	return false
}

// ListGangConfigMaps lists ConfigMaps with the preflight gang label
// in the given namespaces.
func ListGangConfigMaps(
	ctx context.Context, client klient.Client, namespaces []string,
) ([]v1.ConfigMap, error) {
	selector := GangConfigMapLabelManagedBy + "=" + GangConfigMapManagedByVal

	var all []v1.ConfigMap

	for _, ns := range namespaces {
		var list v1.ConfigMapList

		err := client.Resources(ns).List(
			ctx, &list, resources.WithLabelSelector(selector),
		)
		if err != nil {
			return nil, fmt.Errorf("listing ConfigMaps in namespace %s: %w", ns, err)
		}

		all = append(all, list.Items...)
	}

	return all, nil
}

// AssertGangConfigMap waits for the gang ConfigMap to be fully populated
// (gang_id, expected_count, peers, master_addr, master_port) and asserts
// correct values. Polls until all fields are present and match expectations
// to avoid flaky one-shot assertions on a partially-populated ConfigMap.
func AssertGangConfigMap(
	ctx context.Context, t *testing.T, client klient.Client,
	testCtx *PreflightTestContext, expectedGangID string,
	expectedPeerCount int,
) *v1.ConfigMap {
	t.Helper()

	namespaces := []string{testCtx.TestNamespace, NVSentinelNamespace}
	expectedCountStr := fmt.Sprintf("%d", expectedPeerCount)

	var found *v1.ConfigMap

	require.Eventually(t, func() bool {
		all, err := ListGangConfigMaps(ctx, client, namespaces)
		if err != nil {
			return false
		}

		for i := range all {
			cm := &all[i]
			if cm.Data[GangDataKeyGangID] != expectedGangID {
				continue
			}

			if cm.Data[GangDataKeyExpectedCount] != expectedCountStr {
				continue
			}

			peers := strings.TrimSpace(cm.Data[GangDataKeyPeers])
			if peers == "" || len(strings.Split(peers, "\n")) != expectedPeerCount {
				continue
			}

			if cm.Data[GangDataKeyMasterAddr] == "" {
				continue
			}

			if cm.Data[GangDataKeyMasterPort] == "" {
				continue
			}

			found = cm

			return true
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"gang ConfigMap with gang_id=%s should be fully populated "+
			"(expected_count=%s, %d peers, master_addr, master_port)",
		expectedGangID, expectedCountStr, expectedPeerCount)

	t.Logf("Gang ConfigMap %s/%s: gang_id=%s expected_count=%s peers=\n%s",
		found.Namespace, found.Name,
		found.Data[GangDataKeyGangID],
		found.Data[GangDataKeyExpectedCount],
		found.Data[GangDataKeyPeers])

	return found
}

// WaitForGangConfigMapDeleted waits until the named gang ConfigMap is gone.
func WaitForGangConfigMapDeleted(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, name string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		var cm v1.ConfigMap

		err := client.Resources(namespace).Get(ctx, name, namespace, &cm)

		return apierrors.IsNotFound(err)
	}, EventuallyWaitTimeout, WaitInterval,
		"gang ConfigMap %s/%s should be garbage-collected", namespace, name)
}

// CreateKAIPodGroup creates a KAI Scheduler PodGroup with the given
// minMember in the namespace.
func CreateKAIPodGroup(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, name string, minMember int,
) {
	t.Helper()

	pg := NewKAIPodGroup(namespace, name)
	_ = unstructured.SetNestedField(pg.Object, int64(minMember), "spec", "minMember")

	err := client.Resources(namespace).Create(ctx, pg)
	require.NoError(t, err,
		"create KAI PodGroup %s/%s", namespace, name)
}

// GetKAIPodGroup retrieves a KAI Scheduler PodGroup.
func GetKAIPodGroup(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, name string,
) *unstructured.Unstructured {
	t.Helper()

	pg := NewKAIPodGroup(namespace, name)
	err := client.Resources(namespace).Get(ctx, name, namespace, pg)
	require.NoError(t, err, "get KAI PodGroup %s/%s", namespace, name)

	return pg
}

// DeleteKAIPodGroup deletes a KAI Scheduler PodGroup (best-effort).
func DeleteKAIPodGroup(
	ctx context.Context, client klient.Client, namespace, name string,
) {
	_ = client.Resources(namespace).Delete(ctx, NewKAIPodGroup(namespace, name))
}

// NewKAIPodGroup returns an unstructured KAI Scheduler PodGroup object.
func NewKAIPodGroup(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "scheduling.run.ai/v2alpha2",
			"kind":       "PodGroup",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
}

// CreateGPUPodInGang creates a GPU pod annotated with the KAI PodGroup
// name and pinned to nodeName. Returns the pod name.
func CreateGPUPodInGang(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, nodeName, podGroupName string,
) string {
	t.Helper()

	var podName string

	require.Eventually(t, func() bool {
		pod := newPreflightGangPod(namespace, nodeName, podGroupName)

		err := client.Resources().Create(ctx, pod)
		if err != nil {
			t.Logf("Retrying GPU pod create for gang %s after error: %v", podGroupName, err)

			return false
		}

		podName = pod.Name

		return podName != ""
	}, EventuallyWaitTimeout, WaitInterval, "create GPU pod in gang %s", podGroupName)

	require.NotEmpty(t, podName)

	return podName
}

func newPreflightGangPod(namespace, nodeName, podGroupName string) *v1.Pod {
	pod := NewGPUPodSpec(namespace, 1)
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	pod.Annotations[KAIPodGroupAnnotation] = podGroupName
	pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
		Name: PreflightInheritedVolumeName,
		VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		},
	})
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, v1.EnvVar{
		Name:  PreflightInheritedEnvName,
		Value: PreflightInheritedEnvValue,
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
		Name:      PreflightInheritedVolumeName,
		MountPath: PreflightInheritedMountPath,
	})

	if nodeName != "" {
		pod.Spec.NodeName = nodeName
	}

	return pod
}

// ExpectedKAIGangID returns the gang ID the preflight webhook generates
// for a KAI PodGroup: kai-{namespace}-{podGroupName}.
func ExpectedKAIGangID(namespace, podGroupName string) string {
	return fmt.Sprintf("kai-%s-%s", namespace, podGroupName)
}
