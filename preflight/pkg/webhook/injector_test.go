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

package webhook

import (
	"testing"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCollectMatchingEnvVars(t *testing.T) {
	injector := &Injector{cfg: &config.Config{
		FileConfig: config.FileConfig{
			NCCLEnvPatterns: []string{"NCCL_*", "FI_*", "LD_LIBRARY_PATH"},
		},
	}}

	containers := []corev1.Container{
		{Env: []corev1.EnvVar{
			{Name: "NCCL_DEBUG", Value: "INFO"},
			{Name: "FI_PROVIDER", Value: "efa"},
			{Name: "HOME", Value: "/root"}, // should NOT match
		}},
		{Env: []corev1.EnvVar{
			{Name: "NCCL_DEBUG", Value: "WARN"}, // duplicate — first wins
			{Name: "NCCL_SOCKET_IFNAME", Value: "eth0"},
		}},
	}

	result := injector.collectMatchingEnvVars(containers)

	assert.Len(t, result, 3) // NCCL_DEBUG, FI_PROVIDER, NCCL_SOCKET_IFNAME
	assert.Equal(t, "INFO", findEnv(result, "NCCL_DEBUG"))
	assert.Equal(t, "", findEnv(result, "HOME"))
}

func TestCollectMatchingVolumeMounts(t *testing.T) {
	injector := &Injector{cfg: &config.Config{
		FileConfig: config.FileConfig{
			VolumeMountPatterns: []string{"nvtcpxo-*", "host-opt-*"},
		},
	}}

	containers := []corev1.Container{
		{VolumeMounts: []corev1.VolumeMount{
			{Name: "nvtcpxo-libraries", MountPath: "/usr/local/nvidia"},
			{Name: "kube-api-access", MountPath: "/var/run/secrets"},
		}},
		{VolumeMounts: []corev1.VolumeMount{
			{Name: "nvtcpxo-libraries", MountPath: "/usr/local/nvidia"}, // dup
			{Name: "host-opt-amazon", MountPath: "/opt/amazon"},
		}},
	}

	result := injector.collectMatchingVolumeMounts(containers)

	assert.Len(t, result, 2)
	assert.Equal(t, "nvtcpxo-libraries", result[0].Name)
	assert.Equal(t, "host-opt-amazon", result[1].Name)
}

func TestMergeEnvVars_ExistingTakesPrecedence(t *testing.T) {
	injector := &Injector{cfg: &config.Config{}}
	container := &corev1.Container{
		Env: []corev1.EnvVar{{Name: "NCCL_DEBUG", Value: "WARN"}},
	}

	injector.mergeEnvVars(container, []corev1.EnvVar{
		{Name: "NCCL_DEBUG", Value: "INFO"}, // should NOT override
		{Name: "FI_PROVIDER", Value: "efa"}, // should be added
	})

	assert.Len(t, container.Env, 2)
	assert.Equal(t, "WARN", findEnv(container.Env, "NCCL_DEBUG"))
}

func TestMergeVolumeMounts_SkipsExistingNameOrPath(t *testing.T) {
	injector := &Injector{cfg: &config.Config{}}
	container := &corev1.Container{
		VolumeMounts: []corev1.VolumeMount{{Name: "vol-a", MountPath: "/mnt/a"}},
	}

	injector.mergeVolumeMounts(container, []corev1.VolumeMount{
		{Name: "vol-a", MountPath: "/mnt/b"}, // skip: name exists
		{Name: "vol-b", MountPath: "/mnt/a"}, // skip: path exists
		{Name: "vol-c", MountPath: "/mnt/c"}, // add
	})

	assert.Len(t, container.VolumeMounts, 2)
	assert.Equal(t, "vol-c", container.VolumeMounts[1].Name)
}

func TestFindMaxResources(t *testing.T) {
	injector := &Injector{cfg: &config.Config{
		FileConfig: config.FileConfig{
			GPUResourceNames:     []string{"nvidia.com/gpu"},
			NetworkResourceNames: []string{"vpc.amazonaws.com/efa"},
		},
	}}

	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"nvidia.com/gpu":        resource.MustParse("4"),
			"vpc.amazonaws.com/efa": resource.MustParse("2"),
		}}},
		{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"nvidia.com/gpu":        resource.MustParse("8"),
			"vpc.amazonaws.com/efa": resource.MustParse("4"),
		}}},
	}}}

	result := injector.findMaxResources(pod)
	assert.Equal(t, resource.MustParse("8"), result["nvidia.com/gpu"])
	assert.Equal(t, resource.MustParse("4"), result["vpc.amazonaws.com/efa"])
}

func TestFindMaxResources_NoGPU_ReturnsNil(t *testing.T) {
	injector := &Injector{cfg: &config.Config{
		FileConfig: config.FileConfig{GPUResourceNames: []string{"nvidia.com/gpu"}},
	}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{}}}}
	assert.Nil(t, injector.findMaxResources(pod))
}

// findEnv returns the value for a named env var, or "" if not found.
func findEnv(envs []corev1.EnvVar, name string) string {
	for _, e := range envs {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
