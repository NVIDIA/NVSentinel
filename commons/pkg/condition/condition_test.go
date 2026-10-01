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

package condition

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// fixedTime keeps equality comparisons stable across tests.
var fixedTime = time.Date(2026, 5, 11, 20, 14, 9, 0, time.UTC)

func TestToMetav1_PopulatedFields(t *testing.T) {
	t.Parallel()

	in := &protos.Condition{
		Type:               "NVSentinelOwnershipReleased",
		Status:             "True",
		ObservedGeneration: 7,
		LastTransitionTime: timestamppb.New(fixedTime),
		Reason:             "ReleaseTaintApplied",
		Message:            "Applied release taint to node-01",
	}

	got := ToMetav1(in)

	assert.Equal(t, "NVSentinelOwnershipReleased", got.Type)
	assert.Equal(t, metav1.ConditionTrue, got.Status)
	assert.Equal(t, int64(7), got.ObservedGeneration)
	assert.Equal(t, fixedTime.UTC(), got.LastTransitionTime.Time.UTC())
	assert.Equal(t, "ReleaseTaintApplied", got.Reason)
	assert.Equal(t, "Applied release taint to node-01", got.Message)
}

func TestToMetav1_NilInputReturnsZeroValue(t *testing.T) {
	t.Parallel()

	got := ToMetav1(nil)

	assert.Equal(t, metav1.Condition{}, got)
}

func TestToMetav1_NilTimestampPreservesZeroTime(t *testing.T) {
	t.Parallel()

	in := &protos.Condition{
		Type:   "ExternalRemediationComplete",
		Status: "Unknown",
		// LastTransitionTime intentionally nil.
	}

	got := ToMetav1(in)

	assert.True(t, got.LastTransitionTime.IsZero())
}

func TestFromMetav1_PopulatedFields(t *testing.T) {
	t.Parallel()

	in := metav1.Condition{
		Type:               "ExternalRemediationComplete",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 3,
		LastTransitionTime: metav1.NewTime(fixedTime),
		Reason:             "ExternalRemediationFailed",
		Message:            "External system reported failure",
	}

	got := FromMetav1(in)

	require.NotNil(t, got)
	assert.Equal(t, "ExternalRemediationComplete", got.Type)
	assert.Equal(t, "False", got.Status)
	assert.Equal(t, int64(3), got.ObservedGeneration)
	require.NotNil(t, got.LastTransitionTime)
	assert.Equal(t, fixedTime.UTC(), got.LastTransitionTime.AsTime())
	assert.Equal(t, "ExternalRemediationFailed", got.Reason)
	assert.Equal(t, "External system reported failure", got.Message)
}

func TestFromMetav1_ZeroTimeProducesNilTimestamp(t *testing.T) {
	t.Parallel()

	in := metav1.Condition{
		Type:   "NVSentinelOwnershipReleased",
		Status: metav1.ConditionUnknown,
		// LastTransitionTime intentionally zero.
	}

	got := FromMetav1(in)

	require.NotNil(t, got)
	assert.Nil(t, got.LastTransitionTime, "zero metav1.Time should round-trip to nil proto Timestamp")
}

// TestRoundTrip_ProtoToMetav1ToProto guards the property the controller
// boundary depends on: proto → metav1 → proto preserves all fields.
func TestRoundTrip_ProtoToMetav1ToProto(t *testing.T) {
	t.Parallel()

	original := &protos.Condition{
		Type:               "NVSentinelOwnershipReleased",
		Status:             "True",
		ObservedGeneration: 42,
		LastTransitionTime: timestamppb.New(fixedTime),
		Reason:             "ReleaseTaintApplied",
		Message:            "round-trip test",
	}

	roundTripped := FromMetav1(ToMetav1(original))

	require.NotNil(t, roundTripped)
	assert.Equal(t, original.Type, roundTripped.Type)
	assert.Equal(t, original.Status, roundTripped.Status)
	assert.Equal(t, original.ObservedGeneration, roundTripped.ObservedGeneration)
	assert.Equal(t, original.Reason, roundTripped.Reason)
	assert.Equal(t, original.Message, roundTripped.Message)
	require.NotNil(t, roundTripped.LastTransitionTime)
	assert.Equal(t, original.LastTransitionTime.AsTime(), roundTripped.LastTransitionTime.AsTime())
}

