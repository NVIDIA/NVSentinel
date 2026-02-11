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

// Package gang provides gang scheduling discovery and coordination for multi-node workloads.
package gang

import (
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/coordinator"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/discoverer"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Re-export types for convenience.
type (
	PeerInfo          = types.PeerInfo
	GangInfo          = types.GangInfo
	GangDiscoverer    = types.GangDiscoverer
	Coordinator       = coordinator.Coordinator
	CoordinatorConfig = coordinator.CoordinatorConfig
)

// Re-export coordinator functions.
var (
	ConfigMapName            = coordinator.ConfigMapName
	NewCoordinator           = coordinator.NewCoordinator
	DefaultCoordinatorConfig = coordinator.DefaultCoordinatorConfig
	ParsePeers               = coordinator.ParsePeers
	GetRank                  = coordinator.GetRank
)

type discoveryType int

const (
	discoveryTypeInvalid discoveryType = iota
	discoveryTypeKubernetes
	discoveryTypePodGroup
)

// NewDiscovererFromConfig creates a gang discoverer from configuration.
func NewDiscovererFromConfig(
	cfg config.GangDiscoveryConfig,
	c client.Client,
	restMapper meta.RESTMapper,
) (GangDiscoverer, error) {
	switch detectDiscoveryType(cfg) {
	case discoveryTypeKubernetes:
		if err := validateGVK(restMapper, discoverer.WorkloadGVK); err != nil {
			return nil, fmt.Errorf("kubernetes native Workload API not available (requires K8s 1.35+): %w", err)
		}

		return discoverer.NewWorkloadRefDiscoverer(c), nil

	case discoveryTypePodGroup:
		gvr := schema.GroupVersionResource{
			Group:    cfg.PodGroupGVR.Group,
			Version:  cfg.PodGroupGVR.Version,
			Resource: cfg.PodGroupGVR.Resource,
		}

		gvk, err := resolveGVK(restMapper, gvr)
		if err != nil {
			return nil, fmt.Errorf("gangDiscovery.podGroupGVR validation failed: %w", err)
		}

		return newPodGroupDiscoverer(cfg, c, gvk)

	case discoveryTypeInvalid:
		return nil, fmt.Errorf(
			"invalid gangDiscovery config: name %q requires annotationKeys/labelKeys, podGroupGVR, and minCountExpr",
			cfg.Name,
		)
	}

	return nil, fmt.Errorf("unknown discovery type for config: %+v", cfg)
}

// detectDiscoveryType determines the discovery type from config.
func detectDiscoveryType(cfg config.GangDiscoveryConfig) discoveryType {
	if isEmptyConfig(cfg) {
		return discoveryTypeKubernetes
	}

	if isCompletePodGroupConfig(cfg) {
		return discoveryTypePodGroup
	}

	return discoveryTypeInvalid
}

func isEmptyConfig(cfg config.GangDiscoveryConfig) bool {
	return cfg.Name == "" &&
		len(cfg.AnnotationKeys) == 0 &&
		len(cfg.LabelKeys) == 0 &&
		cfg.PodGroupGVR.Group == "" &&
		cfg.PodGroupGVR.Version == "" &&
		cfg.PodGroupGVR.Resource == "" &&
		cfg.MinCountExpr == ""
}

func isCompletePodGroupConfig(cfg config.GangDiscoveryConfig) bool {
	hasName := cfg.Name != ""
	hasKeys := len(cfg.AnnotationKeys) > 0 || len(cfg.LabelKeys) > 0
	hasFullGVR := cfg.PodGroupGVR.Group != "" && cfg.PodGroupGVR.Version != "" && cfg.PodGroupGVR.Resource != ""
	hasMinCountExpr := cfg.MinCountExpr != ""

	return hasName && hasKeys && hasFullGVR && hasMinCountExpr
}

func newPodGroupDiscoverer(
	cfg config.GangDiscoveryConfig,
	c client.Client,
	gvk schema.GroupVersionKind,
) (GangDiscoverer, error) {
	podGroupConfig := discoverer.PodGroupConfig{
		Name:           cfg.Name,
		AnnotationKeys: cfg.AnnotationKeys,
		LabelKeys:      cfg.LabelKeys,
		PodGroupGVK:    gvk,
		MinCountExpr:   cfg.MinCountExpr,
	}

	return discoverer.NewPodGroupDiscoverer(c, podGroupConfig)
}

// resolveGVK converts a GVR to GVK using the RESTMapper, validating the resource exists.
func resolveGVK(restMapper meta.RESTMapper, gvr schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	gvk, err := restMapper.KindFor(gvr)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("resource %q not found (is the CRD installed?): %w", gvr, err)
	}

	slog.Info("Resolved GVK from GVR",
		"group", gvk.Group,
		"version", gvk.Version,
		"kind", gvk.Kind)

	return gvk, nil
}

// validateGVK checks if the specified GVK exists in the cluster.
func validateGVK(restMapper meta.RESTMapper, gvk schema.GroupVersionKind) error {
	_, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("resource %q not found: %w", gvk, err)
	}

	slog.Info("Validated GVK exists",
		"group", gvk.Group,
		"version", gvk.Version,
		"kind", gvk.Kind)

	return nil
}
