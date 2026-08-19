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
	"k8s.io/apimachinery/pkg/runtime"
)

// Phase represents the lifecycle state of a ValidationRequest or test group.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
)

// FailureReason identifies why a test attempt failed.
// +kubebuilder:validation:Enum=TestFailed;TestTimeout;NodeReadinessViolation;BatchMinimumNotMet
type FailureReason string

const (
	FailureReasonTestFailed             FailureReason = "TestFailed"
	FailureReasonTestTimeout            FailureReason = "TestTimeout"
	FailureReasonNodeReadinessViolation FailureReason = "NodeReadinessViolation"
	FailureReasonBatchMinimumNotMet     FailureReason = "BatchMinimumNotMet"
)

// NodeSpec identifies a node to include in the validation run.
type NodeSpec struct {
	// Name is the node name.
	// +required
	Name string `json:"name"`
}

// ValidationRequestSpec defines the desired state of a ValidationRequest.
type ValidationRequestSpec struct {
	// Nodes lists the nodes to validate.
	// +required
	Nodes []NodeSpec `json:"nodes"`

	// Tests lists the test names to run. Each name must reference a key in
	// ValidationConfiguration.spec.tests. If empty, defaultTests from the active
	// ValidationConfiguration are used.
	// +optional
	Tests []string `json:"tests,omitempty"`
}

// AttemptStatus records the outcome of a single test execution attempt.
type AttemptStatus struct {
	// ObjectName is the name of the provider resource created for this attempt.
	// +optional
	ObjectName string `json:"objectName,omitempty"`

	// Phase is the current lifecycle state of this attempt.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// FailureReason describes why the attempt failed.
	// +optional
	FailureReason FailureReason `json:"failureReason,omitempty"`

	// FailedNodes lists nodes that caused this specific attempt to fail.
	// +optional
	FailedNodes []string `json:"failedNodes,omitempty"`

	// StartTime is when the attempt began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// EndTime is when the attempt completed.
	// +optional
	EndTime *metav1.Time `json:"endTime,omitempty"`
}

// TestGroupStatus tracks the progress of a single test group across all its attempts.
type TestGroupStatus struct {
	// Name identifies this test group.
	// +required
	Name string `json:"name"`

	// Provider is the key of the provider in the ValidationConfiguration used to create this group's
	// provider resources. This allows fetching each AttemptStatus.ObjectName by GVK.
	// +required
	Provider string `json:"provider"`

	// Phase is the current lifecycle state of this test group.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Attempts records each execution attempt for this group.
	// +optional
	Attempts []AttemptStatus `json:"attempts,omitempty"`
}

// SkippedStatus records nodes and tests that were skipped during validation.
type SkippedStatus struct {
	// Nodes lists nodes that were skipped because they no longer exist.
	// +optional
	Nodes []string `json:"nodes,omitempty"`

	// Tests lists tests that were skipped due to unmet batch minimums with
	// batchFailurePolicy: ignore.
	// +optional
	Tests []string `json:"tests,omitempty"`
}

// ValidationRequestStatus defines the observed state of a ValidationRequest.
type ValidationRequestStatus struct {
	// Phase is the overall lifecycle state of the ValidationRequest.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// TestGroups tracks progress of each test group.
	// +optional
	TestGroups []TestGroupStatus `json:"testGroups,omitempty"`

	// Skipped records nodes and tests that were omitted from execution.
	// +optional
	Skipped *SkippedStatus `json:"skipped,omitempty"`

	// StartTime is when reconciliation began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the ValidationRequest reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions reflect the current status of the ValidationRequest using standard Kubernetes condition types.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ValidationRequest is the Schema for the ValidationRequests API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ValidationRequest struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state.
	// +required
	Spec ValidationRequestSpec `json:"spec"`

	// Status defines the observed state.
	// +optional
	Status ValidationRequestStatus `json:"status,omitempty"`
}

// ValidationRequestList contains a list of ValidationRequests.
// +kubebuilder:object:root=true
type ValidationRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ValidationRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ValidationRequest{}, &ValidationRequestList{})
		return nil
	})
}
