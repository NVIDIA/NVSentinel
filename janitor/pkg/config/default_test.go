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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for index := range volumes {
		if volumes[index].Name == name {
			return &volumes[index]
		}
	}

	return nil
}

func findVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for index := range mounts {
		if mounts[index].Name == name {
			return &mounts[index]
		}
	}

	return nil
}

func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for index := range env {
		if env[index].Name == name {
			return &env[index]
		}
	}

	return nil
}

func TestGetDefaultGPUResetJobTemplate_DriverRoot_SetsEnvAndMounts(t *testing.T) {
	tests := []struct {
		name                 string
		driverRoot           string
		expectedDriverRoot   string
		expectDriverRootVol  bool
		expectedSysMountPath string
	}{
		{
			name:                 "empty driverRoot falls back to the containerized driver default",
			driverRoot:           "",
			expectedDriverRoot:   DriverRootMountPath,
			expectDriverRootVol:  true,
			expectedSysMountPath: DriverRootMountPath + HostSysPath,
		},
		{
			name:                 "custom driverRoot moves the driver and sysfs mounts",
			driverRoot:           "/opt/nvidia/driver",
			expectedDriverRoot:   "/opt/nvidia/driver",
			expectDriverRootVol:  true,
			expectedSysMountPath: "/opt/nvidia/driver/sys",
		},
		{
			name:                 "root driverRoot drops the driver volume and mounts sysfs at /sys",
			driverRoot:           ContainerDriverRoot,
			expectedDriverRoot:   ContainerDriverRoot,
			expectDriverRootVol:  false,
			expectedSysMountPath: HostSysPath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
				ResourceRequirements{}, DefaultHostDriverRootPath, test.driverRoot, "", true, "")
			require.NoError(t, err)

			podSpec := template.Spec.Template.Spec
			container := podSpec.Containers[0]

			driverRootEnv := findEnvVar(container.Env, "DRIVER_ROOT")
			require.NotNil(t, driverRootEnv)
			assert.Equal(t, test.expectedDriverRoot, driverRootEnv.Value)

			driverRootVolume := findVolume(podSpec.Volumes, DriverRootVolumeName)
			driverRootMount := findVolumeMount(container.VolumeMounts, DriverRootVolumeName)

			if test.expectDriverRootVol {
				require.NotNil(t, driverRootVolume)
				require.NotNil(t, driverRootVolume.HostPath)
				assert.Equal(t, DefaultHostDriverRootPath, driverRootVolume.HostPath.Path)
				require.NotNil(t, driverRootMount)
				assert.Equal(t, test.expectedDriverRoot, driverRootMount.MountPath)
			} else {
				assert.Nil(t, driverRootVolume)
				assert.Nil(t, driverRootMount)
			}

			sysMount := findVolumeMount(container.VolumeMounts, HostSysVolumeName)
			require.NotNil(t, sysMount)
			assert.Equal(t, test.expectedSysMountPath, sysMount.MountPath)

			// /dev is mounted at /dev wherever the chroot points.
			devMount := findVolumeMount(container.VolumeMounts, HostDevVolumeName)
			require.NotNil(t, devMount)
			assert.Equal(t, HostDevPath, devMount.MountPath)
		})
	}
}

func TestGetDefaultGPUResetJobTemplate_InvalidDriverRoot_ReturnsError(t *testing.T) {
	tests := []struct {
		name       string
		driverRoot string
	}{
		{name: "relative path", driverRoot: "run/nvidia/driver"},
		{name: "double slash", driverRoot: "//"},
		{name: "trailing slash", driverRoot: "/run/nvidia/driver/"},
		{name: "parent traversal", driverRoot: "/run/nvidia/../nvidia/driver"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
				ResourceRequirements{}, DefaultHostDriverRootPath, test.driverRoot, "", true, "")
			require.Error(t, err)
			assert.Nil(t, template)
			assert.Contains(t, err.Error(), "resetJob.driverRoot")
		})
	}
}

