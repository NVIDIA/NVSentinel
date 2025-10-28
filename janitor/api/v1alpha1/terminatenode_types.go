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

// nolint:wsl // API types migrated from old code
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TerminateNode condition types
const (
	// TerminateNodeConditionSignalSent indicates whether the terminate signal has been sent to the CSP
	TerminateNodeConditionSignalSent = "SignalSent"
	// TerminateNodeConditionNodeTerminated indicates whether the node has been terminated
	TerminateNodeConditionNodeTerminated = "NodeTerminated"
)

// TerminateNodeSpec defines the desired state of TerminateNode
type TerminateNodeSpec struct {
	// Force indicates whether to force terminate the node
	// +kubebuilder:default:=false
	// +kubebuilder:validation:Required
	Force bool `json:"force"`

	// NodeName is the name of the node to terminate
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`
}

// TerminateNodeStatus defines the observed state of TerminateNode
type TerminateNodeStatus struct {
	// StartTime is the time when the termination was initiated
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the time when the termination was completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions represent the latest available observations of an object's current state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Force",type="boolean",JSONPath=".spec.force"
// nolint:lll
// +kubebuilder:printcolumn:name="NodeTerminated",type="string",JSONPath=".status.conditions[?(@.type=='NodeTerminated')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TerminateNode is the Schema for the terminatenodes API
type TerminateNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TerminateNodeSpec   `json:"spec,omitempty"`
	Status TerminateNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TerminateNodeList contains a list of TerminateNode
type TerminateNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerminateNode `json:"items"`
}

func (t *TerminateNode) IsTerminateInProgress() bool {
	terminateSignalSent := false
	nodeNotTerminated := false
	for _, condition := range t.Status.Conditions {
		if condition.Type == TerminateNodeConditionSignalSent && condition.Status == metav1.ConditionTrue {
			terminateSignalSent = true
		}
		if condition.Type == TerminateNodeConditionNodeTerminated && condition.Status != metav1.ConditionTrue {
			nodeNotTerminated = true
		}
	}
	return terminateSignalSent && nodeNotTerminated
}

// SetInitialConditions sets the initial conditions for the TerminateNode to Unknown state
func (t *TerminateNode) SetInitialConditions() {
	now := metav1.Now()

	// Check if conditions are already initialized
	hasSignalSent := false
	hasNodeTerminated := false

	for _, condition := range t.Status.Conditions {
		if condition.Type == TerminateNodeConditionSignalSent {
			hasSignalSent = true
		}
		if condition.Type == TerminateNodeConditionNodeTerminated {
			hasNodeTerminated = true
		}
	}

	// Only initialize conditions that don't exist yet
	if !hasSignalSent {
		signalSentCondition := metav1.Condition{
			Type:               TerminateNodeConditionSignalSent,
			Status:             metav1.ConditionUnknown,
			Reason:             "Initializing",
			Message:            "Terminate signal not yet sent",
			LastTransitionTime: now,
		}
		t.SetCondition(signalSentCondition)
	}

	if !hasNodeTerminated {
		nodeTerminatedCondition := metav1.Condition{
			Type:               TerminateNodeConditionNodeTerminated,
			Status:             metav1.ConditionUnknown,
			Reason:             "Initializing",
			Message:            "Node terminated state not yet determined",
			LastTransitionTime: now,
		}
		t.SetCondition(nodeTerminatedCondition)
	}
}

// SetCondition updates a condition only if it has changed
func (t *TerminateNode) SetCondition(newCondition metav1.Condition) {
	// find if condition exists
	for i, condition := range t.Status.Conditions {
		if condition.Type == newCondition.Type {
			// Check if the condition has actually changed
			if condition.Status == newCondition.Status &&
				condition.Reason == newCondition.Reason &&
				condition.Message == newCondition.Message {
				// No change needed, return early
				return
			}

			// condition type exists and has changed, update with new information
			t.Status.Conditions[i].Status = newCondition.Status
			t.Status.Conditions[i].LastTransitionTime = newCondition.LastTransitionTime
			t.Status.Conditions[i].Reason = newCondition.Reason
			t.Status.Conditions[i].Message = newCondition.Message
			return
		}
	}
	// condition type does not exist, add a new one
	t.Status.Conditions = append(t.Status.Conditions, newCondition)
}

// SetStartTime sets the start time to now if not set
func (t *TerminateNode) SetStartTime() {
	if t.Status.StartTime == nil {
		now := metav1.Now()
		t.Status.StartTime = &now
	}
}

// SetCompletionTime sets the completion time to now if not set
func (t *TerminateNode) SetCompletionTime() {
	if t.Status.CompletionTime == nil {
		now := metav1.Now()
		t.Status.CompletionTime = &now
	}
}

func init() {
	SchemeBuilder.Register(&TerminateNode{}, &TerminateNodeList{})
}
