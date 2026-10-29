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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		Name: name,
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

	ref, err := client.SendRebootSignal(ctx, node, "")
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
	assert.Equal(t, guardedCommand("chroot /host reboot"), container.Command)
	assert.Equal(t, []corev1.EnvVar{{Name: preRebootBootIDEnvVar, Value: "boot-id-abc"}}, container.Env)
	require.NotNil(t, container.SecurityContext)
	assert.True(t, *container.SecurityContext.Privileged)

	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "/", job.Spec.Template.Spec.Volumes[0].HostPath.Path)

	require.Len(t, job.Spec.Template.Spec.Tolerations, 1)
	assert.Equal(t, corev1.TolerationOpExists, job.Spec.Template.Spec.Tolerations[0].Operator)

	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
	assert.Equal(t, int32(3600), *job.Spec.TTLSecondsAfterFinished)
}

func TestSendRebootSignal_CreatesJob_WithSyslog(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        true,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand(
		"chroot /host logger -t nvsentinel-reboot -p daemon.notice \"NVSentinel reboot: node=worker-1\" || true; chroot /host reboot",
	), container.Command)
}

func TestSendRebootSignal_CreatesJob_WithSyslogAndCRName(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        true,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "rebootnode-worker-1-xyz")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand(
		"chroot /host logger -t nvsentinel-reboot -p daemon.notice \"NVSentinel reboot: node=worker-1 cr=rebootnode-worker-1-xyz\" || true; chroot /host reboot",
	), container.Command)
}

func TestSendRebootSignal_CreatesSysrqJob_WithSyslog(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		UseSysrqReboot:     true,
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        true,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand(
		"chroot /host logger -t nvsentinel-reboot -p daemon.notice \"NVSentinel reboot: node=worker-1\" || true; sync || true; echo b > /host-proc/sysrq-trigger",
	), container.Command)

	require.Len(t, container.VolumeMounts, 2)
	assert.Equal(t, corev1.VolumeMount{
		Name:      "host-proc",
		MountPath: hostProcMountPath,
	}, container.VolumeMounts[0])
	assert.Equal(t, corev1.VolumeMount{
		Name:      "host-root",
		MountPath: hostMountPath,
	}, container.VolumeMounts[1])

	require.Len(t, job.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, corev1.Volume{
		Name: "host-proc",
		HostPath: &corev1.HostPathVolumeSource{
			Path: "/proc",
		},
	}, job.Spec.Template.Spec.Volumes[0])
	assert.Equal(t, corev1.Volume{
		Name: "host-root",
		HostPath: &corev1.HostPathVolumeSource{
			Path: "/",
		},
	}, job.Spec.Template.Spec.Volumes[1])
}

func TestSendRebootSignal_CreatesSysrqJob_WithSyslogAndCRName(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		UseSysrqReboot:     true,
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        true,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "rebootnode-worker-1-xyz")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand(
		"chroot /host logger -t nvsentinel-reboot -p daemon.notice \"NVSentinel reboot: node=worker-1 cr=rebootnode-worker-1-xyz\" || true; sync || true; echo b > /host-proc/sysrq-trigger",
	), container.Command)
}

func TestSendRebootSignal_CreatesSysrqJob_WithoutSyslog(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		UseSysrqReboot:     true,
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        false,
	})
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	job := jobs.Items[0]
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand("echo b > /host-proc/sysrq-trigger"), container.Command)

	require.Len(t, container.VolumeMounts, 1)
	assert.Equal(t, corev1.VolumeMount{
		Name:      "host-proc",
		MountPath: hostProcMountPath,
	}, container.VolumeMounts[0])
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, corev1.Volume{
		Name: "host-proc",
		HostPath: &corev1.HostPathVolumeSource{
			Path: "/proc",
		},
	}, job.Spec.Template.Spec.Volumes[0])
}

func TestBuildRebootJob_UsesCommandRebootByDefault(t *testing.T) {
	client := newTestClient()

	job := client.buildRebootJob("worker-1", "", "boot-id-abc")
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand("chroot /host reboot"), container.Command)
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "host-root", job.Spec.Template.Spec.Volumes[0].Name)
}

