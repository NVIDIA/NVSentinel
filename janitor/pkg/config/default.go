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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	GPUResetContainerName = "gpu-reset"
	HostDevVolumeName     = "host-dev"
	HostDevPath           = "/dev"
	HostDevLogVolumeName  = "dev-log"
	HostDevLogPath        = "/dev/log"
)

// nolint:cyclop // expected cyclomatic complexity for applying default settings
func applyConfigDefaults(config *Config) {
	if config.Global.Timeout == 0 {
		config.Global.Timeout = 30 * time.Minute
	}

	if config.Global.ManualMode == nil {
		config.Global.ManualMode = ptr.To(false)
	}

	if config.RebootNode.Timeout == 0 {
		config.RebootNode.Timeout = config.Global.Timeout
	}

	if config.TerminateNode.Timeout == 0 {
		config.TerminateNode.Timeout = config.Global.Timeout
	}

	if config.GPUReset.Timeout == 0 {
		config.GPUReset.Timeout = config.Global.Timeout
	}

	if config.RebootNode.ManualMode == nil {
		config.RebootNode.ManualMode = config.Global.ManualMode
	}

	if config.TerminateNode.ManualMode == nil {
		config.TerminateNode.ManualMode = config.Global.ManualMode
	}

	if config.GPUReset.ManualMode == nil {
		config.GPUReset.ManualMode = config.Global.ManualMode
	}

	if len(config.RebootNode.Exclusions) == 0 {
		config.RebootNode.Exclusions = config.Global.Nodes.Exclusions
	}

	if len(config.TerminateNode.Exclusions) == 0 {
		config.TerminateNode.Exclusions = config.Global.Nodes.Exclusions
	}

	if len(config.GPUReset.Exclusions) == 0 {
		config.GPUReset.Exclusions = config.Global.Nodes.Exclusions
	}

	if len(config.RebootNode.CSPProviderHost) == 0 {
		config.RebootNode.CSPProviderHost = config.Global.CSPProviderHost
	}

	if len(config.TerminateNode.CSPProviderHost) == 0 {
		config.TerminateNode.CSPProviderHost = config.Global.CSPProviderHost
	}

	if len(config.GPUReset.CSPProviderHost) == 0 {
		config.GPUReset.CSPProviderHost = config.Global.CSPProviderHost
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
	resources ResourceRequirements, runtimeClassName string) (*batchv1.JobTemplateSpec, error) {
	imagePullSecrets := getImagePullSecrets(secrets)

	containerResources, err := getResources(resources)
	if err != nil {
		return nil, err
	}

	job := &batchv1.JobTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   ptr.To(int64(300)),
			BackoffLimit:            ptr.To(int32(2)),
			TTLSecondsAfterFinished: ptr.To(int32(86400)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: HostDevVolumeName,
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: HostDevPath,
								},
							},
						},
						{
							Name: HostDevLogVolumeName,
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: HostDevLogPath,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            GPUResetContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Resources:       *containerResources,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      HostDevVolumeName,
									MountPath: HostDevPath,
								},
								{
									Name:      HostDevLogVolumeName,
									MountPath: HostDevLogPath,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
					RestartPolicy:    corev1.RestartPolicyOnFailure,
					HostNetwork:      true,
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
