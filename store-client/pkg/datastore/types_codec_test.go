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

package datastore

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestHealthEventStatus_UnmarshalBSON_PlainBool(t *testing.T) {
	status := unmarshalHealthEventStatusBSON(t, bson.D{{Key: "faultremediated", Value: true}})

	require.NotNil(t, status.FaultRemediated)
	assert.True(t, *status.FaultRemediated)
}

func TestHealthEventStatus_MarshalBSON_WrappedBool(t *testing.T) {
	faultRemediated := true
	status := HealthEventStatus{FaultRemediated: &faultRemediated}

	data, err := bson.Marshal(status)
	require.NoError(t, err)

	var doc bson.M
	require.NoError(t, bson.Unmarshal(data, &doc))

	value, ok := doc["faultremediated"].(bson.D)
	require.True(t, ok, "faultremediated should be an embedded document, got %T", doc["faultremediated"])
	assert.Equal(t, bson.D{{Key: "value", Value: true}}, value)
}

func TestHealthEventStatus_MarshalBSON_NilBool(t *testing.T) {
	data, err := bson.Marshal(HealthEventStatus{})
	require.NoError(t, err)

	var doc bson.M
	require.NoError(t, bson.Unmarshal(data, &doc))

	assert.Contains(t, doc, "faultremediated")
	assert.Nil(t, doc["faultremediated"])
}

func TestHealthEventStatus_UnmarshalBSON_WrappedBool(t *testing.T) {
	status := unmarshalHealthEventStatusBSON(t, bson.D{
		{Key: "faultremediated", Value: bson.D{{Key: "value", Value: false}}},
	})

	require.NotNil(t, status.FaultRemediated)
	assert.False(t, *status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalBSON_NullBool(t *testing.T) {
	status := unmarshalHealthEventStatusBSON(t, bson.D{{Key: "faultremediated", Value: nil}})

	assert.Nil(t, status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalBSON_MissingBool(t *testing.T) {
	status := unmarshalHealthEventStatusBSON(t, bson.D{})

	assert.Nil(t, status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalBSON_PreservesOtherFields(t *testing.T) {
	status := unmarshalHealthEventStatusBSON(t, bson.D{
		{Key: "nodequarantined", Value: "Quarantined"},
		{Key: "userpodsevictionstatus", Value: bson.D{
			{Key: "status", Value: "Succeeded"},
			{Key: "message", Value: "drain complete"},
		}},
		{Key: "faultremediated", Value: bson.D{{Key: "value", Value: true}}},
	})

	require.NotNil(t, status.NodeQuarantined)
	assert.Equal(t, Quarantined, *status.NodeQuarantined)
	assert.Equal(t, StatusSucceeded, status.UserPodsEvictionStatus.Status)
	assert.Equal(t, "drain complete", status.UserPodsEvictionStatus.Message)
	require.NotNil(t, status.FaultRemediated)
	assert.True(t, *status.FaultRemediated)
}

func TestHealthEventWithStatus_UnmarshalBSON_WrappedBool(t *testing.T) {
	data, err := bson.Marshal(bson.D{
		{Key: "healtheventstatus", Value: bson.D{
			{Key: "faultremediated", Value: bson.D{{Key: "value", Value: false}}},
		}},
	})
	require.NoError(t, err)

	var event HealthEventWithStatus
	require.NoError(t, bson.Unmarshal(data, &event))
	require.NotNil(t, event.HealthEventStatus.FaultRemediated)
	assert.False(t, *event.HealthEventStatus.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalJSON_PlainBool(t *testing.T) {
	status := unmarshalHealthEventStatusJSON(t, `{"faultremediated": true}`)

	require.NotNil(t, status.FaultRemediated)
	assert.True(t, *status.FaultRemediated)
}

func TestHealthEventStatus_MarshalJSON_WrappedBool(t *testing.T) {
	faultRemediated := true
	status := HealthEventStatus{FaultRemediated: &faultRemediated}

	data, err := json.Marshal(status)
	require.NoError(t, err)

	assert.JSONEq(t, `{"nodequarantined": null, "userpodsevictionstatus": {"status": ""}, "faultremediated": {"value": true}}`, string(data))
}

func TestHealthEventStatus_MarshalJSON_NilBool(t *testing.T) {
	data, err := json.Marshal(HealthEventStatus{})
	require.NoError(t, err)

	assert.JSONEq(t, `{"nodequarantined": null, "userpodsevictionstatus": {"status": ""}, "faultremediated": null}`, string(data))
}

func TestHealthEventStatus_UnmarshalJSON_WrappedBool(t *testing.T) {
	status := unmarshalHealthEventStatusJSON(t, `{"faultremediated": {"value": false}}`)

	require.NotNil(t, status.FaultRemediated)
	assert.False(t, *status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalJSON_NullBool(t *testing.T) {
	status := unmarshalHealthEventStatusJSON(t, `{"faultremediated": null}`)

	assert.Nil(t, status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalJSON_MissingBool(t *testing.T) {
	status := unmarshalHealthEventStatusJSON(t, `{}`)

	assert.Nil(t, status.FaultRemediated)
}

func TestHealthEventStatus_UnmarshalJSON_PreservesOtherFields(t *testing.T) {
	status := unmarshalHealthEventStatusJSON(t, `{
		"nodequarantined": "Quarantined",
		"userpodsevictionstatus": {
			"status": "Succeeded",
			"message": "drain complete"
		},
		"faultremediated": {"value": true}
	}`)

	require.NotNil(t, status.NodeQuarantined)
	assert.Equal(t, Quarantined, *status.NodeQuarantined)
	assert.Equal(t, StatusSucceeded, status.UserPodsEvictionStatus.Status)
	assert.Equal(t, "drain complete", status.UserPodsEvictionStatus.Message)
	require.NotNil(t, status.FaultRemediated)
	assert.True(t, *status.FaultRemediated)
}

func TestHealthEventWithStatus_UnmarshalJSON_WrappedBool(t *testing.T) {
	var event HealthEventWithStatus

	require.NoError(t, json.Unmarshal([]byte(`{
		"healtheventstatus": {
			"faultremediated": {"value": false}
		}
	}`), &event))
	require.NotNil(t, event.HealthEventStatus.FaultRemediated)
	assert.False(t, *event.HealthEventStatus.FaultRemediated)
}

func unmarshalHealthEventStatusBSON(t *testing.T, doc bson.D) HealthEventStatus {
	t.Helper()

	data, err := bson.Marshal(doc)
	require.NoError(t, err)

	var status HealthEventStatus
	require.NoError(t, bson.Unmarshal(data, &status))

	return status
}

func unmarshalHealthEventStatusJSON(t *testing.T, doc string) HealthEventStatus {
	t.Helper()

	var status HealthEventStatus
	require.NoError(t, json.Unmarshal([]byte(doc), &status))

	return status
}
