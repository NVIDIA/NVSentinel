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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// extrrWithTimestamp returns an ExtRR whose Condition carries a non-zero
// LastTransitionTime, the case where default encoding/json marshalling
// diverges from the CRD-required RFC3339 string form.
func extrrWithTimestamp(at time.Time) *ExternalRemediationRequest {
	return &ExternalRemediationRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "extrr-roundtrip",
			Labels: map[string]string{"node": "node-1"},
		},
		Spec: &protos.ExternalRemediationRequestSpec{
			HealthEvent: &protos.HealthEvent{
				Id:                "he-1",
				NodeName:          "node-1",
				IsFatal:           true,
				RecommendedAction: protos.RecommendedAction_CUSTOM,
				Message:           "XID-79 on GPU 0",
			},
		},
		Status: &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{
					Type:               "NVSentinelOwnershipReleased",
					Status:             "True",
					Reason:             "ReleaseTaintApplied",
					Message:            "release taint and managed=false applied",
					LastTransitionTime: timestamppb.New(at),
				},
			},
		},
	}
}

func TestExtRR_MarshalJSON_EmitsRFC3339Timestamp(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 5, 15, 30, 45, 0, time.UTC)
	in := extrrWithTimestamp(at)

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)

	// CRD schema requires a string, not an object.
	assert.Contains(t, got, `"lastTransitionTime":"2026-06-05T15:30:45Z"`,
		"timestamp must be emitted as RFC3339 string per protojson canonical form")
	assert.NotContains(t, got, `"seconds"`,
		"timestamp must NOT be emitted as the reflection-default {seconds, nanos} object")
	assert.NotContains(t, got, `"nanos"`,
		"timestamp must NOT be emitted as the reflection-default {seconds, nanos} object")

	// Sanity: top-level shape still looks like a k8s object.
	assert.Contains(t, got, `"apiVersion":"nvsentinel.dgxc.nvidia.com/v1"`)
	assert.Contains(t, got, `"kind":"ExternalRemediationRequest"`)
	assert.Contains(t, got, `"metadata":{`)
	assert.Contains(t, got, `"spec":{`)
	assert.Contains(t, got, `"status":{`)
}

func TestExtRR_JSONRoundTrip_PreservesAllFields(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 5, 15, 30, 45, 123_000_000, time.UTC) // RFC3339 has nanosecond precision
	in := extrrWithTimestamp(at)

	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out ExternalRemediationRequest

	require.NoError(t, json.Unmarshal(b, &out))

	assert.Equal(t, in.APIVersion, out.APIVersion)
	assert.Equal(t, in.Kind, out.Kind)
	assert.Equal(t, in.Name, out.Name)
	assert.Equal(t, in.Namespace, out.Namespace)
	assert.Equal(t, in.Labels, out.Labels)

	require.NotNil(t, out.Spec)
	require.NotNil(t, out.Spec.HealthEvent)
	assert.Equal(t, in.Spec.HealthEvent.Id, out.Spec.HealthEvent.Id)
	assert.Equal(t, in.Spec.HealthEvent.NodeName, out.Spec.HealthEvent.NodeName)
	assert.Equal(t, in.Spec.HealthEvent.Message, out.Spec.HealthEvent.Message)
	assert.Equal(t, in.Spec.HealthEvent.RecommendedAction, out.Spec.HealthEvent.RecommendedAction)

	require.NotNil(t, out.Status)
	require.Len(t, out.Status.Conditions, 1)

	gotCond := out.Status.Conditions[0]
	wantCond := in.Status.Conditions[0]
	assert.Equal(t, wantCond.Type, gotCond.Type)
	assert.Equal(t, wantCond.Status, gotCond.Status)
	assert.Equal(t, wantCond.Reason, gotCond.Reason)
	assert.Equal(t, wantCond.Message, gotCond.Message)

	require.NotNil(t, gotCond.LastTransitionTime)
	assert.True(t,
		wantCond.LastTransitionTime.AsTime().Equal(gotCond.LastTransitionTime.AsTime()),
		"LastTransitionTime must round-trip: want=%v got=%v",
		wantCond.LastTransitionTime.AsTime(), gotCond.LastTransitionTime.AsTime())
}