func TestApplyDriverBinPath_SetsEnvOnlyWhenConfigured(t *testing.T) {
	tests := []struct {
		name          string
		driverBinPath string
	}{
		{name: "unset path leaves the Job unchanged"},
		{name: "host-installed GKE driver", driverBinPath: "/home/kubernetes/bin/nvidia/bin"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
				ResourceRequirements{}, "/", DriverRootMountPath, "", true, "")
			require.NoError(t, err)

			envCountBefore := len(template.Spec.Template.Spec.Containers[0].Env)

			require.NoError(t, applyDriverBinPath(template, test.driverBinPath))

			env := template.Spec.Template.Spec.Containers[0].Env
			envVar := findEnvVar(env, DriverBinPathEnvVar)

			if test.driverBinPath == "" {
				assert.Nil(t, envVar)
				assert.Len(t, env, envCountBefore)

				return
			}

			require.NotNil(t, envVar)
			assert.Equal(t, test.driverBinPath, envVar.Value)
			assert.Len(t, env, envCountBefore+1)
		})
	}
}

func TestApplyDriverBinPath_InvalidPath_ReturnsError(t *testing.T) {
	tests := []struct {
		name          string
		driverBinPath string
	}{
		{name: "relative path", driverBinPath: "home/kubernetes/bin/nvidia/bin"},
		{name: "trailing slash", driverBinPath: "/opt/nvidia/bin/"},
		{name: "parent traversal", driverBinPath: "/opt/../nvidia/bin"},
		{name: "colon", driverBinPath: "/opt/nvidia/bin:/usr/bin"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
				ResourceRequirements{}, "/", DriverRootMountPath, "", true, "")
			require.NoError(t, err)

			err = applyDriverBinPath(template, test.driverBinPath)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "resetJob.driverBinPath")
		})
	}
}

// TestGetDefaultGPUResetJobTemplate_RunCount_LastRunReportsFailure verifies that gpu_reset.sh can count its runs:
// the container restarts in the same pod, the run count file is in an emptyDir that outlives a restart, and the
// last run that the backoff limit allows is the run that writes a failed result.
func TestGetDefaultGPUResetJobTemplate_RunCount_LastRunReportsFailure(t *testing.T) {
	for _, driverRoot := range []string{DriverRootMountPath, ContainerDriverRoot} {
		t.Run(driverRoot, func(t *testing.T) {
			template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
				ResourceRequirements{}, DefaultHostDriverRootPath, driverRoot, "", true, "")
			require.NoError(t, err)

			podSpec := template.Spec.Template.Spec
			container := podSpec.Containers[0]

			// With OnFailure, the Job controller fails the Job when the container restart count reaches the
			// backoff limit, so the limit is the number of full runs.
			assert.Equal(t, corev1.RestartPolicyOnFailure, podSpec.RestartPolicy)
			require.NotNil(t, template.Spec.BackoffLimit)
			assert.Equal(t, int32(ResetJobMaxRuns), *template.Spec.BackoffLimit)

			maxRuns := findEnvVar(container.Env, ResetMaxRunsEnvVar)
			require.NotNil(t, maxRuns)
			assert.Equal(t, "2", maxRuns.Value)

			countFile := findEnvVar(container.Env, ResetRunCountFileEnvVar)
			require.NotNil(t, countFile)
			assert.Equal(t, ResetStateMountPath+"/run-count", countFile.Value)

			stateVolume := findVolume(podSpec.Volumes, ResetStateVolumeName)
			require.NotNil(t, stateVolume)
			assert.NotNil(t, stateVolume.EmptyDir)

			stateMount := findVolumeMount(container.VolumeMounts, ResetStateVolumeName)
			require.NotNil(t, stateMount)
			assert.Equal(t, ResetStateMountPath, stateMount.MountPath)
		})
	}
}

func defaultTestJobTemplate(t *testing.T) *batchv1.JobTemplateSpec {
	t.Helper()

	template, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
		ResourceRequirements{}, DefaultHostDriverRootPath, DriverRootMountPath, "nvidia", true, "")
	require.NoError(t, err)

	return template
}

