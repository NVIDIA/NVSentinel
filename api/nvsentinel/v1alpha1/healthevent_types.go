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

// =============================================================================
// HealthEvent CRD
// =============================================================================

// HealthEvent represents a GPU health event detected by a health monitor.
// It replaces the MongoDB HealthEventWithStatus document and enables
// Kubernetes-native coordination between fault-quarantine, node-drainer,
// and remediation controllers.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=he;hevt
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Fatal",type=boolean,JSONPath=`.spec.isFatal`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HealthEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HealthEventSpec   `json:"spec,omitempty"`
	Status HealthEventStatus `json:"status,omitempty"`
}

// HealthEventList contains a list of HealthEvent resources.
//
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type HealthEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HealthEvent `json:"items"`
}

// =============================================================================
// Spec (Immutable - what happened)
// =============================================================================

// HealthEventSpec defines the immutable details of a health event.
// These fields are set when the event is created and should not be modified.
//
// +k8s:deepcopy-gen=true
type HealthEventSpec struct {
	// Source identifies the health monitor that detected this event.
	// Examples: "nvml-health-monitor", "dcgm", "csp-health-monitor", "syslog-monitor"
	// +kubebuilder:validation:Required
	Source string `json:"source"`

	// Agent is the name of the agent that reported the event (for compatibility with protobuf).
	// +optional
	Agent string `json:"agent,omitempty"`

	// NodeName is the Kubernetes node where the event occurred.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// ComponentClass identifies the type of component affected.
	// Examples: "GPU", "NIC", "Storage", "Memory"
	// +kubebuilder:validation:Required
	ComponentClass string `json:"componentClass"`

	// CheckName identifies the specific health check that detected the issue.
	// Examples: "xid-error-check", "memory-ecc-check", "thermal-check"
	// +kubebuilder:validation:Required
	CheckName string `json:"checkName"`

	// IsFatal indicates whether this event requires immediate node quarantine.
	// Fatal events trigger the full fault-handling workflow.
	// +kubebuilder:default=false
	IsFatal bool `json:"isFatal"`

	// IsHealthy indicates the current health state.
	// false = unhealthy (problem detected), true = healthy (recovered)
	// +kubebuilder:default=false
	IsHealthy bool `json:"isHealthy"`

	// Message provides a human-readable description of the event.
	// +optional
	Message string `json:"message,omitempty"`

	// RecommendedAction suggests what action should be taken.
	// +kubebuilder:validation:Enum=NONE;COMPONENT_RESET;CONTACT_SUPPORT;RUN_FIELDDIAG;RESTART_VM;RESTART_BM;REPLACE_VM;RUN_DCGMEUD;UNKNOWN
	// +kubebuilder:default=NONE
	RecommendedAction RecommendedAction `json:"recommendedAction,omitempty"`

	// ErrorCodes contains the error codes associated with this event.
	// For GPU events, these are typically XID error codes (e.g., "79", "31").
	// +optional
	ErrorCodes []string `json:"errorCodes,omitempty"`

	// EntitiesImpacted lists the specific entities affected by this event.
	// +optional
	EntitiesImpacted []Entity `json:"entitiesImpacted,omitempty"`

	// DetectedAt is the timestamp when the event was first detected.
	// +kubebuilder:validation:Required
	DetectedAt metav1.Time `json:"detectedAt"`

	// Overrides allows customizing the fault-handling behavior for this event.
	// +optional
	Overrides *BehaviourOverrides `json:"overrides,omitempty"`

	// Metadata contains additional key-value data about the event.
	// Common keys: "uuid", "temperature", "serialNumber"
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`

	// Maintenance contains CSP maintenance event details.
	// Only populated when Source is "csp-health-monitor".
	// +optional
	Maintenance *MaintenanceInfo `json:"maintenance,omitempty"`
}

// =============================================================================
// Status (Mutable - processing state)
// =============================================================================

// HealthEventStatus defines the current processing state of a health event.
// These fields are updated by controllers as the event moves through the workflow.
//
// +k8s:deepcopy-gen=true
type HealthEventStatus struct {
	// Phase represents the current state in the fault-handling workflow.
	// +kubebuilder:validation:Enum=New;Quarantined;Draining;Drained;Remediating;Remediated;Resolved;Cancelled
	// +kubebuilder:default=New
	Phase HealthEventPhase `json:"phase,omitempty"`

	// Conditions provide detailed status for each stage of processing.
	// +optional
	Conditions []HealthEventCondition `json:"conditions,omitempty"`

	// LastRemediationTime is when remediation was last attempted.
	// +optional
	LastRemediationTime *metav1.Time `json:"lastRemediationTime,omitempty"`

	// ResolvedAt is when the event reached the Resolved phase.
	// Used for TTL calculation.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`

	// ObservedGeneration is the generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// =============================================================================
// Enums and Types
// =============================================================================

// HealthEventPhase represents the current phase of health event processing.
// +kubebuilder:validation:Enum=New;Quarantined;Draining;Drained;Remediating;Remediated;Resolved;Cancelled
type HealthEventPhase string

const (
	// PhaseNew indicates the event was just created and needs processing.
	PhaseNew HealthEventPhase = "New"

	// PhaseQuarantined indicates the node has been cordoned.
	PhaseQuarantined HealthEventPhase = "Quarantined"

	// PhaseDraining indicates pods are being evicted from the node.
	PhaseDraining HealthEventPhase = "Draining"

	// PhaseDrained indicates all user pods have been evicted.
	PhaseDrained HealthEventPhase = "Drained"

	// PhaseRemediating indicates remediation is in progress.
	PhaseRemediating HealthEventPhase = "Remediating"

	// PhaseRemediated indicates remediation completed.
	PhaseRemediated HealthEventPhase = "Remediated"

	// PhaseResolved indicates the event has been fully handled and GPU is healthy.
	PhaseResolved HealthEventPhase = "Resolved"

	// PhaseCancelled indicates the event was cancelled (e.g., override skip=true).
	PhaseCancelled HealthEventPhase = "Cancelled"
)

// RecommendedAction indicates the suggested remediation action.
// +kubebuilder:validation:Enum=NONE;COMPONENT_RESET;CONTACT_SUPPORT;RUN_FIELDDIAG;RESTART_VM;RESTART_BM;REPLACE_VM;RUN_DCGMEUD;UNKNOWN
type RecommendedAction string

const (
	ActionNone           RecommendedAction = "NONE"
	ActionComponentReset RecommendedAction = "COMPONENT_RESET"
	ActionContactSupport RecommendedAction = "CONTACT_SUPPORT"
	ActionRunFieldDiag   RecommendedAction = "RUN_FIELDDIAG"
	ActionRestartVM      RecommendedAction = "RESTART_VM"
	ActionRestartBM      RecommendedAction = "RESTART_BM"
	ActionReplaceVM      RecommendedAction = "REPLACE_VM"
	ActionRunDCGMEUD     RecommendedAction = "RUN_DCGMEUD"
	ActionUnknown        RecommendedAction = "UNKNOWN"
)

// Entity represents an impacted entity (GPU, NIC, etc.).
type Entity struct {
	// Type identifies the kind of entity (e.g., "GPU", "NIC").
	Type string `json:"type"`

	// Value is the identifier for the entity (e.g., GPU UUID).
	Value string `json:"value"`
}

// BehaviourOverrides allows customizing fault-handling behavior.
type BehaviourOverrides struct {
	// Quarantine overrides control node cordoning behavior.
	// +optional
	Quarantine *ActionOverride `json:"quarantine,omitempty"`

	// Drain overrides control pod eviction behavior.
	// +optional
	Drain *ActionOverride `json:"drain,omitempty"`
}

// ActionOverride specifies how to override a fault-handling action.
type ActionOverride struct {
	// Force causes the action to proceed even if conditions aren't met.
	// +kubebuilder:default=false
	Force bool `json:"force,omitempty"`

	// Skip causes the action to be skipped entirely.
	// +kubebuilder:default=false
	Skip bool `json:"skip,omitempty"`
}

// MaintenanceInfo contains CSP maintenance event details.
// Only populated for events from csp-health-monitor.
type MaintenanceInfo struct {
	// CSP identifies the cloud service provider.
	// +kubebuilder:validation:Enum=aws;gcp
	CSP string `json:"csp"`

	// EventID is the CSP's identifier for this maintenance event.
	EventID string `json:"eventId"`

	// MaintenanceType indicates if this is scheduled or unscheduled.
	// +kubebuilder:validation:Enum=SCHEDULED;UNSCHEDULED
	MaintenanceType string `json:"maintenanceType"`

	// CSPStatus is the status reported by the CSP.
	// +optional
	CSPStatus string `json:"cspStatus,omitempty"`

	// ScheduledStartTime is when maintenance is scheduled to begin.
	// +optional
	ScheduledStartTime *metav1.Time `json:"scheduledStartTime,omitempty"`

	// ScheduledEndTime is when maintenance is scheduled to end.
	// +optional
	ScheduledEndTime *metav1.Time `json:"scheduledEndTime,omitempty"`

	// ActualStartTime is when maintenance actually started.
	// +optional
	ActualStartTime *metav1.Time `json:"actualStartTime,omitempty"`

	// ActualEndTime is when maintenance actually ended.
	// +optional
	ActualEndTime *metav1.Time `json:"actualEndTime,omitempty"`
}

// =============================================================================
// Conditions
// =============================================================================

// HealthEventCondition represents a condition of a HealthEvent.
// Follows the standard Kubernetes condition pattern.
type HealthEventCondition struct {
	// Type is the type of condition.
	// +kubebuilder:validation:Enum=Detected;NodeQuarantined;PodsDrained;Remediated;Resolved
	Type HealthEventConditionType `json:"type"`

	// Status is the status of the condition (True, False, Unknown).
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status metav1.ConditionStatus `json:"status"`

	// LastTransitionTime is the last time the condition transitioned.
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`

	// Reason is a brief machine-readable reason for the condition.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable description of the condition.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation of the resource when this condition was set.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// HealthEventConditionType is the type of HealthEvent condition.
// +kubebuilder:validation:Enum=Detected;NodeQuarantined;PodsDrained;Remediated;Resolved
type HealthEventConditionType string

const (
	// ConditionDetected indicates the event was detected by a health monitor.
	ConditionDetected HealthEventConditionType = "Detected"

	// ConditionNodeQuarantined indicates the node has been cordoned.
	ConditionNodeQuarantined HealthEventConditionType = "NodeQuarantined"

	// ConditionPodsDrained indicates user pods have been evicted.
	ConditionPodsDrained HealthEventConditionType = "PodsDrained"

	// ConditionRemediated indicates remediation has completed.
	ConditionRemediated HealthEventConditionType = "Remediated"

	// ConditionResolved indicates the event has been fully resolved.
	ConditionResolved HealthEventConditionType = "Resolved"
)

