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
	"context"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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



// TestInjectInitContainers covers the main entry point: GPU detection,
// gang context extraction, init container / volume patch generation,
// and append-vs-create behavior for existing init containers and volumes.
func TestInjectInitContainers(t *testing.T) {
	tests := []struct {
		name             string
		cfg              *config.Config
		discoverer       *mockDiscoverer
		pod              *corev1.Pod
		expectPatches    bool
		expectGangCtx    bool
		expectGangID     string
		validatePatches  func(t *testing.T, patches []PatchOperation)
	}{
		{
			name:          "no GPU resources returns nil",
			cfg:           testConfig(),
			pod:           &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}},
			expectPatches: false,
			expectGangCtx: false,
		},
		{
			name:          "GPU pod with gang disabled",
			cfg:           testConfig(),
			discoverer:    nil,
			pod:           gpuPod(),
			expectPatches: true,
			expectGangCtx: false,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				initPatch := findPatchByPath(patches, "/spec/initContainers")
				require.NotNil(t, initPatch, "expected /spec/initContainers patch")
				assert.Equal(t, "add", initPatch.Op)

				containers, ok := initPatch.Value.([]corev1.Container)
				require.True(t, ok)
				assert.Len(t, containers, 1)
				assert.Equal(t, "preflight-dcgm-diag", containers[0].Name)

			// No gang volumes should be present (check both single-volume
			// appends and the initial slice-valued /spec/volumes patch).
			for _, p := range patches {
				switch v := p.Value.(type) {
				case corev1.Volume:
					assert.NotEqual(t, types.GangConfigVolumeName, v.Name)
					assert.NotEqual(t, dshmVolumeName, v.Name)
				case []corev1.Volume:
					for _, vol := range v {
						assert.NotEqual(t, types.GangConfigVolumeName, vol.Name)
						assert.NotEqual(t, dshmVolumeName, vol.Name)
					}
				}
			}
			},
		},
		{
			name:          "GPU pod with gang enabled but not a gang member",
			cfg:           testGangConfig(),
			discoverer:    &mockDiscoverer{name: "test", canHandle: false},
			pod:           gpuPod(),
			expectPatches: true,
			expectGangCtx: false,
		},
		{
			name:       "GPU pod with gang enabled and is gang member",
			cfg:        testGangConfig(),
			discoverer: &mockDiscoverer{name: "volcano", canHandle: true, gangID: "volcano-default-pg1"},
			pod:        gpuEFAPod(),
			expectPatches: true,
			expectGangCtx: true,
			expectGangID:  "volcano-default-pg1",
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				initPatch := findPatchByPath(patches, "/spec/initContainers")
				require.NotNil(t, initPatch)
				containers, ok := initPatch.Value.([]corev1.Container)
				require.True(t, ok)
				require.Len(t, containers, 1)

				c := containers[0]
				assert.True(t, hasEnvVar(c, "GANG_ID"))
				assert.True(t, hasEnvVar(c, "GANG_CONFIG_DIR"))
				assert.True(t, hasEnvVar(c, "GANG_TIMEOUT_SECONDS"))
				assert.True(t, hasEnvVar(c, "MASTER_PORT"))
				assert.True(t, hasEnvVar(c, "POD_NAME"))
				assert.True(t, hasEnvVar(c, "POD_IP"))
				assert.True(t, hasVolumeMount(c, types.GangConfigVolumeName))
				assert.True(t, hasVolumeMount(c, dshmVolumeName))
			},
		},
		{
			name: "GPU pod with gang enabled but nil discoverer",
			cfg:  testGangConfig(),
			discoverer: nil,
			pod:        gpuPod(),
			expectPatches: true,
			expectGangCtx: false,
		},
		{
			name: "existing init containers are appended not replaced",
			cfg:  testConfig(),
			pod: func() *corev1.Pod {
				p := gpuPod()
				p.Spec.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox"}}
				return p
			}(),
			expectPatches: true,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				assert.Nil(t, findPatchByPath(patches, "/spec/initContainers"), "should not replace init containers array")
				appendCount := countPatchesByPath(patches, "/spec/initContainers/-")
				assert.Equal(t, 1, appendCount, "should append 1 init container")
			},
		},
		{
			name:          "no existing init containers creates array",
			cfg:           testConfig(),
			pod:           gpuPod(),
			expectPatches: true,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				p := findPatchByPath(patches, "/spec/initContainers")
				require.NotNil(t, p, "should create init containers array")
				assert.Equal(t, "add", p.Op)
			},
		},
		{
			name: "existing volumes are not duplicated",
			cfg:  testGangConfig(),
			discoverer: &mockDiscoverer{name: "test", canHandle: true, gangID: "test-gang"},
			pod: func() *corev1.Pod {
				p := gpuPod()
				p.Spec.Volumes = []corev1.Volume{
					{Name: dshmVolumeName},
					{Name: types.GangConfigVolumeName},
				}
				return p
			}(),
			expectPatches: true,
			expectGangCtx: true,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				for _, p := range patches {
					if p.Path == "/spec/volumes/-" {
						if vol, ok := p.Value.(corev1.Volume); ok {
							assert.NotEqual(t, dshmVolumeName, vol.Name, "should not duplicate dshm")
							assert.NotEqual(t, types.GangConfigVolumeName, vol.Name, "should not duplicate gang config")
						}
					}
				}
			},
		},
		{
			name: "existing volumes appended with /-",
			cfg:  testConfig(),
			pod: func() *corev1.Pod {
				p := gpuPod()
				p.Spec.Volumes = []corev1.Volume{{Name: "user-vol"}}
				return p
			}(),
			expectPatches: true,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				assert.Nil(t, findPatchByPath(patches, "/spec/volumes"), "should not replace volumes array")
				assert.Greater(t, countPatchesByPath(patches, "/spec/volumes/-"), 0, "should append volumes")
			},
		},
		{
			name:          "empty volumes creates array",
			cfg:           testConfig(),
			pod:           gpuPod(),
			expectPatches: true,
			validatePatches: func(t *testing.T, patches []PatchOperation) {
				t.Helper()
				p := findPatchByPath(patches, "/spec/volumes")
				require.NotNil(t, p, "should create volumes array")
				assert.Equal(t, "add", p.Op)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var disc gang.GangDiscoverer
			if tt.discoverer != nil {
				disc = tt.discoverer
			}
			injector := NewInjector(tt.cfg, disc)

			patches, gangCtx, err := injector.InjectInitContainers(tt.pod)
			require.NoError(t, err)

			if tt.expectPatches {
				assert.NotEmpty(t, patches)
			} else {
				assert.Empty(t, patches)
			}

			if tt.expectGangCtx {
				require.NotNil(t, gangCtx)
				if tt.expectGangID != "" {
					assert.Equal(t, tt.expectGangID, gangCtx.GangID)
					assert.NotEmpty(t, gangCtx.ConfigMapName)
				}
			} else {
				assert.Nil(t, gangCtx)
			}

			if tt.validatePatches != nil && len(patches) > 0 {
				tt.validatePatches(t, patches)
			}
		})
	}
}

