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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUResetPhase is a label for the condition of a GPUReset at the current time.
type GPUResetPhase string

// GPUResetConditionType are the valid conditions of a GPUReset.
type GPUResetConditionType string

// GPUResetReason are the valid reasons for a GPUReset's transition to a condition.
type GPUResetReason string

const (
	ResetPending     GPUResetPhase = "Pending"
	ResetInProgress  GPUResetPhase = "InProgress"
	ResetTerminating GPUResetPhase = "Terminating"
	ResetSucceeded   GPUResetPhase = "Succeeded"
	ResetFailed      GPUResetPhase = "Failed"
	ResetUnknown     GPUResetPhase = "Unknown"
)

const (
	Ready             GPUResetConditionType = "Ready"
	ServicesTornDown  GPUResetConditionType = "ServicesTornDown"
	ResetJobCreated   GPUResetConditionType = "ResetJobCreated"
	ResetJobCompleted GPUResetConditionType = "ResetJobCompleted"
	ServicesRestored  GPUResetConditionType = "ServicesRestored"
	Terminating       GPUResetConditionType = "Terminating"
	Complete          GPUResetConditionType = "Complete"
)

const (
	// Pending reasons
	ReasonResetPending       GPUResetReason = "Pending"
	ReasonReadyForReset      GPUResetReason = "ReadyForReset"
	ReasonResourceContention GPUResetReason = "ResourceContention"

	// In-progress reasons
	ReasonSkipped                    GPUResetReason = "Skipped"
	ReasonTearingDownServices        GPUResetReason = "TearingDownServices"
	ReasonCreatingResetJob           GPUResetReason = "CreatingResetJob"
	ReasonResetInProgress            GPUResetReason = "ResettingGPU"
	ReasonResetJobRunning            GPUResetReason = "ResetJobRunning"
	ReasonRestoringServices          GPUResetReason = "RestoringServices"
	ReasonFinalizerRestoringServices GPUResetReason = "FinalizerRestoringServices"

	// Success reasons
	ReasonServiceTeardownSucceeded  GPUResetReason = "ServiceTeardownSucceeded"
	ReasonResetJobCreationSucceeded GPUResetReason = "ResetJobCreationSucceeded"
	ReasonResetJobSucceeded         GPUResetReason = "ResetJobSucceeded"
	ReasonServiceRestoreSucceeded   GPUResetReason = "ServiceRestoreSucceeded"
	// Final success
	ReasonGPUResetSucceeded GPUResetReason = "GPUResetSucceeded"

	// Failure reasons
	NodeNotFound                         GPUResetReason = "NodeNotFound"
	ReasonInvalidConfig                  GPUResetReason = "InvalidConfig"
	ReasonServiceTeardownTimeoutExceeded GPUResetReason = "ServiceTeardownTimeoutExceeded"
	ReasonResetJobCreationFailed         GPUResetReason = "ResetJobCreationFailed"
	ReasonResetJobNotFound               GPUResetReason = "ResetJobNotFound"
	ReasonResetJobFailed                 GPUResetReason = "ResetJobFailed"
	ReasonServiceRestoreFailed           GPUResetReason = "ServiceRestoreFailed"
	ReasonRestoreTimeoutExceeded         GPUResetReason = "RestoreTimeoutExceeded"
	ReasonInternalError                  GPUResetReason = "InternalError"
)

// GPUSelector allows specifying GPUs by different identifier types.
// A single selector can include multiple identifier types. All specified GPUs
// across all lists will be targeted for reset.
// +kubebuilder:object:root=false
type GPUSelector struct {
	// UUIDs is a list of GPU UUIDs.
	//nolint:lll // kubebuilder validation pattern must stay on one line
	// +kubebuilder:validation:items:Pattern="^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
	// +kubebuilder:validation:Optional
	// +optional
	UUIDs []string `json:"uuids,omitempty"`

	// PCIBusIDs is a list of GPU PCI bus IDs.
	// Format: "domain:bus:device.function" (e.g., "0000:01:00.0").
	// +kubebuilder:validation:items:Pattern="^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\\.[0-9a-fA-F]{1}$"
	// +kubebuilder:validation:Optional
	// +optional
	PCIBusIDs []string `json:"pciBusIDs,omitempty"`
}

// GPUResetSpec defines the desired GPU reset operation.
type GPUResetSpec struct {
	// NodeName identifies the node which contains the GPUs to reset.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`

	// Selector is used to target one or more specific devices to reset.
	// If this field is omitted or empty, all GPUs on the node will be reset.
	// +kubebuilder:validation:Optional
	// +optional
	Selector *GPUSelector `json:"selector,omitempty"`
}

// GPUResetStatus represents information about the status of a GPU reset. Status may trail the actual
// state of a system, especially if the node that hosts the reset job cannot contact the control plane.
type GPUResetStatus struct {
	// The phase of a GPUReset is a simple, high-level summary of where the reset operation is in its lifecycle.
	// The conditions array, the reason and message fields, and the individual reset job contain
	// more detail about the reset status.
	// +kubebuilder:validation:Optional
	// +optional
	Phase GPUResetPhase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of a reset operations state.
	// +kubebuilder:validation:Optional
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RFC 3339 date and time at which the object was acknowledged by the GPUReset controller.
	// +kubebuilder:validation:Optional
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// RFC 3339 date and time at which the reset operation finished, regardless of the outcome (success or failure).
	// +kubebuilder:validation:Optional
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// JobRef is a reference to the job that will execute the GPU reset operation.
	// +kubebuilder:validation:Optional
	// +optional
	JobRef *v1.ObjectReference `json:"jobRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName",description="Target node for GPU reset"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase",description="Current status of reset"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// GPUReset is the Schema for the gpuresets API.
type GPUReset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of the GPU reset operation.
	Spec GPUResetSpec `json:"spec,omitempty"`

	// Most recently observed status of the GPU reset operation.
	// This data may not be up to date.
	// Populated by the system.
	// Read-only.
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
