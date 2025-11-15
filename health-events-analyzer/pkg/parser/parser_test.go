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
		name    string
		stage   []string
		event   datamodels.HealthEventWithStatus
		want    map[string]interface{}
		wantErr bool
	}{
		{
			name: "criteria with fault remediation check and .this reference",
			stage: []string{
				`{"$match" : {"healtheventstatus.faultremediated": true, "healthevent.nodename": "this.healthevent.nodename", "healthevent.isfatal": "this.healthevent.isfatal"}}`,
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
				"$match": map[string]interface{}{
					"healtheventstatus.faultremediated": true,
					"healthevent.nodename":              "gpu-node-1",
					"healthevent.isfatal":               true,
				},
			},
			wantErr: false,
		},
		{
			name: "criteria with MongoDB $ne operator",
			stage: []string{
				`{"$match" : {"healthevent.nodename": "this.healthevent.nodename", "healthevent.agent": {"$ne":"health-events-analyzer"}}}`,
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName: "test-node",
					Agent:    "health-events-analyzer",
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.nodename": "test-node",
					"healthevent.agent":    map[string]interface{}{"$ne": "health-events-analyzer"},
				},
			},
			wantErr: false,
		},
		{
			name: "criteria with errorcode array access",
			stage: []string{
				`{"$match" : {"healthevent.errorcode.0": "this.healthevent.errorcode.0", "healthevent.nodename": "this.healthevent.nodename"}}`,
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "node2",
					ErrorCode: []string{"48"},
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.errorcode.0": "48",
					"healthevent.nodename":    "node2",
				},
			},
			wantErr: false,
		},
		{
			name: "criteria with mixed types - booleans, strings, and references",
			stage: []string{
				`{"$match" : {"healthevent.isfatal": true, "healthevent.ishealthy": false, "healthevent.checkname": "this.healthevent.checkname", "healthevent.nodename": "this.healthevent.nodename", "healthevent.agent": "gpu-health-monitor", "healtheventstatus.userpodsevictionstatus.status": "this.healtheventstatus.userpodsevictionstatus.status"}}`,
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
				"$match": map[string]interface{}{
					"healthevent.isfatal":                             true,
					"healthevent.ishealthy":                           false,
					"healthevent.checkname":                           "GpuXidError",
					"healthevent.nodename":                            "node2",
					"healthevent.agent":                               "gpu-health-monitor",
					"healtheventstatus.userpodsevictionstatus.status": datamodels.StatusInProgress,
				},
			},
			wantErr: false,
		},
		{
			name: "criteria with complex $in operator",
			stage: []string{
				`{"$match" : {"healthevent.checkname": {"$in":["GpuXidError","GpuMemWatch","GpuNvswitchFatalWatch"]}, "healthevent.nodename": "this.healthevent.nodename"}}`,
			},
			event: datamodels.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:  "check-node",
					CheckName: "XID_ERROR",
				},
			},
			want: map[string]interface{}{
				"$match": map[string]interface{}{
					"healthevent.checkname": map[string]interface{}{"$in": []interface{}{"GpuXidError", "GpuMemWatch", "GpuNvswitchFatalWatch"}},
					"healthevent.nodename":  "check-node",
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, st := range tt.stage {
				got, err := ParseSequenceStage(st, tt.event)
				if (err != nil) != tt.wantErr {
					t.Errorf("ParseSequenceString() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
					t.Errorf("ParseSequenceString() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