// TestBuildInitContainers covers per-container logic: resource mirroring,
// CPU/memory floor defaults, env injection ordering (common > DCGM > gang > user),
// user env/mount inheritance, and DRA claim mirroring.
func TestBuildInitContainers(t *testing.T) {
	t.Run("resources mirrored to init containers", func(t *testing.T) {
		cfg := testConfig()
		injector := NewInjector(cfg, nil)
		pod := gpuEFAPod()

		maxResources := corev1.ResourceList{
			"nvidia.com/gpu":        resource.MustParse("8"),
			"vpc.amazonaws.com/efa": resource.MustParse("4"),
		}

		containers := injector.buildInitContainers(pod, maxResources, nil)
		require.Len(t, containers, 1)

		c := containers[0]
		assert.Equal(t, resource.MustParse("8"), c.Resources.Requests["nvidia.com/gpu"])
		assert.Equal(t, resource.MustParse("8"), c.Resources.Limits["nvidia.com/gpu"])
		assert.Equal(t, resource.MustParse("4"), c.Resources.Requests["vpc.amazonaws.com/efa"])
		assert.Equal(t, resource.MustParse("4"), c.Resources.Limits["vpc.amazonaws.com/efa"])
	})

	t.Run("CPU and memory floor applied when not set", func(t *testing.T) {
		cfg := testConfig()
		injector := NewInjector(cfg, nil)
		pod := gpuPod()

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)

		assert.Equal(t, resource.MustParse("100m"), containers[0].Resources.Requests[corev1.ResourceCPU])
		assert.Equal(t, resource.MustParse("500Mi"), containers[0].Resources.Requests[corev1.ResourceMemory])
	})

	t.Run("CPU and memory floor not applied when already set", func(t *testing.T) {
		cfg := testConfig()
		cfg.InitContainers = []corev1.Container{
			{
				Name:  "preflight-dcgm-diag",
				Image: "dcgm:latest",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
		}
		injector := NewInjector(cfg, nil)

		containers := injector.buildInitContainers(gpuPod(), corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)

		assert.Equal(t, resource.MustParse("200m"), containers[0].Resources.Requests[corev1.ResourceCPU])
		assert.Equal(t, resource.MustParse("1Gi"), containers[0].Resources.Requests[corev1.ResourceMemory])
	})

	t.Run("DCGM env only for dcgm-diag container", func(t *testing.T) {
		cfg := testConfig()
		cfg.InitContainers = []corev1.Container{
			{Name: "preflight-dcgm-diag", Image: "dcgm:latest"},
			{Name: "preflight-nccl-allreduce", Image: "nccl:latest"},
		}
		injector := NewInjector(cfg, nil)

		containers := injector.buildInitContainers(gpuPod(), corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 2)

		assert.True(t, hasEnvVar(containers[0], "DCGM_DIAG_LEVEL"), "dcgm-diag should have DCGM_DIAG_LEVEL")
		assert.True(t, hasEnvVar(containers[0], "DCGM_HOSTENGINE_ADDR"), "dcgm-diag should have DCGM_HOSTENGINE_ADDR")
		assert.False(t, hasEnvVar(containers[1], "DCGM_DIAG_LEVEL"), "nccl container should NOT have DCGM_DIAG_LEVEL")
		assert.False(t, hasEnvVar(containers[1], "DCGM_HOSTENGINE_ADDR"), "nccl container should NOT have DCGM_HOSTENGINE_ADDR")
	})

	t.Run("common env injected", func(t *testing.T) {
		cfg := testConfig()
		injector := NewInjector(cfg, nil)

		containers := injector.buildInitContainers(gpuPod(), corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)

		assert.True(t, hasEnvVar(containers[0], "NODE_NAME"))
		assert.True(t, hasEnvVar(containers[0], "PLATFORM_CONNECTOR_SOCKET"))
		assert.True(t, hasEnvVar(containers[0], "PROCESSING_STRATEGY"))
	})

	t.Run("gang env not injected without context", func(t *testing.T) {
		cfg := testGangConfig()
		injector := NewInjector(cfg, nil)

		containers := injector.buildInitContainers(gpuPod(), corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)

		assert.False(t, hasEnvVar(containers[0], "GANG_ID"))
		assert.False(t, hasEnvVar(containers[0], "GANG_CONFIG_DIR"))
		assert.False(t, hasEnvVar(containers[0], "POD_NAME"))
		assert.False(t, hasEnvVar(containers[0], "POD_IP"))
	})

	t.Run("gang env injected with context", func(t *testing.T) {
		cfg := testGangConfig()
		injector := NewInjector(cfg, nil)

		gangCtx := &GangContext{GangID: "test-gang", ConfigMapName: "preflight-test-gang"}
		containers := injector.buildInitContainers(gpuPod(), corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, gangCtx)
		require.Len(t, containers, 1)

		c := containers[0]
		assert.Equal(t, "test-gang", findEnv(c.Env, "GANG_ID"))
		assert.Equal(t, "/etc/preflight", findEnv(c.Env, "GANG_CONFIG_DIR"))
		assert.Equal(t, "600", findEnv(c.Env, "GANG_TIMEOUT_SECONDS"))
		assert.Equal(t, "29500", findEnv(c.Env, "MASTER_PORT"))
		assert.True(t, hasEnvVar(c, "POD_NAME"))
		assert.True(t, hasEnvVar(c, "POD_IP"))
	})

	t.Run("user NCCL env inherited", func(t *testing.T) {
		cfg := testConfig()
		cfg.NCCLEnvPatterns = []string{"NCCL_*"}
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "NCCL_DEBUG", Value: "INFO"},
		}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)
		assert.Equal(t, "INFO", findEnv(containers[0].Env, "NCCL_DEBUG"))
	})

	t.Run("user env lower precedence than template", func(t *testing.T) {
		cfg := testConfig()
		cfg.NCCLEnvPatterns = []string{"NCCL_*"}
		cfg.InitContainers = []corev1.Container{
			{
				Name:  "preflight-dcgm-diag",
				Image: "dcgm:latest",
				Env:   []corev1.EnvVar{{Name: "NCCL_DEBUG", Value: "WARN"}},
			},
		}
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "NCCL_DEBUG", Value: "INFO"},
		}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)
		assert.Equal(t, "WARN", findEnv(containers[0].Env, "NCCL_DEBUG"))
	})

	t.Run("user volume mounts inherited", func(t *testing.T) {
		cfg := testConfig()
		cfg.VolumeMountPatterns = []string{"nvtcpxo-*"}
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "nvtcpxo-libraries", MountPath: "/usr/local/nvidia"},
		}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)
		assert.True(t, hasVolumeMount(containers[0], "nvtcpxo-libraries"))
	})

	t.Run("DRA claims mirrored when enabled", func(t *testing.T) {
		cfg := testGangConfig()
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
			{Name: "gpu-claim"},
			{Name: "rdma-claim"},
		}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, gangCtx)
		require.Len(t, containers, 1)
		require.Len(t, containers[0].Resources.Claims, 2)
		assert.Equal(t, "gpu-claim", containers[0].Resources.Claims[0].Name)
		assert.Equal(t, "rdma-claim", containers[0].Resources.Claims[1].Name)
	})

	t.Run("DRA claims not mirrored when disabled", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.MirrorResourceClaims = boolPtr(false)
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
			{Name: "gpu-claim"},
		}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, gangCtx)
		require.Len(t, containers, 1)
		assert.Empty(t, containers[0].Resources.Claims)
	})

	t.Run("DRA claims not mirrored without gang context", func(t *testing.T) {
		cfg := testGangConfig()
		injector := NewInjector(cfg, nil)

		pod := gpuPod()
		pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
			{Name: "gpu-claim"},
		}

		containers := injector.buildInitContainers(pod, corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}, nil)
		require.Len(t, containers, 1)
		assert.Empty(t, containers[0].Resources.Claims)
	})
}