func TestBuildRebootJob_UsesSyslogWhenEnabled(t *testing.T) {
	client := NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        true,
	})

	job := client.buildRebootJob("worker-1", "", "boot-id-abc")
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, guardedCommand(
		"chroot /host logger -t nvsentinel-reboot -p daemon.notice \"NVSentinel reboot: node=worker-1\" || true; chroot /host reboot",
	), container.Command)
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "host-root", job.Spec.Template.Spec.Volumes[0].Name)
}

func TestSendRebootSignal_EmptyBootID(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "", true)

	_, err := client.SendRebootSignal(ctx, node, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no bootID")

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items, "no reboot Job may be created without a boot ID to guard it")
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
		Name:      "reboot-worker-1-abc12",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
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

// staleRebootJob returns the Job and pod an earlier RebootNode attempt leaves behind
// when the janitor gives up on it: the pod is scheduled but its image never pulled.
func staleRebootJob(nodeName string) (*batchv1.Job, *corev1.Pod) {
	labels := map[string]string{jobLabelKey: "true", jobNodeLabelKey: nodeName}
	job := &batchv1.Job{Name: "reboot-" + nodeName + "-old12", Namespace: "test-ns", Labels: labels}
	pod := &corev1.Pod{
		Name: job.Name + "-abcde", Namespace: "test-ns", Labels: labels,
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}},
		},
	}

	return job, pod
}

func TestSendRebootSignal_RemovesJobOfEarlierAttempt(t *testing.T) {
	job, pod := staleRebootJob("worker-1")
	client := newTestClient(job, pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "rebootnode-2")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("boot-id-abc"), ref)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1, "only the new attempt's Job must remain")
	assert.NotEqual(t, job.Name, jobs.Items[0].Name)
}

func TestIsNodeReady_TerminalPodError_RemovesJob(t *testing.T) {
	job, pod := staleRebootJob("worker-1")
	job.UID = "job-uid"
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID}}
	client := newTestClient(job, pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	_, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ImagePullBackOff")

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items, "the Job whose pod cannot start must not outlive the failed attempt")
}

func TestIsNodeReady_IgnoresPodOfDeletedJob(t *testing.T) {
	// The earlier attempt's Job is gone, its pod is still listed while the garbage collector
	// has not removed it (no deletionTimestamp yet), and the new attempt's Job exists.
	_, oldPod := staleRebootJob("worker-1")
	oldPod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "reboot-worker-1-old12", UID: "old-job-uid"}}
	newJob := &batchv1.Job{
		Name: "reboot-worker-1-new34", Namespace: "test-ns", UID: "new-job-uid",
		Labels: map[string]string{jobLabelKey: "true", jobNodeLabelKey: "worker-1"},
	}

	client := newTestClient(oldPod, newJob)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-abc")
	require.NoError(t, err, "the earlier attempt's pod must not fail the current one")
	assert.False(t, ready)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1, "the current attempt's Job must survive")
	assert.Equal(t, "reboot-worker-1-new34", jobs.Items[0].Name)
}

func TestSendRebootSignal_ThenIsNodeReady_IgnoresPodOfRemovedJob(t *testing.T) {
	// Background deletion removes the Job at once but its pod only later; the next
	// readiness check for the new attempt must not read that pod.
	job, pod := staleRebootJob("worker-1")
	job.UID = "old-job-uid"
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID}}

	client := newTestClient(job, pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-abc", true)

	ref, err := client.SendRebootSignal(ctx, node, "rebootnode-2")
	require.NoError(t, err)

	ready, err := client.IsNodeReady(ctx, node, string(ref))
	require.NoError(t, err, "the removed Job's pod must not fail the new attempt")
	assert.False(t, ready)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1, "the new attempt's Job must survive")
	assert.NotEqual(t, job.Name, jobs.Items[0].Name)
}

