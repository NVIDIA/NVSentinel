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
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var supportedHostPathTypes = map[string]corev1.HostPathType{
	string(corev1.HostPathDirectory):         corev1.HostPathDirectory,
	string(corev1.HostPathDirectoryOrCreate): corev1.HostPathDirectoryOrCreate,
	string(corev1.HostPathFile):              corev1.HostPathFile,
	string(corev1.HostPathFileOrCreate):      corev1.HostPathFileOrCreate,
	string(corev1.HostPathSocket):            corev1.HostPathSocket,
	string(corev1.HostPathCharDev):           corev1.HostPathCharDev,
	string(corev1.HostPathBlockDev):          corev1.HostPathBlockDev,
}

const (
	nvsentinelSocketVolumeName = "nvsentinel-socket"
	// dshmVolumeName is the name for the shared memory volume needed by NCCL
	dshmVolumeName = "dshm"
	// ncclTopoVolumeName is the name for the NCCL topology ConfigMap volume
	ncclTopoVolumeName = "nccl-topo"

	// PreflightChecksAnnotation is the pod annotation listing which preflight
	// checks to run. Value is a comma-separated list of init container names.
	// When absent, all containers with defaultEnabled (or omitted) are injected.
	PreflightChecksAnnotation = "nvsentinel.nvidia.com/preflight-checks"
)

type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Injector struct {
	cfg        *config.Config
	discoverer gang.GangDiscoverer
}

func NewInjector(cfg *config.Config, discoverer gang.GangDiscoverer) *Injector {
	return &Injector{
		cfg:        cfg,
		discoverer: discoverer,
	}
}

// GangContext contains gang information extracted during injection.
// This is returned so the controller can register the peer.
type GangContext struct {
	GangID        string
	ConfigMapName string
	// CheckNames is a comma-separated list of injected check container
	// names. Annotation order when annotation is present, chart order
	// when using defaults.
	CheckNames string
}

// ParseCheckNames splits a comma-separated annotation value into a list of
// container names. Returns an error if any name appears more than once.
// Exported so the gang controller can use the same parsing.
func ParseCheckNames(csv string) ([]string, error) {
	seen := make(map[string]bool)

	var names []string

	for _, part := range strings.Split(csv, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}

		if seen[name] {
			return nil, fmt.Errorf("duplicate check name %q in annotation %s",
				name, PreflightChecksAnnotation)
		}

		seen[name] = true
		names = append(names, name)
	}

	return names, nil
}

func configuredNames(specs []config.InitContainerSpec) []string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}

	return names
}

func (i *Injector) InjectInitContainers(pod *corev1.Pod) ([]PatchOperation, *GangContext, error) {
	maxResources := i.findMaxResources(pod)
	if len(maxResources) == 0 {
		slog.Debug("Pod does not request GPU/network resources, skipping injection")
		return nil, nil, nil
	}

	// Check if pod is part of a gang
	var gangCtx *GangContext

	if i.cfg.GangCoordination.Enabled && i.discoverer != nil {
		if i.discoverer.CanHandle(pod) {
			gangID := i.discoverer.ExtractGangID(pod)
			if gangID != "" {
				gangCtx = &GangContext{
					GangID:        gangID,
					ConfigMapName: gang.ConfigMapName(gangID),
				}
				slog.Info("Pod is part of a gang",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"gangID", gangID,
					"configMap", gangCtx.ConfigMapName,
					"discoverer", i.discoverer.Name())
			}
		} else {
			slog.Debug("Pod not handled by gang discoverer",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"discoverer", i.discoverer.Name())
		}
	}

	selected, err := i.selectInitContainers(pod)
	if err != nil {
		return nil, nil, err
	}

	initContainers := i.buildInitContainers(pod, maxResources, gangCtx, selected)

	// Compute check names for gang validation.
	if gangCtx != nil {
		names := make([]string, len(initContainers))
		for idx, c := range initContainers {
			names[idx] = c.Name
		}

		gangCtx.CheckNames = strings.Join(names, ",")
	}

	if len(initContainers) == 0 {
		// No init containers to inject, but still return gangCtx
		// so the controller can track gang membership
		return nil, gangCtx, nil
	}

	patches := i.patchInitContainers(pod, initContainers)
	patches = append(patches, i.injectVolumes(pod, gangCtx)...)

	return patches, gangCtx, nil
}