func TestRoundTrip_Metav1ToProtoToMetav1(t *testing.T) {
	t.Parallel()

	original := metav1.Condition{
		Type:               "ExternalRemediationComplete",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(fixedTime),
		Reason:             "Succeeded",
		Message:            "external system reported success",
	}

	roundTripped := ToMetav1(FromMetav1(original))

	assert.Equal(t, original.Type, roundTripped.Type)
	assert.Equal(t, original.Status, roundTripped.Status)
	assert.Equal(t, original.ObservedGeneration, roundTripped.ObservedGeneration)
	assert.Equal(t, original.Reason, roundTripped.Reason)
	assert.Equal(t, original.Message, roundTripped.Message)
	assert.Equal(t, original.LastTransitionTime.UTC(), roundTripped.LastTransitionTime.UTC())
}

func TestToMetav1Slice_HandlesNilEntries(t *testing.T) {
	t.Parallel()

	in := []*protos.Condition{
		{Type: "NVSentinelOwnershipReleased", Status: "True"},
		nil, // exercises the nil-skip branch
		{Type: "ExternalRemediationComplete", Status: "Unknown"},
	}

	got := ToMetav1Slice(in)

	require.Len(t, got, 2)
	assert.Equal(t, "NVSentinelOwnershipReleased", got[0].Type)
	assert.Equal(t, "ExternalRemediationComplete", got[1].Type)
}

func TestToMetav1Slice_NilInputReturnsNil(t *testing.T) {
	t.Parallel()

	got := ToMetav1Slice(nil)

	assert.Nil(t, got)
}

func TestFromMetav1Slice_PreservesOrder(t *testing.T) {
	t.Parallel()

	in := []metav1.Condition{
		{Type: "First", Status: metav1.ConditionTrue},
		{Type: "Second", Status: metav1.ConditionFalse},
		{Type: "Third", Status: metav1.ConditionUnknown},
	}

	got := FromMetav1Slice(in)

	require.Len(t, got, 3)
	assert.Equal(t, "First", got[0].Type)
	assert.Equal(t, "True", got[0].Status)
	assert.Equal(t, "Second", got[1].Type)
	assert.Equal(t, "False", got[1].Status)
	assert.Equal(t, "Third", got[2].Type)
	assert.Equal(t, "Unknown", got[2].Status)
}

func TestFromMetav1Slice_NilInputReturnsNil(t *testing.T) {
	t.Parallel()

	got := FromMetav1Slice(nil)

	assert.Nil(t, got)
}

// TestSliceRoundTrip exercises the controller's read-modify-write path.
func TestSliceRoundTrip(t *testing.T) {
	t.Parallel()

	original := []*protos.Condition{
		{
			Type:               "NVSentinelOwnershipReleased",
			Status:             "True",
			ObservedGeneration: 1,
			LastTransitionTime: timestamppb.New(fixedTime),
			Reason:             "ReleaseTaintApplied",
		},
		{
			Type:               "ExternalRemediationComplete",
			Status:             "Unknown",
			ObservedGeneration: 1,
			LastTransitionTime: timestamppb.New(fixedTime),
			Reason:             "AwaitingExternalSystem",
		},
	}

	roundTripped := FromMetav1Slice(ToMetav1Slice(original))

	require.Len(t, roundTripped, 2)

	for i := range original {
		assert.Equal(t, original[i].Type, roundTripped[i].Type)
		assert.Equal(t, original[i].Status, roundTripped[i].Status)
		assert.Equal(t, original[i].ObservedGeneration, roundTripped[i].ObservedGeneration)
		assert.Equal(t, original[i].Reason, roundTripped[i].Reason)
		assert.Equal(t, original[i].LastTransitionTime.AsTime(), roundTripped[i].LastTransitionTime.AsTime())
	}
}