// extractVolumes pulls the []corev1.Volume out of the first /spec/volumes
// or /spec/volumes/- patch, failing the test if none is found.
func extractVolumes(t *testing.T, patches []PatchOperation) []corev1.Volume {
	t.Helper()
	p := findPatchByPath(patches, "/spec/volumes")
	require.NotNil(t, p, "expected /spec/volumes patch")
	volumes, ok := p.Value.([]corev1.Volume)
	require.True(t, ok, "patch value should be []corev1.Volume")
	return volumes
}

// findVolume returns the first volume with the given name, or nil.
func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

// requireVolume returns the volume with the given name, failing if absent.
func requireVolume(t *testing.T, volumes []corev1.Volume, name string) *corev1.Volume {
	t.Helper()
	vol := findVolume(volumes, name)
	require.NotNil(t, vol, "expected volume %q", name)
	return vol
}

// TestInjectVolumes covers volume patch generation: nvsentinel socket,
// gang ConfigMap (optional), /dev/shm, NCCL topology, extra hostPaths,
// and dedup against existing pod volumes.
func TestInjectVolumes(t *testing.T) {
	t.Run("nvsentinel socket volume added", func(t *testing.T) {
		injector := &Injector{cfg: testConfig()}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, nil))
		vol := requireVolume(t, volumes, nvsentinelSocketVolumeName)
		require.NotNil(t, vol.HostPath)
		assert.Equal(t, "/var/run/nvsentinel", vol.HostPath.Path)
	})

	t.Run("nvsentinel socket volume skipped if exists", func(t *testing.T) {
		injector := &Injector{cfg: testConfig()}
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: nvsentinelSocketVolumeName}},
			},
		}

		patches := injector.injectVolumes(pod, nil)
		for _, p := range patches {
			if vol, ok := p.Value.(corev1.Volume); ok {
				assert.NotEqual(t, nvsentinelSocketVolumeName, vol.Name)
			}
		}
	})

	t.Run("gang ConfigMap volume is optional", func(t *testing.T) {
		injector := &Injector{cfg: testGangConfig()}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		vol := requireVolume(t, volumes, types.GangConfigVolumeName)
		require.NotNil(t, vol.ConfigMap)
		require.NotNil(t, vol.ConfigMap.Optional)
		assert.True(t, *vol.ConfigMap.Optional)
		assert.Equal(t, "preflight-test", vol.ConfigMap.Name)
	})

	t.Run("dshm volume specs", func(t *testing.T) {
		injector := &Injector{cfg: testGangConfig()}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		vol := requireVolume(t, volumes, dshmVolumeName)
		require.NotNil(t, vol.EmptyDir)
		assert.Equal(t, corev1.StorageMediumMemory, vol.EmptyDir.Medium)
		assert.Equal(t, resource.MustParse("64Gi"), *vol.EmptyDir.SizeLimit)
	})

	t.Run("NCCL topo volume added when configured", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.NCCLTopoConfigMap = "nccl-topo-ndv4"
		injector := &Injector{cfg: cfg}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		vol := requireVolume(t, volumes, ncclTopoVolumeName)
		require.NotNil(t, vol.ConfigMap)
		assert.Equal(t, "nccl-topo-ndv4", vol.ConfigMap.Name)
		require.NotNil(t, vol.ConfigMap.Optional)
		assert.True(t, *vol.ConfigMap.Optional)
	})

	t.Run("NCCL topo volume not added when not configured", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.NCCLTopoConfigMap = ""
		injector := &Injector{cfg: cfg}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		assert.Nil(t, findVolume(volumes, ncclTopoVolumeName), "nccl-topo volume should not be present")
	})

	t.Run("extra hostPath volumes added", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.ExtraHostPathMounts = []config.HostPathMount{
			{Name: "host-efa", HostPath: "/opt/amazon/efa", MountPath: "/opt/amazon/efa", HostPathType: "Directory"},
		}
		injector := &Injector{cfg: cfg}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		vol := requireVolume(t, volumes, "host-efa")
		require.NotNil(t, vol.HostPath)
		assert.Equal(t, "/opt/amazon/efa", vol.HostPath.Path)
	})

	t.Run("extra hostPath with invalid type skipped", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.ExtraHostPathMounts = []config.HostPathMount{
			{Name: "bad-type", HostPath: "/bad", MountPath: "/bad", HostPathType: "Bogus"},
			{Name: "good-type", HostPath: "/good", MountPath: "/good", HostPathType: "Directory"},
		}
		injector := &Injector{cfg: cfg}
		gangCtx := &GangContext{GangID: "test", ConfigMapName: "preflight-test"}

		volumes := extractVolumes(t, injector.injectVolumes(&corev1.Pod{}, gangCtx))
		assert.Nil(t, findVolume(volumes, "bad-type"), "bogus hostPathType should be skipped")
		assert.NotNil(t, findVolume(volumes, "good-type"), "valid hostPath should be included")
	})
}

