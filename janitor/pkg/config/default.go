// Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	sigsjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

const (
	GPUResetContainerName     = "gpu-reset"
	HostDevVolumeName         = "host-dev"
	HostDevPath               = "/dev"
	HostDevLogVolumeName      = "dev-log"
	HostDevLogPath            = "/run/systemd/journal/dev-log"
	DriverRootVolumeName      = "driver-root"
	DefaultHostDriverRootPath = "/run/nvidia/driver"
	DriverRootMountPath       = "/run/nvidia/driver"
	HostSysVolumeName         = "host-sys"
	HostSysPath               = "/sys"
	WriteSyslogEventEnvVar    = "WRITE_SYSLOG_EVENT"
	NodeNameEnvVar            = "NODE_NAME"
	UploadURLBaseEnvVar       = "UPLOAD_URL_BASE"
	DriverBinPathEnvVar       = "DRIVER_BIN_PATH"
	ResetStateVolumeName      = "reset-state"
	ResetStateMountPath       = "/run/nvsentinel-gpu-reset"
	ResetRunCountFileEnvVar   = "RESET_RUN_COUNT_FILE"
	ResetMaxRunsEnvVar        = "RESET_MAX_RUNS"
	// ResetJobMaxRuns is how many times the reset Job runs gpu_reset.sh. With restartPolicy OnFailure, the
	// Job controller fails the Job when the container restart count reaches backoffLimit, so a backoffLimit
	// of ResetJobMaxRuns gives ResetJobMaxRuns full runs. The next run can start before the Job fails; the
	// script then exits without a reset.
	ResetJobMaxRuns = 2
	// ContainerDriverRoot is the driverRoot that makes the reset command chroot into the reset
	// container itself instead of into the mounted driver filesystem.
	ContainerDriverRoot = "/"
)

func applyConfigDefaults(config *Config) {
	applyGlobalDefaults(config)
	applyTimeoutDefaults(config)
	applyManualModeDefaults(config)
	applyExclusionsDefaults(config)
	applyCSPProviderHostDefaults(config)
}

func applyGlobalDefaults(config *Config) {
	if config.Global.Timeout == 0 {
		config.Global.Timeout = 30 * time.Minute
	}

	if config.Global.ManualMode == nil {
		config.Global.ManualMode = new(false)
	}
}

func applyTimeoutDefaults(config *Config) {
	if config.RebootNode.Timeout == 0 {
		config.RebootNode.Timeout = config.Global.Timeout
	}

	if config.TerminateNode.Timeout == 0 {
		config.TerminateNode.Timeout = config.Global.Timeout
	}

	if config.GPUReset.Timeout == 0 {
		config.GPUReset.Timeout = config.Global.Timeout
	}
}

func applyManualModeDefaults(config *Config) {
	if config.RebootNode.ManualMode == nil {
		config.RebootNode.ManualMode = config.Global.ManualMode
	}

	if config.TerminateNode.ManualMode == nil {
		config.TerminateNode.ManualMode = config.Global.ManualMode
	}

	if config.GPUReset.ManualMode == nil {
		config.GPUReset.ManualMode = config.Global.ManualMode
	}
}

func applyExclusionsDefaults(config *Config) {
	if len(config.RebootNode.Exclusions) == 0 {
		config.RebootNode.Exclusions = config.Global.Nodes.Exclusions
	}

	if len(config.TerminateNode.Exclusions) == 0 {
		config.TerminateNode.Exclusions = config.Global.Nodes.Exclusions
	}

	if len(config.GPUReset.Exclusions) == 0 {
		config.GPUReset.Exclusions = config.Global.Nodes.Exclusions
	}
}

func applyCSPProviderHostDefaults(config *Config) {
	if len(config.RebootNode.CSPProviderHost) == 0 {
		config.RebootNode.CSPProviderHost = config.Global.CSPProviderHost
	}

	if len(config.TerminateNode.CSPProviderHost) == 0 {
		config.TerminateNode.CSPProviderHost = config.Global.CSPProviderHost
	}

	if len(config.GPUReset.CSPProviderHost) == 0 {
		config.GPUReset.CSPProviderHost = config.Global.CSPProviderHost
	}

	// Cascade CSP provider TLS settings from global to controller-specific configs
	if len(config.RebootNode.CSPProviderCAPath) == 0 {
		config.RebootNode.CSPProviderCAPath = config.Global.CSPProviderCAPath
	}

	if !config.RebootNode.CSPProviderInsecure {
		config.RebootNode.CSPProviderInsecure = config.Global.CSPProviderInsecure
	}

	if len(config.TerminateNode.CSPProviderCAPath) == 0 {
		config.TerminateNode.CSPProviderCAPath = config.Global.CSPProviderCAPath
	}

	if !config.TerminateNode.CSPProviderInsecure {
		config.TerminateNode.CSPProviderInsecure = config.Global.CSPProviderInsecure
	}

	// Cascade CSP provider token path from global to controller-specific configs
	if len(config.RebootNode.CSPProviderTokenPath) == 0 {
		config.RebootNode.CSPProviderTokenPath = config.Global.CSPProviderTokenPath
	}

	if len(config.TerminateNode.CSPProviderTokenPath) == 0 {
		config.TerminateNode.CSPProviderTokenPath = config.Global.CSPProviderTokenPath
	}
}