func TestApplyJobTemplatePatch_EmptyPatch_LeavesTemplateUnchanged(t *testing.T) {
	for _, patch := range []string{"", " \n"} {
		template := defaultTestJobTemplate(t)
		want := template.DeepCopy()

		patched, err := applyJobTemplatePatch(template, patch)
		require.NoError(t, err)
		assert.Equal(t, want, patched)
	}
}

// TestApplyJobTemplatePatch_ADRExample_MergesIntoTheBuiltInTemplate applies the ADR-069 example and checks that the
// built-in volumes, mounts, env vars and security context are still there, because lists merge by key.
func TestApplyJobTemplatePatch_ADRExample_MergesIntoTheBuiltInTemplate(t *testing.T) {
	template := defaultTestJobTemplate(t)
	builtIn := template.DeepCopy()

	patched, err := applyJobTemplatePatch(template, `
spec:
  activeDeadlineSeconds: 900
  template:
    spec:
      serviceAccountName: provider-reset
      volumes:
        - name: host-systemd
          hostPath:
            path: /run/systemd
      initContainers:
        - name: stop-provider-daemonsets
          image: provider/reset-hooks:1.0
      containers:
        - name: gpu-reset
          command: ["/hooks/reset-with-restore.sh"]
          volumeMounts:
            - name: host-systemd
              mountPath: /run/systemd
`)
	require.NoError(t, err)

	assert.Equal(t, int64(900), *patched.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, builtIn.Spec.BackoffLimit, patched.Spec.BackoffLimit)
	assert.Equal(t, builtIn.Spec.TTLSecondsAfterFinished, patched.Spec.TTLSecondsAfterFinished)

	podSpec := patched.Spec.Template.Spec
	assert.Equal(t, "provider-reset", podSpec.ServiceAccountName)
	assert.Equal(t, builtIn.Spec.Template.Spec.Tolerations, podSpec.Tolerations)
	assert.Equal(t, builtIn.Spec.Template.Spec.RuntimeClassName, podSpec.RuntimeClassName)

	require.Len(t, podSpec.Volumes, len(builtIn.Spec.Template.Spec.Volumes)+1)
	require.NotNil(t, findVolume(podSpec.Volumes, "host-systemd"))
	assert.Equal(t, "/run/systemd", findVolume(podSpec.Volumes, "host-systemd").HostPath.Path)

	require.Len(t, podSpec.InitContainers, 1)
	assert.Equal(t, "stop-provider-daemonsets", podSpec.InitContainers[0].Name)
	assert.Equal(t, "provider/reset-hooks:1.0", podSpec.InitContainers[0].Image)

	require.Len(t, podSpec.Containers, 1)
	container := podSpec.Containers[0]
	builtInContainer := builtIn.Spec.Template.Spec.Containers[0]
	assert.Equal(t, GPUResetContainerName, container.Name)
	assert.Equal(t, []string{"/hooks/reset-with-restore.sh"}, container.Command)
	assert.Equal(t, builtInContainer.Image, container.Image)
	assert.Equal(t, builtInContainer.Env, container.Env)
	assert.Equal(t, builtInContainer.SecurityContext, container.SecurityContext)
	assert.ElementsMatch(t, append(builtInContainer.VolumeMounts,
		corev1.VolumeMount{Name: "host-systemd", MountPath: "/run/systemd"}), container.VolumeMounts)
}

func TestApplyJobTemplatePatch_PatchValueWinsOverResetJobKey(t *testing.T) {
	patched, err := applyJobTemplatePatch(defaultTestJobTemplate(t), `
spec:
  template:
    spec:
      runtimeClassName: provider-runtime
      containers:
        - name: gpu-reset
          image: provider/gpu-reset:1.0
`)
	require.NoError(t, err)

	podSpec := patched.Spec.Template.Spec
	require.NotNil(t, podSpec.RuntimeClassName)
	assert.Equal(t, "provider-runtime", *podSpec.RuntimeClassName)
	require.Len(t, podSpec.Containers, 1)
	assert.Equal(t, "provider/gpu-reset:1.0", podSpec.Containers[0].Image)
}