// TestParseHostPathType covers all 7 valid K8s HostPathType values,
// empty string (nil), and invalid strings (rejected).
func TestParseHostPathType(t *testing.T) {
	tests := []struct {
		input   string
		wantNil bool
		wantOk  bool
		wantVal corev1.HostPathType
	}{
		{input: "", wantNil: true, wantOk: true},
		{input: "Directory", wantOk: true, wantVal: corev1.HostPathDirectory},
		{input: "DirectoryOrCreate", wantOk: true, wantVal: corev1.HostPathDirectoryOrCreate},
		{input: "File", wantOk: true, wantVal: corev1.HostPathFile},
		{input: "FileOrCreate", wantOk: true, wantVal: corev1.HostPathFileOrCreate},
		{input: "Socket", wantOk: true, wantVal: corev1.HostPathSocket},
		{input: "CharDevice", wantOk: true, wantVal: corev1.HostPathCharDev},
		{input: "BlockDevice", wantOk: true, wantVal: corev1.HostPathBlockDev},
		{input: "Bogus", wantOk: false},
		{input: "directory", wantOk: false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := parseHostPathType(tt.input)
			assert.Equal(t, tt.wantOk, ok)
			if tt.wantNil {
				assert.Nil(t, got)
			} else if tt.wantOk {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantVal, *got)
			}
		})
	}
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