func TestIsNodeReady_ImagePullBackOff(t *testing.T) {
	pod := &corev1.Pod{
		Name:      "reboot-worker-1-pod",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
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
		Name:      "reboot-worker-1-pod",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
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
		Name:      "reboot-worker-1-pod",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
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
		Name:      "reboot-worker-1-pod",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
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

	_, err := client.SendRebootSignal(ctx, node, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create reboot job")
}

func TestBuildRebootJob_UsesGenerateName(t *testing.T) {
	client := newTestClient()
	job := client.buildRebootJob("worker-1", "", "boot-id-abc")

	assert.Empty(t, job.Name)
	assert.Equal(t, "reboot-worker-1-", job.GenerateName)
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		config := loadConfigFromEnv()
		assert.Equal(t, defaultRebootImage, config.RebootImage)
		assert.False(t, config.UseSysrqReboot)
		assert.Equal(t, "", config.RebootJobNamespace)
		assert.Equal(t, int32(defaultRebootJobTTLSeconds), config.RebootJobTTL)
		assert.False(t, config.WriteSyslog)
	})

	t.Run("custom values", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_IMAGE", "custom-image:latest")
		t.Setenv("GENERIC_REBOOT_USE_SYSRQ", "true")
		t.Setenv("GENERIC_REBOOT_JOB_NAMESPACE", "custom-ns")
		t.Setenv("GENERIC_REBOOT_JOB_TTL", "7200")
		t.Setenv("GENERIC_REBOOT_IMAGE_PULL_SECRETS", "secret-a, secret-b")
		t.Setenv("GENERIC_REBOOT_WRITE_SYSLOG", "true")

		config := loadConfigFromEnv()
		assert.Equal(t, "custom-image:latest", config.RebootImage)
		assert.True(t, config.UseSysrqReboot)
		assert.Equal(t, "custom-ns", config.RebootJobNamespace)
		assert.Equal(t, int32(7200), config.RebootJobTTL)
		assert.Equal(t, []string{"secret-a", "secret-b"}, config.RebootJobPullSecrets)
		assert.True(t, config.WriteSyslog)
	})

	t.Run("invalid sysrq flag uses default", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_USE_SYSRQ", "not-a-bool")

		config := loadConfigFromEnv()
		assert.False(t, config.UseSysrqReboot)
	})

	t.Run("invalid TTL uses default", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_JOB_TTL", "not-a-number")

		config := loadConfigFromEnv()
		assert.Equal(t, int32(defaultRebootJobTTLSeconds), config.RebootJobTTL)
	})

	t.Run("invalid write syslog flag uses default", func(t *testing.T) {
		t.Setenv("GENERIC_REBOOT_WRITE_SYSLOG", "not-a-bool")

		config := loadConfigFromEnv()
		assert.False(t, config.WriteSyslog)
	})
}

var rebootVariants = []struct {
	name           string
	useSysrqReboot bool
	writeSyslog    bool
}{
	{name: "chroot reboot", useSysrqReboot: false, writeSyslog: false},
	{name: "chroot reboot with syslog", useSysrqReboot: false, writeSyslog: true},
	{name: "sysrq reboot", useSysrqReboot: true, writeSyslog: false},
	{name: "sysrq reboot with syslog", useSysrqReboot: true, writeSyslog: true},
}

func TestBuildRebootJob_AllVariants_SetsPreRebootBootIDEnv(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			client := newVariantClient(v.useSysrqReboot, v.writeSyslog)

			job := client.buildRebootJob("worker-1", "rb-1", "boot-id-abc")

			container := job.Spec.Template.Spec.Containers[0]
			assert.Equal(t, []corev1.EnvVar{{Name: preRebootBootIDEnvVar, Value: "boot-id-abc"}}, container.Env)
		})
	}
}

func TestSendRebootSignal_NodeBootID_PassedToJobEnv(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()
	node := newNode("worker-1", "8f6c1f3e-0f7a-4d3b-9c55-2b1e7a9d4c10", true)

	_, err := client.SendRebootSignal(ctx, node, "rb-1")
	require.NoError(t, err)

	jobs, err := client.k8sClient.BatchV1().Jobs("test-ns").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, jobs.Items, 1)

	container := jobs.Items[0].Spec.Template.Spec.Containers[0]
	assert.Equal(t, []corev1.EnvVar{
		{Name: preRebootBootIDEnvVar, Value: "8f6c1f3e-0f7a-4d3b-9c55-2b1e7a9d4c10"},
	}, container.Env)
}

