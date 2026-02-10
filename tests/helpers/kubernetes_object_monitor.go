// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	K8S_DEPLOYMENT_NAME = "kubernetes-object-monitor"
	K8S_CONTAINER_NAME  = "kubernetes-object-monitor"

	// K8sObjectMonitorAnnotationKey is the annotation key for policy matches set by kubernetes-object-monitor
	K8sObjectMonitorAnnotationKey = "nvsentinel.nvidia.com/k8s-object-monitor-policy-matches"
)

type KubernetesObjectMonitorTestContext struct {
	NodeName        string
	ConfigMapBackup []byte
	TestNamespace   string
}

func TeardownKubernetesObjectMonitor(
	ctx context.Context, t *testing.T, c *envconf.Config, configMapBackup []byte, originalArgs []string,
) {
	t.Helper()

	client, err := c.NewClient()
	require.NoError(t, err)

	if configMapBackup != nil {
		t.Log("Restoring configmap from memory")

		err = createConfigMapFromBytes(ctx, client, configMapBackup, "kubernetes-object-monitor", NVSentinelNamespace)
		require.NoError(t, err)

		err = RestartDeployment(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
		require.NoError(t, err)
	}

	err = RestoreDeploymentArgs(t, ctx, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace, K8S_CONTAINER_NAME, originalArgs)
	require.NoError(t, err)

	WaitForDeploymentRollout(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
}

func UpdateKubernetesObjectMonitorConfigMap(ctx context.Context, t *testing.T, client klient.Client,
	configMapPath string, configName string) {
	t.Helper()

	if configMapPath == "" {
		t.Fatalf("configMapPath is empty")
	}

	t.Logf("Updating configmap %s", configName)

	err := createConfigMapFromFilePath(ctx, client, configMapPath, configName, NVSentinelNamespace)
	require.NoError(t, err)

	t.Logf("Restarting %s deployment", K8S_DEPLOYMENT_NAME)

	err = RestartDeployment(ctx, t, client, K8S_DEPLOYMENT_NAME, NVSentinelNamespace)
	require.NoError(t, err)
}

// DaemonSetFailureType specifies the type of failure to simulate in a test DaemonSet.
type DaemonSetFailureType int

const (
	// DaemonSetHealthy creates a healthy DaemonSet with no failures
	DaemonSetHealthy DaemonSetFailureType = iota
	// DaemonSetInitBlocking creates a DaemonSet with init container that blocks (sleep)
	DaemonSetInitBlocking
	// DaemonSetInitCrashLoop creates a DaemonSet with init container that crashes (exit 1)
	DaemonSetInitCrashLoop
	// DaemonSetMainCrashLoop creates a DaemonSet with main container that crashes (exit 1)
	DaemonSetMainCrashLoop
)

// BuildTestDaemonSet builds a test DaemonSet object with the specified failure type.
// Failure types and their effects:
//   - DaemonSetHealthy: Pod reaches Running state normally
//   - DaemonSetInitBlocking: Pod stuck in Pending/Init (phase=Pending, detected by phase check)
//   - DaemonSetInitCrashLoop: Init container CrashLoopBackOff (phase=Pending, detected by phase check)
//   - DaemonSetMainCrashLoop: Main container CrashLoopBackOff (phase=Running, detected by containerStatuses check)
func BuildTestDaemonSet(name, namespace, nodeName string, failureType DaemonSetFailureType) *appsv1.DaemonSet {
	selectorLabel := "app-" + name
	// Use short termination grace period for fast cleanup during tests
	terminationGracePeriod := int64(1)

	podSpec := v1.PodSpec{
		NodeSelector: map[string]string{
			"kubernetes.io/hostname": nodeName,
		},
		RestartPolicy:                 v1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: &terminationGracePeriod,
		Tolerations: []v1.Toleration{
			{Operator: v1.TolerationOpExists},
		},
	}

	switch failureType {
	case DaemonSetMainCrashLoop:
		// Main container crashes immediately - pod phase=Running but container in CrashLoopBackOff
		podSpec.Containers = []v1.Container{
			{
				Name:    "crasher",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "exit 1"},
			},
		}
	case DaemonSetInitCrashLoop:
		// Init container crashes immediately - pod phase=Pending with init in CrashLoopBackOff
		podSpec.InitContainers = []v1.Container{
			{
				Name:    "init-crasher",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "exit 1"},
			},
		}
		podSpec.Containers = []v1.Container{
			{
				Name:    "main",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "sleep 3600"},
			},
		}
	case DaemonSetInitBlocking:
		// Init container blocks forever - pod stuck in Pending/Init state
		podSpec.InitContainers = []v1.Container{
			{
				Name:    "init-blocker",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "sleep 3600"},
			},
		}
		podSpec.Containers = []v1.Container{
			{
				Name:    "main",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "sleep 3600"},
			},
		}
	case DaemonSetHealthy:
		// Healthy pod - main container runs normally
		podSpec.Containers = []v1.Container{
			{
				Name:    "main",
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "sleep 3600"},
			},
		}
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":  selectorLabel,
				"test": "kubernetes-object-monitor",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": selectorLabel,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":  selectorLabel,
						"test": "kubernetes-object-monitor",
					},
				},
				Spec: podSpec,
			},
		},
	}
}

// UpdateDaemonSetToHealthy updates a DaemonSet to remove the blocking init container,
// allowing the pod to reach Running state.
func UpdateDaemonSetToHealthy(ctx context.Context, client klient.Client, namespace, name string) error {
	return UpdateDaemonSet(ctx, client, namespace, name, func(ds *appsv1.DaemonSet) {
		ds.Spec.Template.Spec.InitContainers = nil
	})
}

// FixCrashingContainer updates a DaemonSet to fix the crashing main container.
// Changes the container command from "exit 1" to "sleep 3600" so the pod becomes healthy.
// Returns an error if the "crasher" container is not found in the DaemonSet.
func FixCrashingContainer(ctx context.Context, client klient.Client, namespace, name string) error {
	const containerName = "crasher"

	found := false

	err := UpdateDaemonSet(ctx, client, namespace, name, func(ds *appsv1.DaemonSet) {
		for i := range ds.Spec.Template.Spec.Containers {
			if ds.Spec.Template.Spec.Containers[i].Name == containerName {
				ds.Spec.Template.Spec.Containers[i].Command = []string{"sh", "-c", "sleep 3600"}
				found = true

				break
			}
		}
	})
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("container %q not found in DaemonSet %s/%s", containerName, namespace, name)
	}

	return nil
}

// FixCrashingInitContainer updates a DaemonSet to fix the crashing init container.
// Changes the init container command from "exit 1" to "exit 0" so the pod can start.
// Returns an error if the "init-crasher" container is not found in the DaemonSet.
func FixCrashingInitContainer(ctx context.Context, client klient.Client, namespace, name string) error {
	const containerName = "init-crasher"

	found := false

	err := UpdateDaemonSet(ctx, client, namespace, name, func(ds *appsv1.DaemonSet) {
		for i := range ds.Spec.Template.Spec.InitContainers {
			if ds.Spec.Template.Spec.InitContainers[i].Name == containerName {
				ds.Spec.Template.Spec.InitContainers[i].Command = []string{"sh", "-c", "exit 0"}
				found = true

				break
			}
		}
	})
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("init container %q not found in DaemonSet %s/%s", containerName, namespace, name)
	}

	return nil
}