type mockDiscoverer struct {
	name      string
	canHandle bool
	gangID    string
}

func (m *mockDiscoverer) Name() string                       { return m.name }
func (m *mockDiscoverer) CanHandle(_ *corev1.Pod) bool       { return m.canHandle }
func (m *mockDiscoverer) ExtractGangID(_ *corev1.Pod) string { return m.gangID }
func (m *mockDiscoverer) DiscoverPeers(_ context.Context, _ *corev1.Pod) (*types.GangInfo, error) {
	return nil, nil
}

func boolPtr(b bool) *bool { return &b }

func testConfig() *config.Config {
	return &config.Config{
		FileConfig: config.FileConfig{
			InitContainers: []corev1.Container{
				{Name: "preflight-dcgm-diag", Image: "nvcr.io/nvidia/dcgm:latest"},
			},
			GPUResourceNames:     []string{"nvidia.com/gpu"},
			NetworkResourceNames: []string{"vpc.amazonaws.com/efa"},
			DCGM: config.DCGMConfig{
				HostengineAddr:     "localhost:5555",
				DiagLevel:          1,
				ConnectorSocket:    "/var/run/nvsentinel/nvsentinel.sock",
				ProcessingStrategy: "EXECUTE_REMEDIATION",
			},
		},
	}
}

