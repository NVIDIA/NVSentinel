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

package webhook

import (
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	nvsentinelSocketVolumeName = "nvsentinel-socket"
)

type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Injector struct {
	cfg *config.Config
}

func NewInjector(cfg *config.Config) *Injector {
	return &Injector{cfg: cfg}
}

func (i *Injector) InjectInitContainers(pod *corev1.Pod) ([]PatchOperation, error) {
	maxResources := i.findMaxResources(pod)
	if len(maxResources) == 0 {
		slog.Debug("Pod does not request GPU/network resources, skipping injection")
		return nil, nil
	}

	initContainers := i.buildInitContainers(maxResources)
	if len(initContainers) == 0 {
		return nil, nil
	}

	var patches []PatchOperation

	if len(pod.Spec.InitContainers) == 0 {
		patches = append(patches, PatchOperation{
			Op:    "add",
			Path:  "/spec/initContainers",
			Value: initContainers,
		})
	} else {
		// Prepend in reverse order so they end up in correct order at the front
		// If preflight containers are [A, B] and existing are [C, D],
		// result will be [A, B, C, D]
		for idx := len(initContainers) - 1; idx >= 0; idx-- {
			patches = append(patches, PatchOperation{
				Op:    "add",
				Path:  "/spec/initContainers/0",
				Value: initContainers[idx],
			})
		}
	}

	volumePatches := i.injectVolumes(pod)
	patches = append(patches, volumePatches...)

	return patches, nil
}

// findMaxResources scans all containers and returns the maximum quantity
// for each GPU and network resource. Returns empty map if no GPU resources found.
func (i *Injector) findMaxResources(pod *corev1.Pod) corev1.ResourceList {
	maxResources := make(corev1.ResourceList)

	allResourceNames := append([]string{}, i.cfg.GPUResourceNames...)
	allResourceNames = append(allResourceNames, i.cfg.NetworkResourceNames...)

	for _, container := range pod.Spec.Containers {
		for _, name := range allResourceNames {
			resName := corev1.ResourceName(name)

			i.updateMax(maxResources, resName, container.Resources.Limits[resName])
			i.updateMax(maxResources, resName, container.Resources.Requests[resName])
		}
	}

	hasGPU := false

	for _, name := range i.cfg.GPUResourceNames {
		if qty, ok := maxResources[corev1.ResourceName(name)]; ok && !qty.IsZero() {
			hasGPU = true
			break
		}
	}

	if !hasGPU {
		return nil
	}

	return maxResources
}

func (i *Injector) updateMax(resources corev1.ResourceList, name corev1.ResourceName, qty resource.Quantity) {
	if qty.IsZero() {
		return
	}

	if current, exists := resources[name]; !exists || qty.Cmp(current) > 0 {
		resources[name] = qty
	}
}

func (i *Injector) buildInitContainers(maxResources corev1.ResourceList) []corev1.Container {
	var initContainers []corev1.Container

	for _, tmpl := range i.cfg.InitContainers {
		container := tmpl.DeepCopy()

		if container.Resources.Requests == nil {
			container.Resources.Requests = make(corev1.ResourceList)
		}

		if container.Resources.Limits == nil {
			container.Resources.Limits = make(corev1.ResourceList)
		}

		for name, qty := range maxResources {
			container.Resources.Requests[name] = qty
			container.Resources.Limits[name] = qty
		}

		if _, ok := container.Resources.Requests[corev1.ResourceCPU]; !ok {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		}

		if _, ok := container.Resources.Requests[corev1.ResourceMemory]; !ok {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("500Mi")
		}

		i.injectCommonEnv(container)
		i.injectDCGMEnv(container)

		initContainers = append(initContainers, *container)
	}

	return initContainers
}

// injectCommonEnv injects environment variables common to all preflight init containers.
// These include NODE_NAME, PLATFORM_CONNECTOR_SOCKET, and PROCESSING_STRATEGY which are
// needed by any preflight check that publishes health events.
func (i *Injector) injectCommonEnv(container *corev1.Container) {
	envVars := []corev1.EnvVar{
		{
			Name: "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
	}

	if i.cfg.DCGM.ConnectorSocket != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "PLATFORM_CONNECTOR_SOCKET",
			Value: i.cfg.DCGM.ConnectorSocket,
		})
	}

	if i.cfg.DCGM.ProcessingStrategy != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "PROCESSING_STRATEGY",
			Value: i.cfg.DCGM.ProcessingStrategy,
		})
	}

	i.mergeEnvVars(container, envVars)
}

func (i *Injector) injectVolumes(pod *corev1.Pod) []PatchOperation {
	var patches []PatchOperation

	var volumesToAdd []corev1.Volume

	existingVolumes := make(map[string]bool)
	for _, vol := range pod.Spec.Volumes {
		existingVolumes[vol.Name] = true
	}

	if i.cfg.DCGM.ConnectorSocket != "" && !existingVolumes[nvsentinelSocketVolumeName] {
		// Platform-connector mounts /var/run/nvsentinel (host) -> /var/run (container)
		// and creates socket at /var/run/nvsentinel.sock inside its container.
		// This is the same hostPath used by gpu-health-monitor.
		hostPathType := corev1.HostPathDirectoryOrCreate

		volumesToAdd = append(volumesToAdd, corev1.Volume{
			Name: nvsentinelSocketVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/nvsentinel",
					Type: &hostPathType,
				},
			},
		})
	}

	if len(volumesToAdd) == 0 {
		return patches
	}

	if len(pod.Spec.Volumes) == 0 {
		patches = append(patches, PatchOperation{
			Op:    "add",
			Path:  "/spec/volumes",
			Value: volumesToAdd,
		})
	} else {
		for _, vol := range volumesToAdd {
			patches = append(patches, PatchOperation{
				Op:    "add",
				Path:  "/spec/volumes/-",
				Value: vol,
			})
		}
	}

	return patches
}

// injectDCGMEnv injects DCGM-specific environment variables for the dcgm-diag check.
func (i *Injector) injectDCGMEnv(container *corev1.Container) {
	if container.Name != "preflight-dcgm-diag" {
		return
	}

	envVars := []corev1.EnvVar{
		{
			Name:  "DCGM_DIAG_LEVEL",
			Value: fmt.Sprintf("%d", i.cfg.DCGM.DiagLevel),
		},
	}

	if i.cfg.DCGM.HostengineAddr != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "DCGM_HOSTENGINE_ADDR",
			Value: i.cfg.DCGM.HostengineAddr,
		})
	}

	i.mergeEnvVars(container, envVars)
}

// mergeEnvVars merges the provided env vars into the container.
// User-defined env vars (already present in container) take precedence.
func (i *Injector) mergeEnvVars(container *corev1.Container, envVars []corev1.EnvVar) {
	existingEnvNames := make(map[string]bool)
	for _, env := range container.Env {
		existingEnvNames[env.Name] = true
	}

	for _, env := range envVars {
		if !existingEnvNames[env.Name] {
			container.Env = append(container.Env, env)
		}
	}
}
