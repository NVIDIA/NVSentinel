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

// RebootNode condition types
const (
	// RebootNodeConditionSignalSent indicates whether the reboot signal has been sent to the CSP
	RebootNodeConditionSignalSent = "SignalSent"
	// RebootNodeConditionNodeReady indicates whether the node has returned to ready state after reboot
	RebootNodeConditionNodeReady = "NodeReady"
	// ManualModeConditionType indicates that manual mode is enabled and outside actor is required
	ManualModeConditionType = "ManualMode"
)

// RebootNodeSpec defines the desired state of RebootNode
type RebootNodeSpec struct {
	// Force indicates whether to force reboot the node
	// +kubebuilder:default:=false
	// +kubebuilder:validation:Required
	Force bool `json:"force"`

	// NodeName is the name of the node to reboot
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`
}

// RebootNodeStatus defines the observed state of RebootNode
type RebootNodeStatus struct {
	// StartTime is the time when the reboot was initiated
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the time when the reboot was completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// RetryCount tracks the number of reconciliation attempts for this reboot operation
	// Used to implement maximum retry limits to prevent indefinite reconciliation
	RetryCount int32 `json:"retryCount,omitempty"`

	// ConsecutiveFailures tracks consecutive CSP operation failures for exponential backoff
	// Reset to 0 on successful operations
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// Conditions represent the latest available observations of an object's current state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Force",type="boolean",JSONPath=".spec.force"
// +kubebuilder:printcolumn:name="NodeReady",type="string",JSONPath=".status.conditions[?(@.type=='NodeReady')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// RebootNode is the Schema for the rebootnodes API
type RebootNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RebootNodeSpec   `json:"spec,omitempty"`
	Status RebootNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RebootNodeList contains a list of RebootNode
type RebootNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RebootNode `json:"items"`
}

func (r *RebootNode) IsRebootInProgress() bool {
	rebootSignalSent := false
	nodeNotReady := false
	for _, condition := range r.Status.Conditions {
		if condition.Type == RebootNodeConditionSignalSent && condition.Status == metav1.ConditionTrue {
			rebootSignalSent = true
		}
		if condition.Type == RebootNodeConditionNodeReady && condition.Status != metav1.ConditionTrue {
			nodeNotReady = true
		}
	}
	return rebootSignalSent && nodeNotReady
}

func (r *RebootNode) GetCSPReqRef() string {
	for _, condition := range r.Status.Conditions {
		if condition.Type == RebootNodeConditionSignalSent {
			return condition.Message
		}
	}
	return ""
}

// SetInitialConditions sets the initial conditions for the RebootNode to Unknown state
func (r *RebootNode) SetInitialConditions() {
	now := metav1.Now()

	// Check if conditions are already initialized
	hasSignalSent := false
	hasNodeReady := false

	for _, condition := range r.Status.Conditions {
		if condition.Type == RebootNodeConditionSignalSent {
			hasSignalSent = true
		}
		if condition.Type == RebootNodeConditionNodeReady {
			hasNodeReady = true
		}
	}

	// Only initialize conditions that don't exist yet
	if !hasSignalSent {
		signalSentCondition := metav1.Condition{
			Type:               RebootNodeConditionSignalSent,
			Status:             metav1.ConditionUnknown,
			Reason:             "Initializing",
			Message:            "Reboot signal not yet sent",
			LastTransitionTime: now,
		}
		r.SetCondition(signalSentCondition)
	}

	if !hasNodeReady {
		nodeReadyCondition := metav1.Condition{
			Type:               RebootNodeConditionNodeReady,
			Status:             metav1.ConditionUnknown,
			Reason:             "Initializing",
			Message:            "Node ready state not yet determined",
			LastTransitionTime: now,
		}
		r.SetCondition(nodeReadyCondition)
	}
}

// SetCondition updates a condition only if it has changed
func (r *RebootNode) SetCondition(newCondition metav1.Condition) {
	// find if condition exists
	for i, condition := range r.Status.Conditions {
		if condition.Type == newCondition.Type {
			// Check if the condition has actually changed
			if condition.Status == newCondition.Status &&
				condition.Reason == newCondition.Reason &&
				condition.Message == newCondition.Message {
				// No change needed, return early
				return
			}

			// condition type exists and has changed, update with new information
			r.Status.Conditions[i].Status = newCondition.Status
			r.Status.Conditions[i].LastTransitionTime = newCondition.LastTransitionTime
			r.Status.Conditions[i].Reason = newCondition.Reason
			r.Status.Conditions[i].Message = newCondition.Message
			return
		}
	}
	// condition type does not exist, add a new one
	r.Status.Conditions = append(r.Status.Conditions, newCondition)
}

// SetStartTime sets the start time to now if not set
func (r *RebootNode) SetStartTime() {
	if r.Status.StartTime == nil {
		now := metav1.Now()
		r.Status.StartTime = &now
	}
}

// SetCompletionTime sets the completion time to now if not set
func (r *RebootNode) SetCompletionTime() {
	if r.Status.CompletionTime == nil {
		now := metav1.Now()
		r.Status.CompletionTime = &now
	}
}

// Interface implementation for generic status update handling

// GetRetryCount returns the retry count
func (s *RebootNodeStatus) GetRetryCount() int32 {
	return s.RetryCount
}

// GetConsecutiveFailures returns the consecutive failures count
func (s *RebootNodeStatus) GetConsecutiveFailures() int32 {
	return s.ConsecutiveFailures
}

// GetStartTime returns the start time
func (s *RebootNodeStatus) GetStartTime() *metav1.Time {
	return s.StartTime
}

// GetCompletionTime returns the completion time
func (s *RebootNodeStatus) GetCompletionTime() *metav1.Time {
	return s.CompletionTime
}

// GetConditions returns the conditions
func (s *RebootNodeStatus) GetConditions() []metav1.Condition {
	return s.Conditions
}

func init() {
	SchemeBuilder.Register(&RebootNode{}, &RebootNodeList{})
}
