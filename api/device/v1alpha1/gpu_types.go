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

// GPUSpec defines the desired state of the GPU.
//
// +k8s:deepcopy-gen=true
type GPUSpec struct {
	// UUID is the physical hardware UUID of the GPU.
	//
	// Format: 'GPU-<hex-string>'
	// (e.g., 'GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6').
	UUID string `json:"uuid"`
}

// GPUStatus defines the observed state of the GPU.
//
// +k8s:deepcopy-gen=true
type GPUStatus struct {
	// Conditions is an array of current gpu conditions.
	//
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RecommendedAction is a suggestion given the current state.
	//
	// +optional
	RecommendedAction string `json:"recommendedAction,omitempty"`
}

// GPU represents a single GPU resource.
//
// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=get,list,watch
// +genclient:noStatus
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type GPU struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUSpec   `json:"spec,omitempty"`
	Status GPUStatus `json:"status,omitempty"`
}

// GPUList contains a list of GPU resources.
//
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type GPUList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPU `json:"items"`
}