func applyResetJobDefaults(config *ResetJobConfig) {
	if config.WriteSysLogEvent == nil {
		config.WriteSysLogEvent = new(true)
	}

	if config.HostDriverRootPath == "" {
		config.HostDriverRootPath = DefaultHostDriverRootPath
	}

	if config.DriverRoot == "" {
		config.DriverRoot = DriverRootMountPath
	}
}

func getResources(resources ResourceRequirements) (*corev1.ResourceRequirements, error) {
	limits, err := parseResourceList(resources.Limits)
	if err != nil {
		return nil, err
	}

	requests, err := parseResourceList(resources.Requests)
	if err != nil {
		return nil, err
	}

	return &corev1.ResourceRequirements{
		Limits:   limits,
		Requests: requests,
	}, nil
}

func parseResourceList(input map[string]string) (corev1.ResourceList, error) {
	result := corev1.ResourceList{}

	for k, v := range input {
		qty, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", k, err)
		}

		result[corev1.ResourceName(k)] = qty
	}

	return result, nil
}

func getImagePullSecrets(imagePullSecrets []ImagePullSecret) []corev1.LocalObjectReference {
	var imagePullSecretsReference []corev1.LocalObjectReference
	for _, imagePullSecret := range imagePullSecrets {
		imagePullSecretsReference = append(imagePullSecretsReference, corev1.LocalObjectReference{
			Name: imagePullSecret.Name,
		})
	}

	return imagePullSecretsReference
}

// getDefaultGPUResetJobTemplate returns the default JobTemplateSpec for GPU reset jobs.
func getDefaultGPUResetJobTemplate(namespace string, image string, secrets []ImagePullSecret,
	resources ResourceRequirements, hostDriverRootPath string, driverRoot string, runtimeClassName string,
	writeSyslogEvent bool, uploadURL string) (*batchv1.JobTemplateSpec, error) {
	imagePullSecrets := getImagePullSecrets(secrets)

	if hostDriverRootPath == "" {
		hostDriverRootPath = DefaultHostDriverRootPath
	}

	if driverRoot == "" {
		driverRoot = DriverRootMountPath
	}

	// Reject here, at janitor startup, rather than when a Job is created for a node that is
	// already cordoned and drained. A path such as "//" is absolute but is not ContainerDriverRoot,
	// so it would mount the host driver root over the container root.
	if !filepath.IsAbs(driverRoot) || filepath.Clean(driverRoot) != driverRoot {
		return nil, fmt.Errorf("resetJob.driverRoot %q must be an absolute, clean path", driverRoot)
	}

	containerResources, err := getResources(resources)
	if err != nil {
		return nil, err
	}

	job := &batchv1.JobTemplateSpec{
		Namespace: namespace,
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   new(int64(300)),
			BackoffLimit:            new(int32(ResetJobMaxRuns)),
			TTLSecondsAfterFinished: new(int32(86400)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: HostDevVolumeName,
							HostPath: &corev1.HostPathVolumeSource{
								Path: HostDevPath,
							},
						},
						{
							Name: HostDevLogVolumeName,
							HostPath: &corev1.HostPathVolumeSource{
								Path: HostDevLogPath,
							},
						},
						{
							Name: DriverRootVolumeName,
							HostPath: &corev1.HostPathVolumeSource{
								Path: hostDriverRootPath,
							},
						},
						{
							Name: HostSysVolumeName,
							HostPath: &corev1.HostPathVolumeSource{
								Path: HostSysPath,
							},
						},
						{
							// gpu_reset.sh counts its runs here. An emptyDir outlives a container restart, and
							// each new pod starts from zero.
							Name: ResetStateVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            GPUResetContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Resources:       *containerResources,
							Env: []corev1.EnvVar{
								{
									Name:  "NVIDIA_VISIBLE_DEVICES",
									Value: "void",
								},
								{
									Name:  "DRIVER_ROOT",
									Value: driverRoot,
								},
								{
									Name:  WriteSyslogEventEnvVar,
									Value: strconv.FormatBool(writeSyslogEvent),
								},
								{
									Name: NodeNameEnvVar,
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name:  UploadURLBaseEnvVar,
									Value: uploadURL,
								},
								{
									Name:  ResetRunCountFileEnvVar,
									Value: filepath.Join(ResetStateMountPath, "run-count"),
								},
								{
									Name:  ResetMaxRunsEnvVar,
									Value: strconv.Itoa(ResetJobMaxRuns),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      HostDevVolumeName,
									MountPath: HostDevPath,
								},
								{
									Name:      HostDevLogVolumeName,
									MountPath: HostDevLogPath,
								},
								{
									Name:      DriverRootVolumeName,
									MountPath: driverRoot,
								},
								{
									Name:      HostSysVolumeName,
									MountPath: filepath.Join(driverRoot, HostSysPath),
								},
								{
									Name:      ResetStateVolumeName,
									MountPath: ResetStateMountPath,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: new(true),
							},
						},
					},
					RestartPolicy:    corev1.RestartPolicyOnFailure,
					ImagePullSecrets: imagePullSecrets,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
				},
			},
		},
	}
	if len(runtimeClassName) > 0 {
		job.Spec.Template.Spec.RuntimeClassName = &runtimeClassName
	}

	// Kubernetes cannot mount the host driver root over "/", and a chroot into "/" is a no-op,
	// so the reset command uses the nvidia-smi of the reset container image instead.
	if driverRoot == ContainerDriverRoot {
		podSpec := &job.Spec.Template.Spec
		podSpec.Volumes = slices.DeleteFunc(podSpec.Volumes, func(volume corev1.Volume) bool {
			return volume.Name == DriverRootVolumeName
		})
		podSpec.Containers[0].VolumeMounts = slices.DeleteFunc(podSpec.Containers[0].VolumeMounts,
			func(mount corev1.VolumeMount) bool { return mount.Name == DriverRootVolumeName })
	}

	return job, nil
}