// TestApplyJobTemplatePatch_Directive_IsApplied checks that a $patch directive takes effect, and that the strict
// check does not reject it as an unknown field.
func TestApplyJobTemplatePatch_Directive_IsApplied(t *testing.T) {
	template := defaultTestJobTemplate(t)
	require.NotNil(t, findEnvVar(template.Spec.Template.Spec.Containers[0].Env, UploadURLBaseEnvVar))

	patched, err := applyJobTemplatePatch(template, `
spec:
  template:
    spec:
      containers:
        - name: gpu-reset
          env:
            - name: UPLOAD_URL_BASE
              $patch: delete
`)
	require.NoError(t, err)

	env := patched.Spec.Template.Spec.Containers[0].Env
	assert.Nil(t, findEnvVar(env, UploadURLBaseEnvVar))
	assert.NotNil(t, findEnvVar(env, NodeNameEnvVar))
}

// TestApplyJobTemplatePatch_MergeRules covers the two merge rules that docs/configuration/janitor.md warns about: a
// new mountPath adds a second mount, and tolerations are replaced.
func TestApplyJobTemplatePatch_MergeRules(t *testing.T) {
	t.Run("a new mountPath for driver-root adds a second mount", func(t *testing.T) {
		patched, err := applyJobTemplatePatch(defaultTestJobTemplate(t), `
spec:
  template:
    spec:
      containers:
        - name: gpu-reset
          volumeMounts:
            - name: driver-root
              mountPath: /opt/driver
`)
		require.NoError(t, err)

		var driverRootMountPaths []string

		for _, mount := range patched.Spec.Template.Spec.Containers[0].VolumeMounts {
			if mount.Name == DriverRootVolumeName {
				driverRootMountPaths = append(driverRootMountPaths, mount.MountPath)
			}
		}

		assert.ElementsMatch(t, []string{DriverRootMountPath, "/opt/driver"}, driverRootMountPaths)
	})

	t.Run("tolerations are replaced", func(t *testing.T) {
		patched, err := applyJobTemplatePatch(defaultTestJobTemplate(t), `
spec:
  template:
    spec:
      tolerations:
        - key: provider.example.com/maintenance
          operator: Exists
          effect: NoSchedule
`)
		require.NoError(t, err)

		assert.Equal(t, []corev1.Toleration{{
			Key:      "provider.example.com/maintenance",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}}, patched.Spec.Template.Spec.Tolerations)
	})
}

func TestApplyJobTemplatePatch_InvalidPatch_ReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		patch   string
		wantErr string
	}{
		{
			name:    "misspelled field",
			patch:   "spec:\n  activeDeadlineSecond: 900\n",
			wantErr: `unknown field "spec.activeDeadlineSecond"`,
		},
		{
			name:    "misspelled list field",
			patch:   "spec:\n  template:\n    spec:\n      initContainer:\n        - name: hook\n          image: hook:1.0\n",
			wantErr: `unknown field "spec.template.spec.initContainer"`,
		},
		{
			name:    "field name in the wrong case",
			patch:   "spec:\n  ActiveDeadlineSeconds: 900\n",
			wantErr: `unknown field "spec.ActiveDeadlineSeconds"`,
		},
		{
			// The merge removes fields set to null, so only the check of the patch itself catches this one.
			name:    "misspelled field set to null",
			patch:   "spec:\n  template:\n    spec:\n      containers:\n        - name: gpu-reset\n          env:\n            - name: NODE_NAME\n              valuefrom: null\n              value: node\n",
			wantErr: `unknown field "spec.template.spec.containers[0].env[0].valuefrom"`,
		},
		{
			name:    "wrong type",
			patch:   "spec:\n  activeDeadlineSeconds: soon\n",
			wantErr: "failed to decode",
		},
		{
			name:    "duplicate field",
			patch:   "spec:\n  activeDeadlineSeconds: 900\n  activeDeadlineSeconds: 600\n",
			wantErr: "not valid YAML",
		},
		{
			name:    "not an object",
			patch:   "- spec\n",
			wantErr: "resetJob.jobTemplatePatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			patched, err := applyJobTemplatePatch(defaultTestJobTemplate(t), test.patch)
			require.Error(t, err)
			assert.Nil(t, patched)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}