func TestExtRR_MarshalJSON_NilSpecAndStatusOmitted(t *testing.T) {
	t.Parallel()

	in := &ExternalRemediationRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequest",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "no-spec-no-status"},
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)
	assert.NotContains(t, got, `"spec"`, "nil Spec must be omitted, not emitted as null")
	assert.NotContains(t, got, `"status"`, "nil Status must be omitted, not emitted as null")
}

func TestExtRR_UnmarshalJSON_NullSpecAndStatusBecomeNil(t *testing.T) {
	t.Parallel()

	// Server may return spec or status as JSON null.
	input := `{
        "apiVersion": "nvsentinel.dgxc.nvidia.com/v1",
        "kind": "ExternalRemediationRequest",
        "metadata": {"name": "null-spec"},
        "spec": null,
        "status": null
    }`

	var out ExternalRemediationRequest

	require.NoError(t, json.Unmarshal([]byte(input), &out))
	assert.Nil(t, out.Spec, "null spec must unmarshal to nil pointer")
	assert.Nil(t, out.Status, "null status must unmarshal to nil pointer")
}

func TestExtRR_UnmarshalJSON_DiscardsUnknownProtoFields(t *testing.T) {
	t.Parallel()

	// Simulate a server adding a new field to the proto we don't yet know about.
	input := `{
        "apiVersion": "nvsentinel.dgxc.nvidia.com/v1",
        "kind": "ExternalRemediationRequest",
        "metadata": {"name": "forward-compat"},
        "spec": {
            "healthEvent": {"id": "he-x", "nodeName": "n-x"},
            "futureFieldFromNewerServer": "ignored"
        }
    }`

	var out ExternalRemediationRequest

	require.NoError(t, json.Unmarshal([]byte(input), &out),
		"unknown fields must be tolerated for k8s-style forward compatibility")
	require.NotNil(t, out.Spec)
	require.NotNil(t, out.Spec.HealthEvent)
	assert.Equal(t, "he-x", out.Spec.HealthEvent.Id)
	assert.Equal(t, "n-x", out.Spec.HealthEvent.NodeName)
}

func TestExtRR_UnmarshalJSON_RejectsLegacyObjectTimestamp(t *testing.T) {
	t.Parallel()

	// The old (broken) wire form — protojson should refuse to unmarshal this.
	input := `{
        "apiVersion": "nvsentinel.dgxc.nvidia.com/v1",
        "kind": "ExternalRemediationRequest",
        "metadata": {"name": "legacy-shape"},
        "status": {
            "conditions": [{
                "type": "X",
                "status": "Unknown",
                "lastTransitionTime": {"seconds": 1, "nanos": 0}
            }]
        }
    }`

	var out ExternalRemediationRequest

	err := json.Unmarshal([]byte(input), &out)
	require.Error(t, err, "object-form timestamp must NOT round-trip; CRDs only accept RFC3339 strings")
	// protojson reports a generic "unexpected token {" rather than naming the field;
	// what matters is that the error originates in our status-unmarshal path.
	assert.Contains(t, err.Error(), "ExternalRemediationRequest.status",
		"error should originate in the status-unmarshal path; got: %v", err)
}

func TestERRList_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 5, 15, 30, 45, 0, time.UTC)
	in := &ExternalRemediationRequestList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequestList",
		},
		ListMeta: metav1.ListMeta{ResourceVersion: "42"},
		Items: []ExternalRemediationRequest{
			*extrrWithTimestamp(at),
			*extrrWithTimestamp(at.Add(time.Minute)),
		},
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)
	// Items' timestamps must use RFC3339 strings, same as singletons.
	assert.Contains(t, got, `"lastTransitionTime":"2026-06-05T15:30:45Z"`)
	assert.Contains(t, got, `"lastTransitionTime":"2026-06-05T15:31:45Z"`)
	assert.NotContains(t, got, `"seconds"`)
	assert.Contains(t, got, `"resourceVersion":"42"`)

	var out ExternalRemediationRequestList
	require.NoError(t, json.Unmarshal(b, &out))
	require.Len(t, out.Items, 2)
	assert.Equal(t, in.Items[0].Name, out.Items[0].Name)
	assert.Equal(t, in.Items[1].Name, out.Items[1].Name)
	assert.Equal(t,
		in.Items[0].Status.Conditions[0].LastTransitionTime.AsTime(),
		out.Items[0].Status.Conditions[0].LastTransitionTime.AsTime())
}