func TestRebootCommand_AllVariants_GuardPrecedesSideEffects(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			command := newVariantClient(v.useSysrqReboot, v.writeSyslog).config.rebootCommand("worker-1", "rb-1")

			require.Len(t, command, 3)
			assert.Equal(t, []string{"sh", "-c"}, command[:2])
			assert.True(t, strings.HasPrefix(command[2], bootIDGuard()),
				"the boot ID guard must run before the syslog entry and the reboot")
			assert.NotContains(t, bootIDGuard(), "logger")
			assert.NotContains(t, bootIDGuard(), "chroot")
			assert.NotContains(t, bootIDGuard(), "sysrq-trigger")
		})
	}
}

func TestRebootCommand_ShellMetacharBootID_NotInterpolated(t *testing.T) {
	const hostileBootID = `$(touch /tmp/nvsentinel-pwned)"; reboot; "`

	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			job := newVariantClient(v.useSysrqReboot, v.writeSyslog).buildRebootJob("worker-1", "rb-1", hostileBootID)

			container := job.Spec.Template.Spec.Containers[0]
			assert.NotContains(t, container.Command[2], hostileBootID)
			assert.Equal(t, []corev1.EnvVar{{Name: preRebootBootIDEnvVar, Value: hostileBootID}}, container.Env)
		})
	}
}

func TestRebootScript_BootIDMatches_RunsRebootAction(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "boot-id-abc",
				preRebootBootID: "boot-id-abc",
			})

			assert.Equal(t, 0, run.exitCode, run.output)
			assert.True(t, run.rebootAttempted, run.output)
			assert.Equal(t, v.writeSyslog, run.loggedToSyslog, run.output)
		})
	}
}

func TestRebootScript_BootIDChanged_DoesNotRebootAndExitsZero(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "boot-id-new",
				preRebootBootID: "boot-id-abc",
			})

			assert.Equal(t, 0, run.exitCode, run.output)
			assert.False(t, run.rebootAttempted, run.output)
			assert.False(t, run.loggedToSyslog, run.output)
			assert.Contains(t, run.output, "skipping reboot")
		})
	}
}

func TestRebootScript_BootIDUnreadable_DoesNotRebootAndExitsNonZero(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				missingBootIDFile: true,
				preRebootBootID:   "boot-id-abc",
			})

			assert.NotEqual(t, 0, run.exitCode, run.output)
			assert.False(t, run.rebootAttempted, run.output)
			assert.False(t, run.loggedToSyslog, run.output)
		})
	}
}

func TestRebootScript_EmptyHostBootID_DoesNotRebootAndExitsNonZero(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "",
				preRebootBootID: "boot-id-abc",
			})

			assert.NotEqual(t, 0, run.exitCode, run.output)
			assert.False(t, run.rebootAttempted, run.output)
			assert.False(t, run.loggedToSyslog, run.output)
		})
	}
}

func TestRebootScript_EmptyPreRebootBootID_DoesNotRebootAndExitsNonZero(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "boot-id-abc",
				preRebootBootID: "",
			})

			assert.NotEqual(t, 0, run.exitCode, run.output)
			assert.False(t, run.rebootAttempted, run.output)
			assert.False(t, run.loggedToSyslog, run.output)
		})
	}
}

func TestRebootScript_RebootFails_ExitsNonZero(t *testing.T) {
	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "boot-id-abc",
				preRebootBootID: "boot-id-abc",
				rebootFails:     true,
			})

			assert.NotEqual(t, 0, run.exitCode, run.output)
			assert.NotContains(t, run.output, "skipping reboot")
		})
	}
}

