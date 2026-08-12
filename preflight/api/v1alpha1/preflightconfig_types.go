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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the condition type indicating whether the per-namespace
// preflight configuration was successfully applied.
const ConditionReady = "Ready"

// GroupVersionResource specifies a Kubernetes GroupVersionResource for a
// PodGroup-style custom resource.
type GroupVersionResource struct {
	// Group is the API group of the PodGroup resource (e.g. "scheduling.volcano.sh").
	// +optional
	Group string `json:"group,omitempty"`

	// Version is the API version of the PodGroup resource (e.g. "v1beta1").
	// +optional
	Version string `json:"version,omitempty"`

	// Resource is the plural resource name of the PodGroup resource (e.g. "podgroups").
	// +optional
	Resource string `json:"resource,omitempty"`
}

// GangDiscoverySpec defines how preflight discovers gang membership for pods in
// this object's namespace. An empty spec selects native Kubernetes gang
// discovery (the K8s 1.36+ PodGroup API, falling back to the 1.35 Workload API).
// To use a PodGroup-based scheduler (e.g. Volcano, Run:ai), set name, the
// annotation/label keys, podGroupGVR, and minCountExpr.
type GangDiscoverySpec struct {
	// Name is the discoverer identifier, used in the gang ID prefix and logging
	// (e.g. "volcano"). Leave empty for native Kubernetes discovery.
	// +optional
	Name string `json:"name,omitempty"`

	// AnnotationKeys are pod annotation keys checked, in order, for the PodGroup name.
	// +optional
	AnnotationKeys []string `json:"annotationKeys,omitempty"`

	// LabelKeys are optional pod label keys checked, in order, as a fallback.
	// +optional
	LabelKeys []string `json:"labelKeys,omitempty"`

	// PodGroupGVR specifies the PodGroup custom resource location.
	// +optional
	PodGroupGVR GroupVersionResource `json:"podGroupGVR,omitempty"`

	// MinCountExpr is a CEL expression to extract the minimum member count from
	// the PodGroup object, which receives 'podGroup' as the unstructured object
	// (e.g. "podGroup.spec.minMember").
	// +optional
	MinCountExpr string `json:"minCountExpr,omitempty"`
}

// PreflightConfigSpec defines per-namespace preflight behavior. Today it scopes
// gang discovery; additional namespace-scoped settings may be added under
// dedicated sub-fields in future API versions.
type PreflightConfigSpec struct {
	// GangDiscovery overrides the cluster-wide gang discovery configuration for
	// pods in this namespace.
	// +optional
	GangDiscovery GangDiscoverySpec `json:"gangDiscovery,omitempty"`
}

// PreflightConfigStatus reports the observed state of a PreflightConfig.
type PreflightConfigStatus struct {
	// ObservedGeneration is the .metadata.generation last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Discoverer is the resolved gang discoverer name in effect for the namespace
	// (e.g. "volcano", "kubernetes"). Empty when the config is not active.
	// +optional
	Discoverer string `json:"discoverer,omitempty"`

	// Conditions represent the latest available observations of the object's state.
	// The "Ready" condition reports whether the configuration was successfully
	// applied (its message explains why, when not ready).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=pfc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Discoverer",type="string",JSONPath=".status.discoverer"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PreflightConfig holds per-namespace preflight configuration. Pods admitted in
// this namespace use it instead of the cluster-wide defaults. At most one
// PreflightConfig should exist per namespace; if more than one is present, the
// oldest (tie-broken by name) stays active and the rest are marked not ready as
// superseded.
type PreflightConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired per-namespace preflight behavior.
	Spec PreflightConfigSpec `json:"spec,omitempty"`

	// Status reports the observed state. Populated by the controller. Read-only.
	// +optional
	Status PreflightConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreflightConfigList contains a list of PreflightConfig.
type PreflightConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreflightConfig `json:"items"`
}