func testGangConfig() *config.Config {
	cfg := testConfig()
	cfg.GangCoordination = config.GangCoordinationConfig{
		Enabled:            true,
		Timeout:            "10m",
		TimeoutDuration:    10 * time.Minute,
		MasterPort:         29500,
		ConfigMapMountPath: "/etc/preflight",
		MirrorResourceClaims: boolPtr(true),
	}
	return cfg
}

func gpuPod() *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "train",
					Image: "training:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
		},
	}
}

func gpuEFAPod() *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "train",
					Image: "training:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"nvidia.com/gpu":        resource.MustParse("8"),
							"vpc.amazonaws.com/efa": resource.MustParse("4"),
						},
					},
				},
			},
		},
	}
}

func findPatchByPath(patches []PatchOperation, path string) *PatchOperation {
	for i := range patches {
		if patches[i].Path == path {
			return &patches[i]
		}
	}
	return nil
}

func countPatchesByPath(patches []PatchOperation, path string) int {
	count := 0
	for _, p := range patches {
		if p.Path == path {
			count++
		}
	}
	return count
}

func hasEnvVar(container corev1.Container, name string) bool {
	for _, e := range container.Env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasVolumeMount(container corev1.Container, name string) bool {
	for _, vm := range container.VolumeMounts {
		if vm.Name == name {
			return true
		}
	}
	return false
}