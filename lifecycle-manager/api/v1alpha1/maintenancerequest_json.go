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
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// mrProtojsonUnmarshalOpts is the lenient unmarshal config used for k8s-side
// reads. DiscardUnknown=true matches apimachinery's behavior of tolerating
// server-side schema additions.
var mrProtojsonUnmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

// mrJSONEnvelope mirrors the wire shape of the wrapper but pre-encodes Spec
// and Status as opaque JSON blobs so they can be produced by protojson
// (marshal) or fed to protojson (unmarshal).
type mrJSONEnvelope struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              json.RawMessage `json:"spec,omitempty"`
	Status            json.RawMessage `json:"status,omitempty"`
}

// MarshalJSON serialises a MaintenanceRequest with TypeMeta and ObjectMeta
// via encoding/json and Spec / Status via protojson. This ensures proto
// well-known types (Timestamp, etc.) emit their canonical JSON forms.
func (m *MaintenanceRequest) MarshalJSON() ([]byte, error) {
	out := mrJSONEnvelope{
		TypeMeta:   m.TypeMeta,
		ObjectMeta: m.ObjectMeta,
	}

	if m.Spec != nil {
		b, err := protojson.Marshal(m.Spec)
		if err != nil {
			return nil, fmt.Errorf("marshal MaintenanceRequest.spec via protojson: %w", err)
		}

		out.Spec = b
	}

	if m.Status != nil {
		b, err := protojson.Marshal(m.Status)
		if err != nil {
			return nil, fmt.Errorf("marshal MaintenanceRequest.status via protojson: %w", err)
		}

		out.Status = b
	}

	return json.Marshal(&out)
}

// UnmarshalJSON deserialises a MaintenanceRequest with the inverse of
// MarshalJSON: TypeMeta and ObjectMeta via encoding/json, Spec / Status
// via protojson with DiscardUnknown enabled.
func (m *MaintenanceRequest) UnmarshalJSON(data []byte) error {
	var in mrJSONEnvelope
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("unmarshal MaintenanceRequest envelope: %w", err)
	}

	m.TypeMeta = in.TypeMeta
	m.ObjectMeta = in.ObjectMeta

	if mrIsJSONPresent(in.Spec) {
		spec := &protos.MaintenanceRequestSpec{}
		if err := mrProtojsonUnmarshalOpts.Unmarshal(in.Spec, spec); err != nil {
			return fmt.Errorf("unmarshal MaintenanceRequest.spec via protojson: %w", err)
		}

		m.Spec = spec
	} else {
		m.Spec = nil
	}

	if mrIsJSONPresent(in.Status) {
		status := &protos.MaintenanceRequestStatus{}
		if err := mrProtojsonUnmarshalOpts.Unmarshal(in.Status, status); err != nil {
			return fmt.Errorf("unmarshal MaintenanceRequest.status via protojson: %w", err)
		}

		m.Status = status
	} else {
		m.Status = nil
	}

	return nil
}

// mrListJSONEnvelope mirrors the wire shape of MaintenanceRequestList.
type mrListJSONEnvelope struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceRequest `json:"items"`
}

// MarshalJSON delegates each item to MaintenanceRequest.MarshalJSON.
func (l *MaintenanceRequestList) MarshalJSON() ([]byte, error) {
	return json.Marshal(&mrListJSONEnvelope{
		TypeMeta: l.TypeMeta,
		ListMeta: l.ListMeta,
		Items:    l.Items,
	})
}

// UnmarshalJSON deserialises the envelope and delegates each item to
// MaintenanceRequest.UnmarshalJSON.
func (l *MaintenanceRequestList) UnmarshalJSON(data []byte) error {
	var in mrListJSONEnvelope
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("unmarshal MaintenanceRequestList: %w", err)
	}

	l.TypeMeta = in.TypeMeta
	l.ListMeta = in.ListMeta
	l.Items = in.Items

	return nil
}

// mrIsJSONPresent reports whether the given RawMessage carries a real JSON
// payload, treating empty, missing, or explicit `null` as absence.
func mrIsJSONPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}

	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// Compile-time check that the proto types implement proto.Message.
var (
	_ proto.Message = (*protos.MaintenanceRequestSpec)(nil)
	_ proto.Message = (*protos.MaintenanceRequestStatus)(nil)
)
