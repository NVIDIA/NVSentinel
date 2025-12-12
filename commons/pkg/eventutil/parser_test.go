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

package eventutil

import (
	"testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHealthEventFromEvent(t *testing.T) {
	quarantined := model.Quarantined

	tests := []struct {
		name        string
		event       datastore.Event
		expectError bool
		checkResult func(*testing.T, model.HealthEventWithStatus)
	}{
		{
			name: "parse change stream event with fullDocument",
			event: datastore.Event{
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"healtheventstatus": map[string]interface{}{
						"nodequarantined": "Quarantined",
					},
					"healthevent": map[string]interface{}{
						"nodename":      "test-node",
						"checkname":     "GpuXidError",
						"componentclass": "GPU",
					},
				},
			},
			expectError: false,
			checkResult: func(t *testing.T, result model.HealthEventWithStatus) {
				assert.Equal(t, "test-node", result.HealthEvent.NodeName)
				assert.Equal(t, "GpuXidError", result.HealthEvent.CheckName)
				assert.Equal(t, quarantined, *result.HealthEventStatus.NodeQuarantined)
			},
		},
		{
			name: "parse direct document event",
			event: datastore.Event{
				"healtheventstatus": map[string]interface{}{
					"nodequarantined": "Quarantined",
				},
				"healthevent": map[string]interface{}{
					"nodename":       "test-node-2",
					"checkname":      "NvLinkDown",
					"componentclass": "NVLink",
				},
			},
			expectError: false,
			checkResult: func(t *testing.T, result model.HealthEventWithStatus) {
				assert.Equal(t, "test-node-2", result.HealthEvent.NodeName)
				assert.Equal(t, "NvLinkDown", result.HealthEvent.CheckName)
				assert.Equal(t, quarantined, *result.HealthEventStatus.NodeQuarantined)
			},
		},
		{
			name: "error on nil health event",
			event: datastore.Event{
				"healtheventstatus": map[string]interface{}{
					"nodequarantined": "Quarantined",
				},
			},
			expectError: true,
		},
		{
			name: "parse PostgreSQL format with document field",
			event: datastore.Event{
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"document": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "Quarantined",
						},
						"healthevent": map[string]interface{}{
							"nodename":       "postgres-node",
							"checkname":      "GpuXidError",
							"componentclass": "GPU",
						},
					},
				},
			},
			expectError: false,
			checkResult: func(t *testing.T, result model.HealthEventWithStatus) {
				assert.Equal(t, "postgres-node", result.HealthEvent.NodeName)
				assert.Equal(t, "GpuXidError", result.HealthEvent.CheckName)
				assert.Equal(t, quarantined, *result.HealthEventStatus.NodeQuarantined)
			},
		},
		{
			name: "handle nil NodeQuarantined with default value",
			event: datastore.Event{
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"document": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": nil,
						},
						"healthevent": map[string]interface{}{
							"nodename":       "new-event-node",
							"checkname":      "GpuXidError",
							"componentclass": "GPU",
						},
					},
				},
			},
			expectError: false,
			checkResult: func(t *testing.T, result model.HealthEventWithStatus) {
				assert.Equal(t, "new-event-node", result.HealthEvent.NodeName)
				assert.Equal(t, "GpuXidError", result.HealthEvent.CheckName)
				assert.NotNil(t, result.HealthEventStatus.NodeQuarantined)
				assert.Equal(t, model.StatusNotStarted, *result.HealthEventStatus.NodeQuarantined)
			},
		},
		{
			name: "handle missing NodeQuarantined field with default value",
			event: datastore.Event{
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"document": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{},
						"healthevent": map[string]interface{}{
							"nodename":       "missing-status-node",
							"checkname":      "GpuXidError",
							"componentclass": "GPU",
						},
					},
				},
			},
			expectError: false,
			checkResult: func(t *testing.T, result model.HealthEventWithStatus) {
				assert.Equal(t, "missing-status-node", result.HealthEvent.NodeName)
				assert.NotNil(t, result.HealthEventStatus.NodeQuarantined)
				assert.Equal(t, model.StatusNotStarted, *result.HealthEventStatus.NodeQuarantined)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseHealthEventFromEvent(tt.event)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if tt.checkResult != nil {
					tt.checkResult(t, result)
				}
			}
		})
	}
}
