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

package client

import (
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

func TestBuildDirectFieldUpdate(t *testing.T) {
	c := &MongoDBClient{}

	t.Run("Direct field update with string value", func(t *testing.T) {
		result := c.buildDirectFieldUpdate("healtheventstatus.nodequarantined", "Quarantined")

		expected := bson.M{
			"healtheventstatus.nodequarantined": "Quarantined",
		}

		assert.Equal(t, expected, result)
	})

	t.Run("Direct field update with complex value", func(t *testing.T) {
		status := datastore.OperationStatus{
			Status:  datastore.StatusSucceeded,
			Message: "test message",
		}

		result := c.buildDirectFieldUpdate("healtheventstatus.userpodsevictionstatus", status)

		expected := bson.M{
			"healtheventstatus.userpodsevictionstatus": status,
		}

		assert.Equal(t, expected, result)
	})
}

func TestBuildStructFieldUpdates(t *testing.T) {
	c := &MongoDBClient{}

	t.Run("HealthEventStatus with FaultRemediated only", func(t *testing.T) {
		faultRemediated := true
		now := time.Now().UTC()
		status := datastore.HealthEventStatus{
			FaultRemediated:          &faultRemediated,
			LastRemediationTimestamp: &now,
		}

		result := c.buildStructFieldUpdates("healtheventstatus", status)

		expected := bson.M{
			"healtheventstatus.faultremediated":          true,
			"healtheventstatus.lastremediationtimestamp": now,
		}

		assert.Equal(t, expected, result)
	})

	t.Run("HealthEventStatus with all fields", func(t *testing.T) {
		quarantined := datastore.Quarantined
		faultRemediated := true
		now := time.Now().UTC()
		status := datastore.HealthEventStatus{
			NodeQuarantined: &quarantined,
			UserPodsEvictionStatus: datastore.OperationStatus{
				Status:  datastore.StatusSucceeded,
				Message: "test message",
			},
			FaultRemediated:          &faultRemediated,
			LastRemediationTimestamp: &now,
		}

		result := c.buildStructFieldUpdates("healtheventstatus", status)

		expected := bson.M{
			"healtheventstatus.nodequarantined":                string(datastore.Quarantined),
			"healtheventstatus.userpodsevictionstatus.status":  string(datastore.StatusSucceeded),
			"healtheventstatus.userpodsevictionstatus.message": "test message",
			"healtheventstatus.faultremediated":                true,
			"healtheventstatus.lastremediationtimestamp":       now,
		}

		assert.Equal(t, expected, result)
	})

	t.Run("Non-HealthEventStatus type falls back to whole object", func(t *testing.T) {
		status := "simple string"

		result := c.buildStructFieldUpdates("status", status)

		expected := bson.M{
			"status": "simple string",
		}

		assert.Equal(t, expected, result)
	})
}

func TestIsNestedFieldPath(t *testing.T) {
	c := &MongoDBClient{}

	t.Run("Nested field path returns true", func(t *testing.T) {
		assert.True(t, c.isNestedFieldPath("healtheventstatus.nodequarantined"))
		assert.True(t, c.isNestedFieldPath("healtheventstatus.userpodsevictionstatus"))
		assert.True(t, c.isNestedFieldPath("a.b.c"))
	})

	t.Run("Base path returns false", func(t *testing.T) {
		assert.False(t, c.isNestedFieldPath("healtheventstatus"))
		assert.False(t, c.isNestedFieldPath("status"))
		assert.False(t, c.isNestedFieldPath("nodequarantined"))
	})
}
