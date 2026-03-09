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

package generic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

func newTestClient(objects ...runtime.Object) *Client {
	return NewClientWithK8s(fake.NewSimpleClientset(objects...), Config{
		RebootImage:        "busybox:1.37",
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
	})
}

func newNode(name, bootID string, ready bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}

	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{BootID: bootID},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: status},
			},
		},
	}
}

var _ model.CSPClient = (*Client)(nil)

func TestSendRebootSignal_CreatesJob(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node)
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	assert.Equal(t, "worker-1", job.Spec.Template.Spec.NodeName)
	assert.Equal(t, "true", job.Labels[jobLabelKey])
	assert.Equal(t, "worker-1", job.Labels[jobNodeLabelKey])
	assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)

	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "busybox:1.37", container.Image)
	assert.Equal(t, []string{"chroot", hostMountPath, "reboot"}, container.Command)
	require.NotNil(t, container.SecurityContext)
	assert.True(t, *container.SecurityContext.Privileged)

	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "/", job.Spec.Template.Spec.Volumes[0].HostPath.Path)

	require.Len(t, job.Spec.Template.Spec.Tolerations, 1)
	assert.Equal(t, corev1.TolerationOpExists, job.Spec.Template.Spec.Tolerations[0].Operator)

	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
	assert.Equal(t, int32(3600), *job.Spec.TTLSecondsAfterFinished)
}

func TestSendRebootSignal_EmptyBootID(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "", true)

	_, err := client.SendRebootSignal(ctx, node)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no bootID")
}

func TestIsNodeReady_BootIDUnchanged(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	require.NoError(t, err)
	assert.False(t, ready)
}

func TestIsNodeReady_BootIDChangedButNotReady(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-new", false)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-old")
	require.NoError(t, err)
	assert.False(t, ready)
}

func TestIsNodeReady_BootIDChangedAndReady(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-new", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-old")
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestIsNodeReady_BootIDChangedAndReady_CleansUpJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reboot-worker-1-abc12",
			Namespace: "test-ns",
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: "worker-1",
			},
		},
	}
	client := newTestClient(job)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-new", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-old")
	require.NoError(t, err)
	assert.True(t, ready)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items)
}

func TestIsNodeReady_ImagePullBackOff(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reboot-worker-1-pod",
			Namespace: "test-ns",
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: "worker-1",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	client := newTestClient(pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ImagePullBackOff")
}

func TestIsNodeReady_ErrImagePull(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reboot-worker-1-pod",
			Namespace: "test-ns",
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: "worker-1",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ErrImagePull",
						},
					},
				},
			},
		},
	}

	client := newTestClient(pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ErrImagePull")
}

func TestIsNodeReady_ContainerCreating(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reboot-worker-1-pod",
			Namespace: "test-ns",
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: "worker-1",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ContainerCreating",
						},
					},
				},
			},
		},
	}

	client := newTestClient(pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	require.NoError(t, err)
	assert.False(t, ready)
}

func TestIsNodeReady_CreateContainerConfigError(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reboot-worker-1-pod",
			Namespace: "test-ns",
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: "worker-1",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CreateContainerConfigError",
						},
					},
				},
			},
		},
	}

	client := newTestClient(pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CreateContainerConfigError")
}

func TestSendTerminateSignal_ReturnsError(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.SendTerminateSignal(ctx, node)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestSendRebootSignal_JobCreationFailure(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	client := NewClientWithK8s(fakeClient, Config{
		RebootImage:        "busybox:1.37",
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.SendRebootSignal(ctx, node)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create reboot job")
}

func TestBuildRebootJob_UsesGenerateName(t *testing.T) {
	client := newTestClient()
	job := client.buildRebootJob("worker-1")

	assert.Empty(t, job.Name)
	assert.Equal(t, "reboot-worker-1-", job.GenerateName)
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		config := loadConfigFromEnv()
		assert.Equal(t, defaultRebootImage, config.RebootImage)
		assert.Equal(t, "", config.RebootJobNamespace)
		assert.Equal(t, int32(defaultRebootJobTTLSeconds), config.RebootJobTTL)
	})

	t.Run("custom values", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_IMAGE", "custom-image:latest")
		t.Setenv("GENERIC_REBOOT_JOB_NAMESPACE", "custom-ns")
		t.Setenv("GENERIC_REBOOT_JOB_TTL", "7200")
		t.Setenv("GENERIC_REBOOT_IMAGE_PULL_SECRETS", "secret-a, secret-b")

		config := loadConfigFromEnv()
		assert.Equal(t, "custom-image:latest", config.RebootImage)
		assert.Equal(t, "custom-ns", config.RebootJobNamespace)
		assert.Equal(t, int32(7200), config.RebootJobTTL)
		assert.Equal(t, []string{"secret-a", "secret-b"}, config.RebootJobPullSecrets)
	})

	t.Run("invalid TTL uses default", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_JOB_TTL", "not-a-number")

		config := loadConfigFromEnv()
		assert.Equal(t, int32(defaultRebootJobTTLSeconds), config.RebootJobTTL)
	})
}