func TestRebootScript_ShellMetacharBootID_NotExecuted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	hostileBootID := fmt.Sprintf(`$(touch %s)`, marker)

	for _, v := range rebootVariants {
		t.Run(v.name, func(t *testing.T) {
			run := runRebootScript(t, v.useSysrqReboot, v.writeSyslog, scriptEnv{
				hostBootID:      "boot-id-new",
				preRebootBootID: hostileBootID,
			})

			assert.Equal(t, 0, run.exitCode, run.output)
			assert.False(t, run.rebootAttempted, run.output)
			assert.NoFileExists(t, marker)
		})
	}
}

func TestIsNodeReady_GuardedPodSucceededAndBootIDChanged_ReturnsTrue(t *testing.T) {
	pod := &corev1.Pod{
		Name:      "reboot-worker-1-pod",
		Namespace: "test-ns",
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: "worker-1",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
					},
				},
			},
		},
	}

	client := newTestClient(pod)
	ctx := context.Background()
	node := newNode("worker-1", "boot-id-new", true)

	ready, err := client.IsNodeReady(ctx, node, "boot-id-old")
	require.NoError(t, err)
	assert.True(t, ready)
}

func guardedCommand(action string) []string {
	return []string{"sh", "-c", bootIDGuard() + action}
}

func newVariantClient(useSysrqReboot, writeSyslog bool) *Client {
	return NewClientWithK8s(fake.NewSimpleClientset(), Config{
		RebootImage:        "busybox:1.37",
		UseSysrqReboot:     useSysrqReboot,
		RebootJobNamespace: "test-ns",
		RebootJobTTL:       3600,
		WriteSyslog:        writeSyslog,
	})
}

type scriptEnv struct {
	hostBootID        string
	missingBootIDFile bool
	preRebootBootID   string
	rebootFails       bool
}

type scriptRun struct {
	exitCode        int
	output          string
	rebootAttempted bool
	loggedToSyslog  bool
}

// runRebootScript runs the reboot Job's real container script with sh. A stub chroot on
// PATH records its arguments instead of touching the host, and the boot ID file and
// sysrq-trigger paths point into a temp dir.
func runRebootScript(t *testing.T, useSysrqReboot, writeSyslog bool, env scriptEnv) scriptRun {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))

	chrootCalls := filepath.Join(dir, "chroot-calls")
	stub := "#!/bin/sh\n" +
		`echo "$*" >> "$CHROOT_CALLS"` + "\n" +
		`if [ "$2" = reboot ] && [ -n "$REBOOT_FAILS" ]; then exit 1; fi` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "chroot"), []byte(stub), 0o755))

	bootIDFile := filepath.Join(dir, "boot_id")
	if !env.missingBootIDFile {
		require.NoError(t, os.WriteFile(bootIDFile, []byte(env.hostBootID+"\n"), 0o644))
	}

	sysrqFile := filepath.Join(dir, "sysrq-trigger")
	if env.rebootFails {
		sysrqFile = filepath.Join(dir, "missing-dir", "sysrq-trigger")
	}

	job := newVariantClient(useSysrqReboot, writeSyslog).buildRebootJob("worker-1", "rb-1", env.preRebootBootID)
	container := job.Spec.Template.Spec.Containers[0]
	require.Equal(t, []string{"sh", "-c"}, container.Command[:2])

	script := strings.NewReplacer(
		bootIDPath, bootIDFile,
		hostProcMountPath+"/sysrq-trigger", sysrqFile,
	).Replace(container.Command[2])

	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CHROOT_CALLS=" + chrootCalls,
	}

	for _, e := range container.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}

	if env.rebootFails {
		cmd.Env = append(cmd.Env, "REBOOT_FAILS=1")
	}

	out, err := cmd.CombinedOutput()

	run := scriptRun{output: string(out)}

	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "script did not run: %s", out)

		run.exitCode = exitErr.ExitCode()
	}

	calls, err := os.ReadFile(chrootCalls)
	if err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	sysrq, err := os.ReadFile(sysrqFile)
	if err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	run.rebootAttempted = strings.Contains(string(calls), hostMountPath+" reboot") || string(sysrq) == "b\n"
	run.loggedToSyslog = strings.Contains(string(calls), "logger")

	return run
}
