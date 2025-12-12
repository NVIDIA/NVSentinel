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

// +kubebuilder:object:generate=true

type EntityImpacted struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value,omitempty"`
}

// +kubebuilder:object:generate=true

type DrainRequestSpec struct {
	// +kubebuilder:validation:Required
	NodeName          string              `json:"nodeName"`
	CheckName         string              `json:"checkName,omitempty"`
	RecommendedAction string              `json:"recommendedAction,omitempty"`
	ErrorCode         []string            `json:"errorCode,omitempty"`
	HealthEventID     string              `json:"healthEventID,omitempty"`
	EntitiesImpacted  []EntityImpacted    `json:"entitiesImpacted,omitempty"`
	Metadata          map[string]string   `json:"metadata,omitempty"`
	Reason            string              `json:"reason,omitempty"`
	PodsToDrain       map[string][]string `json:"podsToDrain,omitempty"`
}

// +kubebuilder:object:generate=true

type DrainRequestStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type DrainRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DrainRequestSpec   `json:"spec,omitempty"`
	Status DrainRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DrainRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DrainRequest `json:"items"`
}
