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
	"fmt"
	"log/slog"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

const (
	GPUResetContainerName  = "gpu-reset"
	HostDevVolumeName      = "host-dev"
	HostDevPath            = "/dev"
	HostDevLogVolumeName   = "dev-log"
	HostDevLogPath         = "/run/systemd/journal/dev-log"
	DriverRootVolumeName   = "driver-root"
	DriverRootPath         = "/run/nvidia/driver"
	HostSysVolumeName      = "host-sys"
	HostSysPath            = "/sys"
	WriteSyslogEventEnvVar = "WRITE_SYSLOG_EVENT"
	NodeNameEnvVar         = "NODE_NAME"
	UploadURLBaseEnvVar    = "UPLOAD_URL_BASE"
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

func resolveGPUResetJobTemplate(resetJob *ResetJobConfig, namespace string) (*batchv1.JobTemplateSpec, error) {
	if resetJob.JobTemplate != "" {
		if ignored := ignoredResetJobFields(resetJob); len(ignored) > 0 {
			slog.Warn("resetJob.jobTemplate replaces the built-in template, ignoring other resetJob fields",
				"fields", ignored)
		}

		return parseGPUResetJobTemplate(namespace, resetJob.JobTemplate)
	}

	if len(resetJob.ImageConfig.Image) == 0 {
		return nil, fmt.Errorf("ResetJob.ImageConfig.Image is required but not set")
	}

	if resetJob.WriteSysLogEvent == nil {
		resetJob.WriteSysLogEvent = new(true)
	}

	return getDefaultGPUResetJobTemplate(namespace, resetJob.ImageConfig.Image, resetJob.ImageConfig.ImagePullSecrets,
		resetJob.Resources, resetJob.RuntimeClassName, *resetJob.WriteSysLogEvent, resetJob.UploadURL)
}

// parseGPUResetJobTemplate uses sigs.k8s.io/yaml rather than mapstructure so that decoding
// honours the Kubernetes json tags and the resource.Quantity unmarshaller.
func parseGPUResetJobTemplate(namespace, raw string) (*batchv1.JobTemplateSpec, error) {
	var tmpl batchv1.JobTemplateSpec

	if err := yaml.UnmarshalStrict([]byte(raw), &tmpl); err != nil {
		return nil, fmt.Errorf("failed to parse resetJob.jobTemplate: %w", err)
	}

	switch {
	case tmpl.Namespace == "":
		tmpl.Namespace = namespace
	case tmpl.Namespace != namespace:
		return nil, fmt.Errorf("resetJob.jobTemplate metadata.namespace %q must be empty or the janitor "+
			"namespace %q, which is where the controller looks for its jobs", tmpl.Namespace, namespace)
	}

	if len(tmpl.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("resetJob.jobTemplate must define at least one container")
	}

	if tmpl.Spec.Template.Spec.NodeName != "" {
		return nil, fmt.Errorf("resetJob.jobTemplate must not set spec.template.spec.nodeName %q, "+
			"the controller sets it from the GPUReset it is reconciling", tmpl.Spec.Template.Spec.NodeName)
	}

	if tmpl.Name != "" {
		return nil, fmt.Errorf("resetJob.jobTemplate must not set metadata.name %q, "+
			"the controller derives it from the GPUReset it is reconciling", tmpl.Name)
	}

	if tmpl.GenerateName != "" {
		return nil, fmt.Errorf("resetJob.jobTemplate must not set metadata.generateName %q, "+
			"the controller derives metadata.name from the GPUReset it is reconciling", tmpl.GenerateName)
	}

	return &tmpl, nil
}

func ignoredResetJobFields(resetJob *ResetJobConfig) []string {
	var ignored []string

	if resetJob.ImageConfig.Image != "" {
		ignored = append(ignored, "imageConfig.image")
	}

	if len(resetJob.ImageConfig.ImagePullSecrets) > 0 {
		ignored = append(ignored, "imageConfig.imagePullSecrets")
	}

	if len(resetJob.Resources.Limits) > 0 || len(resetJob.Resources.Requests) > 0 {
		ignored = append(ignored, "resources")
	}

	if resetJob.RuntimeClassName != "" {
		ignored = append(ignored, "runtimeClassName")
	}

	if resetJob.WriteSysLogEvent != nil {
		ignored = append(ignored, "writeSysLogEvent")
	}

	if resetJob.UploadURL != "" {
		ignored = append(ignored, "uploadURL")
	}

	return ignored
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
	resources ResourceRequirements, runtimeClassName string, writeSyslogEvent bool,
	uploadURL string) (*batchv1.JobTemplateSpec, error) {
	imagePullSecrets := getImagePullSecrets(secrets)

	containerResources, err := getResources(resources)
	if err != nil {
		return nil, err
	}

	job := &batchv1.JobTemplateSpec{
		Namespace: namespace,
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   new(int64(300)),
			BackoffLimit:            new(int32(2)),
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
								Path: DriverRootPath,
							},
						},
						{
							Name: HostSysVolumeName,
							HostPath: &corev1.HostPathVolumeSource{
								Path: HostSysPath,
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
									Value: DriverRootPath,
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
									MountPath: DriverRootPath,
								},
								{
									Name:      HostSysVolumeName,
									MountPath: DriverRootPath + HostSysPath,
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

	return job, nil
}