// patchInitContainers builds JSON Patch operations to add preflight init
// containers to the pod. When no existing init containers are present,
// the array is created. Otherwise placement is controlled by config.
func (i *Injector) patchInitContainers(pod *corev1.Pod, initContainers []corev1.Container) []PatchOperation {
	if len(pod.Spec.InitContainers) == 0 {
		return []PatchOperation{{
			Op:    "add",
			Path:  "/spec/initContainers",
			Value: initContainers,
		}}
	}

	patches := make([]PatchOperation, 0, len(initContainers))

	for idx, c := range initContainers {
		path := "/spec/initContainers/-"
		if i.cfg.InitContainerPlacement == config.PlacementPrepend {
			path = fmt.Sprintf("/spec/initContainers/%d", idx)
		}

		patches = append(patches, PatchOperation{
			Op:    "add",
			Path:  path,
			Value: c,
		})
	}

	return patches
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

	if !i.hasGPUResources(maxResources) {
		return nil
	}

	return maxResources
}

// hasGPUResources returns true if maxResources contains at least one
// non-zero GPU resource.
func (i *Injector) hasGPUResources(maxResources corev1.ResourceList) bool {
	for _, name := range i.cfg.GPUResourceNames {
		if qty, ok := maxResources[corev1.ResourceName(name)]; ok && !qty.IsZero() {
			return true
		}
	}

	return false
}

func (i *Injector) updateMax(resources corev1.ResourceList, name corev1.ResourceName, qty resource.Quantity) {
	if qty.IsZero() {
		return
	}

	if current, exists := resources[name]; !exists || qty.Cmp(current) > 0 {
		resources[name] = qty
	}
}

// selectInitContainers returns the subset of configured init containers to
// inject based on the pod's preflight-checks annotation or defaultEnabled.
func (i *Injector) selectInitContainers(pod *corev1.Pod) ([]config.InitContainerSpec, error) {
	ann, ok := pod.Annotations[PreflightChecksAnnotation]
	if !ok {
		// No annotation — use defaultEnabled.
		var result []config.InitContainerSpec

		for _, spec := range i.cfg.InitContainers {
			if spec.IsDefaultEnabled() {
				result = append(result, spec)
			} else {
				slog.Debug("Init container disabled by default", "container", spec.Name)
			}
		}

		return result, nil
	}

	// Annotation present — only inject named containers.
	// ParseCheckNames normalizes: split, trim; rejects duplicates.
	// An empty or whitespace/comma-only annotation yields an empty list,
	// which disables all checks.
	requested, err := ParseCheckNames(ann)
	if err != nil {
		return nil, err
	}

	if len(requested) == 0 {
		return nil, nil
	}

	configuredByName := make(map[string]config.InitContainerSpec, len(i.cfg.InitContainers))

	for _, spec := range i.cfg.InitContainers {
		configuredByName[spec.Name] = spec
	}

	// Walk annotation order so init container execution order matches
	// what the user specified.
	var result []config.InitContainerSpec

	var unknown []string

	for _, name := range requested {
		spec, exists := configuredByName[name]
		if !exists {
			unknown = append(unknown, name)

			continue
		}

		result = append(result, spec)
	}

	if len(unknown) > 0 {
		return nil, fmt.Errorf(
			"annotation %s references unknown checks: %s (configured: %s)",
			PreflightChecksAnnotation,
			strings.Join(unknown, ", "),
			strings.Join(configuredNames(i.cfg.InitContainers), ", "))
	}

	return result, nil
}

func (i *Injector) buildInitContainers(
	pod *corev1.Pod,
	maxResources corev1.ResourceList,
	gangCtx *GangContext,
	selected []config.InitContainerSpec,
) []corev1.Container {
	var initContainers []corev1.Container

	// Determine whether to mirror pod-level DRA claims to init containers.
	mirrorClaims := i.cfg.GangCoordination.MirrorResourceClaims != nil &&
		*i.cfg.GangCoordination.MirrorResourceClaims

	// Collect env vars and volume mounts from main containers that match
	// configured patterns. This allows init containers to inherit fabric-
	// specific NCCL config from the user's training container.
	userEnvVars := i.collectMatchingEnvVars(pod.Spec.Containers)
	userVolumeMounts := i.collectMatchingVolumeMounts(pod.Spec.Containers)

	for _, tmpl := range selected {
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
		i.injectGangEnv(container, gangCtx)

		// Copy matching env vars from user containers (lower precedence
		// than the init container's own env vars — only adds new names).
		i.mergeEnvVars(container, userEnvVars)

		// Copy matching volume mounts from user containers.
		i.mergeVolumeMounts(container, userVolumeMounts)

		if gangCtx != nil {
			i.injectGangMounts(container, mirrorClaims, pod.Spec.ResourceClaims)
		}

		initContainers = append(initContainers, *container)
	}

	return initContainers
}