// applyDriverBinPath passes driverBinPath to the reset container. It is a path inside driverRoot
// that the reset command searches for the driver binaries, for drivers installed outside the
// default PATH. An empty value adds nothing, so the Job is unchanged.
func applyDriverBinPath(job *batchv1.JobTemplateSpec, driverBinPath string) error {
	if driverBinPath == "" {
		return nil
	}

	// Reject at janitor startup, as for driverRoot. The value is joined into PATH, so a relative
	// path or a ":" would add an unintended search entry.
	if !filepath.IsAbs(driverBinPath) || filepath.Clean(driverBinPath) != driverBinPath ||
		strings.Contains(driverBinPath, ":") {
		return fmt.Errorf("resetJob.driverBinPath %q must be an absolute, clean path without ':'", driverBinPath)
	}

	container := &job.Spec.Template.Spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: DriverBinPathEnvVar, Value: driverBinPath})

	return nil
}

// applyJobTemplatePatch applies jobTemplatePatch to the reset Job template as a strategic merge patch, using
// JobTemplateSpec as the schema. An empty patch leaves the template unchanged.
//
// It checks the patch and the merged template strictly, so a misspelled field stops janitor at startup. Without the
// check, the misspelled field would be dropped silently, and every reset Job would run without it.
func applyJobTemplatePatch(job *batchv1.JobTemplateSpec, patch string) (*batchv1.JobTemplateSpec, error) {
	if strings.TrimSpace(patch) == "" {
		return job, nil
	}

	patchJSON, err := yaml.YAMLToJSONStrict([]byte(patch))
	if err != nil {
		return nil, fmt.Errorf("resetJob.jobTemplatePatch is not valid YAML: %w", err)
	}

	if err := validatePatchFields(patchJSON); err != nil {
		return nil, err
	}

	original, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the GPU reset Job template: %w", err)
	}

	merged, err := strategicpatch.StrategicMergePatch(original, patchJSON, batchv1.JobTemplateSpec{})
	if err != nil {
		return nil, fmt.Errorf("failed to apply resetJob.jobTemplatePatch: %w", err)
	}

	patched := &batchv1.JobTemplateSpec{}

	strictErrs, err := sigsjson.UnmarshalStrict(merged, patched)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the GPU reset Job template after resetJob.jobTemplatePatch: %w", err)
	}

	if len(strictErrs) > 0 {
		return nil, fmt.Errorf("resetJob.jobTemplatePatch: %w", errors.Join(strictErrs...))
	}

	return patched, nil
}

// validatePatchFields checks that every field in the patch is a real Job field with the right type. Directives such
// as $patch and $retainKeys are removed first, because they are not fields. Checking only the merged template misses
// one case: the merge removes any field that the patch sets to null, so a misspelled field set to null never shows
// up there.
func validatePatchFields(patchJSON []byte) error {
	var patch any
	if err := json.Unmarshal(patchJSON, &patch); err != nil {
		return fmt.Errorf("failed to decode resetJob.jobTemplatePatch: %w", err)
	}

	fields, err := json.Marshal(withoutPatchDirectives(patch))
	if err != nil {
		return fmt.Errorf("failed to encode resetJob.jobTemplatePatch: %w", err)
	}

	strictErrs, err := sigsjson.UnmarshalStrict(fields, &batchv1.JobTemplateSpec{})
	if err != nil {
		return fmt.Errorf("failed to decode resetJob.jobTemplatePatch: %w", err)
	}

	if len(strictErrs) > 0 {
		return fmt.Errorf("resetJob.jobTemplatePatch: %w", errors.Join(strictErrs...))
	}

	return nil
}

// withoutPatchDirectives removes every key that starts with "$" from the decoded patch.
func withoutPatchDirectives(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if strings.HasPrefix(key, "$") {
				delete(typed, key)
				continue
			}

			typed[key] = withoutPatchDirectives(item)
		}
	case []any:
		for i, item := range typed {
			typed[i] = withoutPatchDirectives(item)
		}
	}

	return value
}
