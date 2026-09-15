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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func mrWithTimestamp(at time.Time) *MaintenanceRequest {
	return &MaintenanceRequest{
		APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
		Kind:       "MaintenanceRequest",
		Name:       "mr-roundtrip",
		Labels:     map[string]string{"node": "node-1"},
		Spec: &protos.MaintenanceRequestSpec{
			HealthEvent: &protos.HealthEvent{
				Id:                "he-mr-1",
				NodeName:          "node-1",
				Agent:             "maintenance-controller",
				CheckName:         "external-maintenance",
				IsFatal:           true,
				RecommendedAction: protos.RecommendedAction_NONE,
				Message:           "Planned maintenance window",
			},
			StartTime: timestamppb.New(at),
		},
		Status: &protos.MaintenanceRequestStatus{
			Conditions: []*protos.Condition{
				{
					Type:               "HealthEventEmitted",
					Status:             "True",
					Reason:             "Emitted",
					Message:            "Submitted health event to platform-connector.",
					LastTransitionTime: timestamppb.New(at),
				},
			},
		},
	}
}

func TestMR_MarshalJSON_EmitsRFC3339Timestamp(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	in := mrWithTimestamp(at)

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)

	assert.Contains(t, got, `"lastTransitionTime":"2026-08-15T10:00:00Z"`,
		"condition timestamp must be emitted as RFC3339 string")
	assert.Contains(t, got, `"startTime":"2026-08-15T10:00:00Z"`,
		"spec.startTime must be emitted as RFC3339 string")
	assert.NotContains(t, got, `"seconds"`,
		"timestamp must NOT be emitted as the reflection-default {seconds, nanos} object")
	assert.NotContains(t, got, `"nanos"`,
		"timestamp must NOT be emitted as the reflection-default {seconds, nanos} object")

	assert.Contains(t, got, `"apiVersion":"nvsentinel.dgxc.nvidia.com/v1"`)
	assert.Contains(t, got, `"kind":"MaintenanceRequest"`)
	assert.Contains(t, got, `"metadata":{`)
	assert.Contains(t, got, `"spec":{`)
	assert.Contains(t, got, `"status":{`)
}

func TestMR_JSONRoundTrip_PreservesAllFields(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 15, 10, 0, 0, 123_000_000, time.UTC)
	in := mrWithTimestamp(at)

	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out MaintenanceRequest

	require.NoError(t, json.Unmarshal(b, &out))

	assert.Equal(t, in.APIVersion, out.APIVersion)
	assert.Equal(t, in.Kind, out.Kind)
	assert.Equal(t, in.Name, out.Name)
	assert.Equal(t, in.Labels, out.Labels)

	require.NotNil(t, out.Spec)
	require.NotNil(t, out.Spec.HealthEvent)
	assert.Equal(t, in.Spec.HealthEvent.Id, out.Spec.HealthEvent.Id)
	assert.Equal(t, in.Spec.HealthEvent.NodeName, out.Spec.HealthEvent.NodeName)
	assert.Equal(t, in.Spec.HealthEvent.Agent, out.Spec.HealthEvent.Agent)
	assert.Equal(t, in.Spec.HealthEvent.CheckName, out.Spec.HealthEvent.CheckName)
	assert.Equal(t, in.Spec.HealthEvent.Message, out.Spec.HealthEvent.Message)

	require.NotNil(t, out.Spec.StartTime)
	assert.True(t,
		in.Spec.StartTime.AsTime().Equal(out.Spec.StartTime.AsTime()),
		"StartTime must round-trip: want=%v got=%v",
		in.Spec.StartTime.AsTime(), out.Spec.StartTime.AsTime())

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
		"LastTransitionTime must round-trip")
}

func TestMR_MarshalJSON_NilSpecAndStatusOmitted(t *testing.T) {
	t.Parallel()

	in := &MaintenanceRequest{
		APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
		Kind:       "MaintenanceRequest",
		Name:       "no-spec-no-status",
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)
	assert.NotContains(t, got, `"spec"`, "nil Spec must be omitted, not emitted as null")
	assert.NotContains(t, got, `"status"`, "nil Status must be omitted, not emitted as null")
}

func TestMR_UnmarshalJSON_NullSpecAndStatusBecomeNil(t *testing.T) {
	t.Parallel()

	input := `{
        "apiVersion": "nvsentinel.dgxc.nvidia.com/v1",
        "kind": "MaintenanceRequest",
        "metadata": {"name": "null-spec"},
        "spec": null,
        "status": null
    }`

	var out MaintenanceRequest

	require.NoError(t, json.Unmarshal([]byte(input), &out))
	assert.Nil(t, out.Spec, "null spec must unmarshal to nil pointer")
	assert.Nil(t, out.Status, "null status must unmarshal to nil pointer")
}

func TestMR_UnmarshalJSON_DiscardsUnknownProtoFields(t *testing.T) {
	t.Parallel()

	input := `{
        "apiVersion": "nvsentinel.dgxc.nvidia.com/v1",
        "kind": "MaintenanceRequest",
        "metadata": {"name": "forward-compat"},
        "spec": {
            "healthEvent": {"id": "he-x", "nodeName": "n-x"},
            "futureFieldFromNewerServer": "ignored"
        }
    }`

	var out MaintenanceRequest

	require.NoError(t, json.Unmarshal([]byte(input), &out),
		"unknown fields must be tolerated for k8s-style forward compatibility")
	require.NotNil(t, out.Spec)
	require.NotNil(t, out.Spec.HealthEvent)
	assert.Equal(t, "he-x", out.Spec.HealthEvent.Id)
	assert.Equal(t, "n-x", out.Spec.HealthEvent.NodeName)
}

func TestMRList_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	in := &MaintenanceRequestList{
		APIVersion:      "nvsentinel.dgxc.nvidia.com/v1",
		Kind:            "MaintenanceRequestList",
		ResourceVersion: "42",
		Items: []MaintenanceRequest{
			*mrWithTimestamp(at),
			*mrWithTimestamp(at.Add(time.Minute)),
		},
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	got := string(b)
	assert.Contains(t, got, `"lastTransitionTime":"2026-08-15T10:00:00Z"`)
	assert.Contains(t, got, `"lastTransitionTime":"2026-08-15T10:01:00Z"`)
	assert.NotContains(t, got, `"seconds"`)
	assert.Contains(t, got, `"resourceVersion":"42"`)

	var out MaintenanceRequestList
	require.NoError(t, json.Unmarshal(b, &out))
	require.Len(t, out.Items, 2)
	assert.Equal(t, in.Items[0].Name, out.Items[0].Name)
	assert.Equal(t, in.Items[1].Name, out.Items[1].Name)
	assert.Equal(t,
		in.Items[0].Status.Conditions[0].LastTransitionTime.AsTime(),
		out.Items[0].Status.Conditions[0].LastTransitionTime.AsTime())
}
