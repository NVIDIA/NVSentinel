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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUSelector allows specifying GPUs by different identifier types.
// A single selector can include multiple identifier types. All specified GPUs
// across all lists will be targeted for reset.
// +kubebuilder:object:root=false
type GPUSelector struct {
	// UUIDs is a list of GPU UUIDs.
	// +optional
	//nolint:lll // kubebuilder validation pattern
	// +kubebuilder:validation:items:Pattern="^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
	UUIDs []string `json:"uuids,omitempty"`

	// PCIBusIDs is a list of GPU PCI bus IDs.
	// Format: "domain:bus:device.function" (e.g., "0000:01:00.0").
	// +optional
	//nolint:lll // kubebuilder validation pattern
	// +kubebuilder:validation:items:Pattern="^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\\.[0-9a-fA-F]{1}$"
	PCIBusIDs []string `json:"pciBusIDs,omitempty"`
}

// GPUResetSpec defines the desired state of GPUReset.
type GPUResetSpec struct {
	// NodeName identifies the node which contains the GPUs to reset.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`

	// Selector is used to target one or more specific devices to reset.
	// If this field is omitted or empty, all GPUs on the node will be reset.
	// +optional
	Selector *GPUSelector `json:"selector,omitempty"`
}

// GPUResetStatus defines the observed state of GPUReset.
type GPUResetStatus struct {
	// StartTime is the time at which the reset operation began processing.
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the time at which the reset operation finished,
	// regardless of the outcome (success or failure).
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions represent the latest available observations of an object's current state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	// GPUResetConditionGPUReset indicates whether the GPU reset operation has completed
	GPUResetConditionGPUReset = "GPUReset"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
//nolint:lll // kubebuilder printcolumn marker
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName",description="The target node for the GPU reset"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// GPUReset is the Schema for the gpuresets API.
type GPUReset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUResetSpec   `json:"spec,omitempty"`
	Status GPUResetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUResetList contains a list of GPUReset.
type GPUResetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUReset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUReset{}, &GPUResetList{})
}

// SetInitialConditions sets the initial conditions for the GPUReset
func (g *GPUReset) SetInitialConditions() {
	if len(g.Status.Conditions) == 0 {
		g.Status.Conditions = []metav1.Condition{
			{
				Type:               GPUResetConditionGPUReset,
				Status:             metav1.ConditionFalse,
				Reason:             "Pending",
				Message:            "GPU reset operation is pending",
				LastTransitionTime: metav1.Now(),
			},
		}
	}
}

// SetStartTime sets the start time if not already set
func (g *GPUReset) SetStartTime() {
	if g.Status.StartTime == nil {
		now := metav1.Now()
		g.Status.StartTime = &now
	}
}

// SetCompletionTime sets the completion time
func (g *GPUReset) SetCompletionTime() {
	if g.Status.CompletionTime == nil {
		now := metav1.Now()
		g.Status.CompletionTime = &now
	}
}

// SetCondition sets or updates a condition
func (g *GPUReset) SetCondition(condition metav1.Condition) {
	for i, existingCondition := range g.Status.Conditions {
		if existingCondition.Type == condition.Type {
			g.Status.Conditions[i] = condition
			return
		}
	}

	g.Status.Conditions = append(g.Status.Conditions, condition)
}

// IsResetInProgress returns true if the GPU reset is in progress
func (g *GPUReset) IsResetInProgress() bool {
	for _, condition := range g.Status.Conditions {
		if condition.Type == GPUResetConditionGPUReset {
			return condition.Status == metav1.ConditionFalse && condition.Reason == "InProgress"
		}
	}

	return false
}

// GetCSPReqRef returns a reference string for CSP tracking
func (g *GPUReset) GetCSPReqRef() string {
	return string(g.UID)
}
