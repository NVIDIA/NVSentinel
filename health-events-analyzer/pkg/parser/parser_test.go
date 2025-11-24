// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"reflect"
	"testing"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestParseSequenceString(t *testing.T) {
	remediated := true

	tests := []struct {
		name     string
		criteria map[string]any
		event    datamodels.HealthEventWithStatus
		want     map[string]interface{}
		wantErr  bool
	}{
		{
			name: "criteria with fault remediation check and .this reference",
			criteria: map[string]any{
				"healtheventstatus.faultremediated": true,
				"healthevent.nodename":              "this.healthevent.nodename",
				"healthevent.isfatal":               "this.healthevent.isfatal",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "gpu-node-1",
					IsFatal:  true,
				},
				HealthEventStatus: datamodels.HealthEventStatus{
					FaultRemediated: &remediated,
				},
			},
			want: map[string]interface{}{
				"healtheventstatus.faultremediated": true,
				"healthevent.nodename":              "gpu-node-1",
				"healthevent.isfatal":               true,
			},
			wantErr: false,
		},
		{
			name: "criteria with $ne operator",
			criteria: map[string]any{
				"healthevent.nodename": "this.healthevent.nodename",
				"healthevent.agent":    `{"$ne":"health-events-analyzer"}`,
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
					Agent:    "health-events-analyzer",
				},
			},
			want: map[string]interface{}{
				"healthevent.nodename": "test-node",
				"healthevent.agent":    map[string]any{"$ne": "health-events-analyzer"},
			},
			wantErr: false,
		},
		{
			name: "criteria with errorcode array access",
			criteria: map[string]any{
				"healthevent.errorcode.0": "this.healthevent.errorcode.0",
				"healthevent.nodename":    "this.healthevent.nodename",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "node2",
					ErrorCode: []string{"48"},
				},
			},
			want: map[string]interface{}{
				"healthevent.errorcode.0": "48",
				"healthevent.nodename":    "node2",
			},
			wantErr: false,
		},
		{
			name: "criteria with mixed types - booleans, strings, and references",
			criteria: map[string]any{
				"healthevent.isfatal":                             true,
				"healthevent.ishealthy":                           false,
				"healthevent.checkname":                           "this.healthevent.checkname",
				"healthevent.nodename":                            "this.healthevent.nodename",
				"healthevent.agent":                               "gpu-health-monitor",
				"healtheventstatus.userpodsevictionstatus.status": "this.healtheventstatus.userpodsevictionstatus.status",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "node2",
					CheckName: "GpuXidError",
					IsFatal:   true,
					IsHealthy: false,
					Agent:     "gpu-health-monitor",
				},
				HealthEventStatus: datamodels.HealthEventStatus{
					UserPodsEvictionStatus: datamodels.OperationStatus{
						Status:  datamodels.StatusInProgress,
						Message: "Evicting pods from node",
					},
				},
			},
			want: map[string]interface{}{
				"healthevent.isfatal":   true,
				"healthevent.ishealthy": false,
				"healthevent.checkname": "GpuXidError",
				"healthevent.nodename":  "node2",
				"healthevent.agent":     "gpu-health-monitor",
				"healtheventstatus.userpodsevictionstatus.status": datamodels.StatusInProgress,
			},
			wantErr: false,
		},
		{
			name: "criteria with complex $in operator",
			criteria: map[string]any{
				"healthevent.checkname": `{"$in":["GpuXidError","GpuMemWatch","GpuNvswitchFatalWatch"]}`,
				"healthevent.nodename":  "this.healthevent.nodename",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "check-node",
					CheckName: "XID_ERROR",
				},
			},
			want: map[string]interface{}{
				"healthevent.checkname": map[string]any{"$in": []any{"GpuXidError", "GpuMemWatch", "GpuNvswitchFatalWatch"}},
				"healthevent.nodename":  "check-node",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSequenceString(tt.criteria, tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSequenceString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseSequenceString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSequenceStage(t *testing.T) {
	tests := []struct {
		name    string
		stage   string
		event   datamodels.HealthEventWithStatus
		want    map[string]interface{}
		wantErr bool
	}{
		{
			name:  "simple $match stage with this reference",
			stage: `{"$match": {"healthevent.nodename": "this.healthevent.nodename", "healthevent.isfatal": true}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node-1",
					IsFatal:  true,
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.nodename": "test-node-1",
					"healthevent.isfatal":  true,
				},
			},
			wantErr: false,
		},
		{
			name:  "$match stage with nested this references",
			stage: `{"$match": {"healthevent.nodename": "this.healthevent.nodename", "healthevent.errorcode.0": "this.healthevent.errorcode.0"}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "gpu-node-2",
					ErrorCode: []string{"13", "31"},
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.nodename":    "gpu-node-2",
					"healthevent.errorcode.0": "13",
				},
			},
			wantErr: false,
		},
		{
			name:  "$count stage without this references",
			stage: `{"$count": "total"}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "node1",
				},
			},
			want: map[string]interface{}{
				"$count": "total",
			},
			wantErr: false,
		},
		{
			name:  "complex $match with expression and this references",
			stage: `{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", 1000]}, "healthevent.nodename": "this.healthevent.nodename"}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "expr-node",
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"$expr": map[string]interface{}{
						"$gte": []interface{}{
							"$healthevent.generatedtimestamp.seconds",
							float64(1000),
						},
					},
					"healthevent.nodename": "expr-node",
				},
			},
			wantErr: false,
		},
		{
			name:  "stage with entity array access",
			stage: `{"$match": {"healthevent.entitiesimpacted.0.entityvalue": "this.healthevent.entitiesimpacted.0.entityvalue"}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					EntitiesImpacted: []*protos.Entity{
						{EntityType: "GPU", EntityValue: "GPU-123"},
					},
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.entitiesimpacted.0.entityvalue": "GPU-123",
				},
			},
			wantErr: false,
		},
		{
			name:  "stage with full entitiesimpacted array (MongoDB filter context)",
			stage: `{"$match": {"$filter": {"input": "this.healthevent.entitiesimpacted", "cond": {"$eq": ["$$this.entitytype", "GPU"]}}}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					EntitiesImpacted: []*protos.Entity{
						{EntityType: "GPU", EntityValue: "0"},
						{EntityType: "CPU", EntityValue: "1"},
					},
				},
			},
			// After normalization, protobuf array should have lowercase field names
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"$filter": map[string]interface{}{
						"input": []interface{}{
							map[string]interface{}{"entitytype": "GPU", "entityvalue": "0"},
							map[string]interface{}{"entitytype": "CPU", "entityvalue": "1"},
						},
						"cond": map[string]interface{}{
							"$eq": []interface{}{"$$this.entitytype", "GPU"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "invalid JSON stage",
			stage:   `{invalid json}`,
			event:   datamodels.HealthEventWithStatus{},
			want:    nil,
			wantErr: true,
		},
		{
			name:  "stage with invalid this reference",
			stage: `{"$match": {"healthevent.nonexistent": "this.healthevent.nonexistentfield"}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test",
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name:  "nested arrays in stage",
			stage: `{"$match": {"filters": ["this.healthevent.nodename", "this.healthevent.checkname"]}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "array-node",
					CheckName: "TestCheck",
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"filters": []interface{}{"array-node", "TestCheck"},
				},
			},
			wantErr: false,
		},
		{
			name:  "deeply nested map with this references",
			stage: `{"$match": {"nested": {"level1": {"level2": "this.healthevent.nodename"}}}}`,
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "deep-node",
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"nested": map[string]interface{}{
						"level1": map[string]interface{}{
							"level2": "deep-node",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSequenceStage(tt.stage, tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSequenceStage() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseSequenceStage() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGetValueFromPath(t *testing.T) {
	remediated := true

	tests := []struct {
		name    string
		path    string
		event   datamodels.HealthEventWithStatus
		want    interface{}
		wantErr bool
	}{
		{
			name: "simple field access",
			path: "healthevent.nodename",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
				},
			},
			want:    "test-node",
			wantErr: false,
		},
		{
			name: "boolean field",
			path: "healthevent.isfatal",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					IsFatal: true,
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "array element access",
			path: "healthevent.errorcode.0",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					ErrorCode: []string{"13", "31", "48"},
				},
			},
			want:    "13",
			wantErr: false,
		},
		{
			name: "nested struct field",
			path: "healtheventstatus.faultremediated",
			event: datamodels.HealthEventWithStatus{
				HealthEventStatus: datamodels.HealthEventStatus{
					FaultRemediated: &remediated,
				},
			},
			want:    &remediated,
			wantErr: false,
		},
		{
			name: "entity array access",
			path: "healthevent.entitiesimpacted.0.entityvalue",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					EntitiesImpacted: []*protos.Entity{
						{EntityType: "GPU", EntityValue: "GPU-456"},
					},
				},
			},
			want:    "GPU-456",
			wantErr: false,
		},
		{
			name: "deeply nested field",
			path: "healtheventstatus.userpodsevictionstatus.status",
			event: datamodels.HealthEventWithStatus{
				HealthEventStatus: datamodels.HealthEventStatus{
					UserPodsEvictionStatus: datamodels.OperationStatus{
						Status:  datamodels.StatusSucceeded,
						Message: "Done",
					},
				},
			},
			want:    datamodels.StatusSucceeded,
			wantErr: false,
		},
		{
			name: "invalid path - too short",
			path: "healthevent",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test",
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid field name",
			path: "healthevent.nonexistent",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test",
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid array index",
			path: "healthevent.errorcode.99",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					ErrorCode: []string{"13"},
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "case insensitive field matching",
			path: "healthevent.NodeName",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "case-test",
				},
			},
			want:    "case-test",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getValueFromPath(tt.path, tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("getValueFromPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getValueFromPath() = %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestProcessValue(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		event   datamodels.HealthEventWithStatus
		want    interface{}
		wantErr bool
	}{
		{
			name:  "plain string without this reference",
			value: "static-value",
			event: datamodels.HealthEventWithStatus{},
			want:  "static-value",
		},
		{
			name:  "this reference string",
			value: "this.healthevent.nodename",
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "node123",
				},
			},
			want: "node123",
		},
		{
			name:  "number value",
			value: float64(42),
			event: datamodels.HealthEventWithStatus{},
			want:  float64(42),
		},
		{
			name:  "boolean value",
			value: true,
			event: datamodels.HealthEventWithStatus{},
			want:  true,
		},
		{
			name: "map with this reference",
			value: map[string]interface{}{
				"field": "this.healthevent.checkname",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					CheckName: "TestCheck",
				},
			},
			want: map[string]interface{}{
				"field": "TestCheck",
			},
		},
		{
			name: "array with this reference",
			value: []interface{}{
				"this.healthevent.nodename",
				"static",
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "arraynode",
				},
			},
			want: []interface{}{
				"arraynode",
				"static",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := processValue(tt.value, tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("processValue() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("processValue() = %v, want %v", got, tt.want)
			}
		})
	}
}