// injectGangMounts adds gang-related volume mounts and DRA resource claims
// to an init container.
func (i *Injector) injectGangMounts(
	container *corev1.Container,
	mirrorClaims bool,
	podResourceClaims []corev1.PodResourceClaim,
) {
	// Gang ConfigMap and /dev/shm mounts are always needed for gang members.
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      types.GangConfigVolumeName,
			MountPath: i.cfg.GangCoordination.ConfigMapMountPath,
			ReadOnly:  true,
		},
		// NCCL requires a larger shared memory segment than the default 64MB.
		corev1.VolumeMount{
			Name:      dshmVolumeName,
			MountPath: "/dev/shm",
		},
	)

	// Add NCCL topology ConfigMap mount if configured.
	if i.cfg.GangCoordination.NCCLTopoConfigMap != "" {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      ncclTopoVolumeName,
			MountPath: "/etc/nccl",
			ReadOnly:  true,
		})
	}

	i.appendExtraHostPathMounts(container)
	i.appendExtraVolumeMounts(container)

	// Mirror all pod-level DRA resource claims to init containers.
	// This ensures init containers get the same device access as main
	// containers: GPUs, RDMA NICs, IMEX channels (GB200 MNNVL), etc.
	if mirrorClaims {
		for _, podClaim := range podResourceClaims {
			container.Resources.Claims = append(container.Resources.Claims, corev1.ResourceClaim{
				Name: podClaim.Name,
			})
		}
	}
}

// appendExtraHostPathMounts appends configured extra hostPath mounts to the container.
func (i *Injector) appendExtraHostPathMounts(container *corev1.Container) {
	for _, m := range i.cfg.GangCoordination.ExtraHostPathMounts {
		if m.Name == "" || m.MountPath == "" {
			continue
		}

		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ReadOnly:  boolDefault(m.ReadOnly, true),
		})
	}
}

// appendExtraVolumeMounts appends configured extra volume mounts (for pre-existing
// pod volumes such as GCP TCPXO plugin volumes) to the container.
func (i *Injector) appendExtraVolumeMounts(container *corev1.Container) {
	for _, m := range i.cfg.GangCoordination.ExtraVolumeMounts {
		if m.Name == "" || m.MountPath == "" {
			continue
		}

		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ReadOnly:  boolDefault(m.ReadOnly, true),
		})
	}
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

	// NOTE: ConnectorSocket and ProcessingStrategy are global preflight config
	// that is incorrectly scoped under DCGMConfig. These apply to all init
	// containers, not just dcgm-diag. They will move to a top-level config
	// struct when the dcgm: block is removed (see ADR-035).
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

func (i *Injector) injectVolumes(pod *corev1.Pod, gangCtx *GangContext) []PatchOperation {
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

	if gangCtx != nil {
		volumesToAdd = append(volumesToAdd, i.collectGangVolumes(gangCtx, existingVolumes)...)
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

// collectGangVolumes gathers all gang-related volumes (ConfigMap, shared memory,
// NCCL topology, extra hostPath) that are not already present in the pod.
func (i *Injector) collectGangVolumes(
	gangCtx *GangContext,
	existingVolumes map[string]bool,
) []corev1.Volume {
	var volumes []corev1.Volume

	// ConfigMap is optional because it may not exist yet when the pod is created.
	// The controller creates it when it discovers the gang.
	// Init containers poll the mounted path until peers are registered.
	if !existingVolumes[types.GangConfigVolumeName] {
		optional := true

		volumes = append(volumes, corev1.Volume{
			Name: types.GangConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: gangCtx.ConfigMapName,
					},
					Optional: &optional,
				},
			},
		})
	}

	// Add shared memory volume for NCCL multi-GPU communication.
	// NCCL requires a larger /dev/shm than the default 64MB container limit.
	// Using emptyDir with Memory medium provides RAM-backed storage.
	// Cap at 64Gi to prevent unbounded RAM consumption on the node.
	if !existingVolumes[dshmVolumeName] {
		dshmSizeLimit := resource.MustParse("64Gi")
		volumes = append(volumes, corev1.Volume{
			Name: dshmVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &dshmSizeLimit,
				},
			},
		})
	}

	// Add NCCL topology ConfigMap volume if configured.
	// Required for Azure NDv4/v5 - NCCL needs this to map GPUs to IB NICs.
	// The ConfigMap is auto-created by the gang controller in the pod's namespace
	// when ncclTopoData is configured, or must exist if set explicitly.
	if i.cfg.GangCoordination.NCCLTopoConfigMap != "" && !existingVolumes[ncclTopoVolumeName] {
		optional := true

		volumes = append(volumes, corev1.Volume{
			Name: ncclTopoVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: i.cfg.GangCoordination.NCCLTopoConfigMap,
					},
					Optional: &optional,
				},
			},
		})
	}

	volumes = append(volumes, i.collectExtraHostPathVolumes(existingVolumes)...)

	return volumes
}

