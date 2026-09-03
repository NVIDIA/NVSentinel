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
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// MRGroupVersion is the GroupVersion used by MaintenanceRequest.
// MR lives in the nvsentinel.dgxc.nvidia.com API group, the same group
// as ExternalRemediationRequest.
//
// The version is "v1" because protoc-gen-crd hardcodes that version.
// Despite the v1 version string, the MR API is still pre-stable;
// ADR-051 governs its lifecycle.
var MRGroupVersion = schema.GroupVersion{Group: managed.MRAPIGroup, Version: managed.MRVersion}

// MRSchemeBuilder is the SchemeBuilder for the nvsentinel.dgxc.nvidia.com
// API group. It is independent of the SchemeBuilder for nvsentinel.nvidia.com
// (defined in groupversion_info.go) so the two groups can be registered
// independently.
var MRSchemeBuilder = runtime.NewSchemeBuilder(addMRKnownTypes)

// AddMRToScheme adds the nvsentinel.dgxc.nvidia.com/v1 MR types to the
// given scheme.
var AddMRToScheme = MRSchemeBuilder.AddToScheme

func addMRKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(
		MRGroupVersion,
		&MaintenanceRequest{},
		&MaintenanceRequestList{},
	)
	metav1.AddToGroupVersion(scheme, MRGroupVersion)

	return nil
}

// MaintenanceRequest is the Kubernetes API wrapper for the proto-generated
// MaintenanceRequestSpec/Status types.
//
// Per ADR-051, the CRD schema is generated from proto (via protoc-gen-crd)
// and the Spec/Status field shapes are owned entirely by the .proto file.
// This wrapper exists only to provide the Kubernetes machinery (TypeMeta,
// ObjectMeta, runtime.Object interface) that the proto-generated types do
// not supply on their own.
type MaintenanceRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   *protos.MaintenanceRequestSpec   `json:"spec,omitempty"`
	Status *protos.MaintenanceRequestStatus `json:"status,omitempty"`
}

// MaintenanceRequestList is the list wrapper, required by the
// runtime.Object interface for list operations.
type MaintenanceRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceRequest `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (m *MaintenanceRequest) DeepCopyObject() runtime.Object {
	if m == nil {
		return nil
	}

	out := new(MaintenanceRequest)
	m.DeepCopyInto(out)

	return out
}

// DeepCopy returns a deep copy of the receiver.
func (m *MaintenanceRequest) DeepCopy() *MaintenanceRequest {
	if m == nil {
		return nil
	}

	return m.DeepCopyObject().(*MaintenanceRequest)
}

// DeepCopyInto copies the receiver into out. Spec and Status use
// proto.Clone for deep-copy, which is the canonical way to deep-copy
// a protobuf message. Nil sources explicitly nil the corresponding
// field in out so callers that pass a dirty out (e.g. informer
// machinery) get a faithful copy.
func (m *MaintenanceRequest) DeepCopyInto(out *MaintenanceRequest) {
	out.TypeMeta = m.TypeMeta
	m.ObjectMeta.DeepCopyInto(&out.ObjectMeta)

	if m.Spec != nil {
		out.Spec = proto.Clone(m.Spec).(*protos.MaintenanceRequestSpec)
	} else {
		out.Spec = nil
	}

	if m.Status != nil {
		out.Status = proto.Clone(m.Status).(*protos.MaintenanceRequestStatus)
	} else {
		out.Status = nil
	}
}

// DeepCopyObject implements runtime.Object.
func (l *MaintenanceRequestList) DeepCopyObject() runtime.Object {
	if l == nil {
		return nil
	}

	out := new(MaintenanceRequestList)
	l.DeepCopyInto(out)

	return out
}

// DeepCopy returns a deep copy of the receiver.
func (l *MaintenanceRequestList) DeepCopy() *MaintenanceRequestList {
	if l == nil {
		return nil
	}

	return l.DeepCopyObject().(*MaintenanceRequestList)
}

// DeepCopyInto copies the receiver into out.
func (l *MaintenanceRequestList) DeepCopyInto(out *MaintenanceRequestList) {
	out.TypeMeta = l.TypeMeta
	l.ListMeta.DeepCopyInto(&out.ListMeta)

	if l.Items != nil {
		out.Items = make([]MaintenanceRequest, len(l.Items))
		for i := range l.Items {
			l.Items[i].DeepCopyInto(&out.Items[i])
		}
	} else {
		out.Items = nil
	}
}