// collectExtraHostPathVolumes builds volumes for configured extra hostPath mounts
// that are not already present in the pod.
func (i *Injector) collectExtraHostPathVolumes(existingVolumes map[string]bool) []corev1.Volume {
	var volumes []corev1.Volume

	for _, m := range i.cfg.GangCoordination.ExtraHostPathMounts {
		if m.Name == "" || m.HostPath == "" || existingVolumes[m.Name] {
			continue
		}

		hostPathType, ok := parseHostPathType(m.HostPathType)
		if !ok {
			slog.Warn("Ignoring unsupported hostPathType in extraHostPathMount",
				"name", m.Name,
				"hostPathType", m.HostPathType)

			continue
		}

		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: m.HostPath,
					Type: hostPathType,
				},
			},
		})
	}

	return volumes
}

func parseHostPathType(hostPathType string) (*corev1.HostPathType, bool) {
	if hostPathType == "" {
		return nil, true
	}

	t, ok := supportedHostPathTypes[hostPathType]
	if !ok {
		return nil, false
	}

	return &t, true
}

// injectDCGMEnv injects DCGM-specific environment variables for the dcgm-diag check.
//
// Deprecated: prefer defining DCGM_DIAG_LEVEL and DCGM_HOSTENGINE_ADDR as
// inline env vars on the preflight-dcgm-diag init container in values.yaml.
// Inline env vars take precedence via mergeEnvVars (user-defined wins over
// webhook-injected). This function will be removed when the dcgm: config
// block is dropped (see ADR-035).
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

// injectGangEnv injects gang-related environment variables for multi-node checks.
func (i *Injector) injectGangEnv(container *corev1.Container, gangCtx *GangContext) {
	if gangCtx == nil {
		return
	}

	slog.Info("Injecting gang environment variables", "gangID", gangCtx.GangID, "configMap", gangCtx.ConfigMapName)

	envVars := []corev1.EnvVar{
		{
			Name:  "GANG_ID",
			Value: gangCtx.GangID,
		},
		{
			Name:  "GANG_CONFIG_DIR",
			Value: i.cfg.GangCoordination.ConfigMapMountPath,
		},
		{
			Name:  "GANG_TIMEOUT_SECONDS",
			Value: strconv.Itoa(int(i.cfg.GangCoordination.TimeoutDuration.Seconds())),
		},
		{
			Name:  "MASTER_PORT",
			Value: strconv.Itoa(i.cfg.GangCoordination.MasterPort),
		},
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
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

// collectMatchingEnvVars scans all containers for env vars whose names match
// any of the configured ncclEnvPatterns. Returns a deduplicated list (first
// occurrence wins if multiple containers set the same var).
func (i *Injector) collectMatchingEnvVars(containers []corev1.Container) []corev1.EnvVar {
	if len(i.cfg.NCCLEnvPatterns) == 0 {
		return nil
	}

	seen := make(map[string]bool)

	var result []corev1.EnvVar

	for _, c := range containers {
		for _, env := range c.Env {
			if seen[env.Name] {
				continue
			}

			if matchesAnyPattern(env.Name, i.cfg.NCCLEnvPatterns) {
				result = append(result, env)
				seen[env.Name] = true
			}
		}
	}

	return result
}

// collectMatchingVolumeMounts scans all containers for volume mounts whose
// names match any of the configured volumeMountPatterns. Returns a deduplicated
// list (first occurrence wins).
func (i *Injector) collectMatchingVolumeMounts(containers []corev1.Container) []corev1.VolumeMount {
	if len(i.cfg.VolumeMountPatterns) == 0 {
		return nil
	}

	seen := make(map[string]bool)

	var result []corev1.VolumeMount

	for _, c := range containers {
		for _, vm := range c.VolumeMounts {
			if seen[vm.Name] {
				continue
			}

			if matchesAnyPattern(vm.Name, i.cfg.VolumeMountPatterns) {
				result = append(result, vm)
				seen[vm.Name] = true
			}
		}
	}

	return result
}

// mergeVolumeMounts adds volume mounts to a container, skipping any whose
// name or mountPath already exists.
func (i *Injector) mergeVolumeMounts(container *corev1.Container, mounts []corev1.VolumeMount) {
	existingNames := make(map[string]bool)
	existingPaths := make(map[string]bool)

	for _, vm := range container.VolumeMounts {
		existingNames[vm.Name] = true
		existingPaths[vm.MountPath] = true
	}

	for _, vm := range mounts {
		if !existingNames[vm.Name] && !existingPaths[vm.MountPath] {
			container.VolumeMounts = append(container.VolumeMounts, vm)
		}
	}
}

// matchesAnyPattern returns true if name matches any of the glob patterns.
func matchesAnyPattern(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
	}

	return false
}

// boolDefault returns *ptr if non-nil, or def otherwise.
func boolDefault(ptr *bool, def bool) bool {
	if ptr != nil {
		return *ptr
	}

	return def
}
