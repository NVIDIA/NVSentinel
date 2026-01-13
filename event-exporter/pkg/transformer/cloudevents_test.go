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

package transformer

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestToCloudEvent(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	fixedTimestamp := timestamppb.New(fixedTime)

	tests := []struct {
		name         string
		event        *pb.HealthEvent
		metadata     map[string]string
		wantErr      bool
		validateFunc func(t *testing.T, ce *CloudEvent)
	}{
		{
			name: "full event with all fields",
			event: &pb.HealthEvent{
				Version:           1,
				Agent:             "gpu-health-monitor",
				ComponentClass:    "GPU",
				CheckName:         "XidError",
				IsFatal:           true,
				IsHealthy:         false,
				Message:           "GPU XID error detected",
				RecommendedAction: pb.RecommendedAction_RESTART_VM,
				ErrorCode:         []string{"XID_48", "XID_79"},
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "GPU", EntityValue: "0"},
					{EntityType: "PCI", EntityValue: "0000:17:00.0"},
				},
				Metadata: map[string]string{
					"chassis_serial":                "SN123456",
					"providerID":                    "aws:///us-west-2a/i-1234567890",
					"topology.kubernetes.io/zone":   "us-west-2a",
					"topology.kubernetes.io/region": "us-west-2",
				},
				GeneratedTimestamp: fixedTimestamp,
				NodeName:           "gpu-node-42",
				QuarantineOverrides: &pb.BehaviourOverrides{
					Force: true,
					Skip:  false,
				},
				DrainOverrides: &pb.BehaviourOverrides{
					Force: false,
					Skip:  true,
				},
				ProcessingStrategy: pb.ProcessingStrategy_STORE_ONLY,
			},
			metadata: map[string]string{
				"cluster":     "prod-cluster-1",
				"environment": "production",
			},
			wantErr: false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				if ce.SpecVersion != "1.0" {
					t.Errorf("SpecVersion = %v, want 1.0", ce.SpecVersion)
				}
				if ce.Type != "com.nvidia.nvsentinel.health.v1" {
					t.Errorf("Type = %v, want com.nvidia.nvsentinel.health.v1", ce.Type)
				}
				if ce.Source != "nvsentinel://prod-cluster-1/healthevents" {
					t.Errorf("Source = %v, want nvsentinel://prod-cluster-1/healthevents", ce.Source)
				}
				if ce.ID == "" {
					t.Error("ID should not be empty")
				}
				if ce.Time != fixedTime.Format(time.RFC3339Nano) {
					t.Errorf("Time = %v, want %v", ce.Time, fixedTime.Format(time.RFC3339Nano))
				}

				healthEvent := ce.Data["healthEvent"].(map[string]any)
				if healthEvent["agent"] != "gpu-health-monitor" {
					t.Errorf("agent = %v, want gpu-health-monitor", healthEvent["agent"])
				}
				if healthEvent["componentClass"] != "GPU" {
					t.Errorf("componentClass = %v, want GPU", healthEvent["componentClass"])
				}
				if healthEvent["nodeName"] != "gpu-node-42" {
					t.Errorf("nodeName = %v, want gpu-node-42", healthEvent["nodeName"])
				}
				if healthEvent["recommendedAction"] != "RESTART_VM" {
					t.Errorf("recommendedAction = %v, want %v", healthEvent["recommendedAction"], "RESTART_VM")
				}
				if healthEvent["processingStrategy"] != "STORE_ONLY" {
					t.Errorf("processingStrategy = %v, want STORE_ONLY", healthEvent["processingStrategy"])
				}

				entities := healthEvent["entitiesImpacted"].([]map[string]any)
				if len(entities) != 2 {
					t.Errorf("entitiesImpacted length = %v, want 2", len(entities))
				}

				quarantine := healthEvent["quarantineOverrides"].(map[string]any)
				if quarantine["force"] != true {
					t.Errorf("quarantineOverrides.force = %v, want true", quarantine["force"])
				}

				drain := healthEvent["drainOverrides"].(map[string]any)
				if drain["skip"] != true {
					t.Errorf("drainOverrides.skip = %v, want true", drain["skip"])
				}

				metadata := ce.Data["metadata"].(map[string]string)
				if metadata["cluster"] != "prod-cluster-1" {
					t.Errorf("metadata.cluster = %v, want prod-cluster-1", metadata["cluster"])
				}
			},
		},
		{
			name: "event with nil overrides",
			event: &pb.HealthEvent{
				Version:             1,
				Agent:               "syslog-health-monitor",
				ComponentClass:      "GPU",
				CheckName:           "Test",
				NodeName:            "node-1",
				GeneratedTimestamp:  fixedTimestamp,
				QuarantineOverrides: nil,
				DrainOverrides:      nil,
			},
			metadata: map[string]string{"cluster": "test-cluster"},
			wantErr:  false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				healthEvent := ce.Data["healthEvent"].(map[string]any)
				if _, exists := healthEvent["quarantineOverrides"]; exists {
					t.Error("quarantineOverrides should not exist when nil")
				}
				if _, exists := healthEvent["drainOverrides"]; exists {
					t.Error("drainOverrides should not exist when nil")
				}
			},
		},
		{
			name: "event with empty metadata map",
			event: &pb.HealthEvent{
				Version:            1,
				Agent:              "test-agent",
				ComponentClass:     "CPU",
				CheckName:          "Test",
				NodeName:           "node-1",
				GeneratedTimestamp: fixedTimestamp,
				Metadata:           map[string]string{},
			},
			metadata: map[string]string{"cluster": "test-cluster"},
			wantErr:  false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				healthEvent := ce.Data["healthEvent"].(map[string]any)
				if _, exists := healthEvent["metadata"]; exists {
					t.Error("metadata should not exist when empty")
				}
			},
		},
		{
			name: "event with multiple entities impacted",
			event: &pb.HealthEvent{
				Version:            1,
				Agent:              "test-agent",
				ComponentClass:     "GPU",
				CheckName:          "Test",
				NodeName:           "node-1",
				GeneratedTimestamp: fixedTimestamp,
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "GPU", EntityValue: "0"},
					{EntityType: "GPU", EntityValue: "1"},
					{EntityType: "GPU", EntityValue: "2"},
					{EntityType: "PCI", EntityValue: "0000:17:00.0"},
				},
			},
			metadata: map[string]string{"cluster": "test-cluster"},
			wantErr:  false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				healthEvent := ce.Data["healthEvent"].(map[string]any)
				entities := healthEvent["entitiesImpacted"].([]map[string]any)
				if len(entities) != 4 {
					t.Errorf("entitiesImpacted length = %v, want 4", len(entities))
				}
				if entities[0]["entityType"] != "GPU" || entities[0]["entityValue"] != "0" {
					t.Errorf("first entity incorrect: %v", entities[0])
				}
			},
		},
		{
			name: "metadata with missing cluster returns error",
			event: &pb.HealthEvent{
				Version:            1,
				Agent:              "test-agent",
				ComponentClass:     "GPU",
				CheckName:          "Test",
				NodeName:           "node-1",
				GeneratedTimestamp: fixedTimestamp,
			},
			metadata: map[string]string{"environment": "test"},
			wantErr:  true,
		},
		{
			name: "event with empty errorCode array",
			event: &pb.HealthEvent{
				Version:            1,
				Agent:              "test-agent",
				ComponentClass:     "GPU",
				CheckName:          "Test",
				NodeName:           "node-1",
				GeneratedTimestamp: fixedTimestamp,
				ErrorCode:          []string{},
			},
			metadata: map[string]string{"cluster": "test-cluster"},
			wantErr:  false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				healthEvent := ce.Data["healthEvent"].(map[string]any)
				errorCode := healthEvent["errorCode"].([]string)
				if len(errorCode) != 0 {
					t.Errorf("errorCode length = %v, want 0", len(errorCode))
				}
			},
		},
		{
			name: "event with nil entitiesImpacted",
			event: &pb.HealthEvent{
				Version:            1,
				Agent:              "test-agent",
				ComponentClass:     "GPU",
				CheckName:          "Test",
				NodeName:           "node-1",
				GeneratedTimestamp: fixedTimestamp,
				EntitiesImpacted:   nil,
			},
			metadata: map[string]string{"cluster": "test-cluster"},
			wantErr:  false,
			validateFunc: func(t *testing.T, ce *CloudEvent) {
				healthEvent := ce.Data["healthEvent"].(map[string]any)
				entities := healthEvent["entitiesImpacted"].([]map[string]any)
				if len(entities) != 0 {
					t.Errorf("entitiesImpacted length = %v, want 0", len(entities))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce, err := ToCloudEvent(tt.event, tt.metadata)

			if (err != nil) != tt.wantErr {
				t.Errorf("ToCloudEvent() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.validateFunc != nil {
				tt.validateFunc(t, ce)
			}
		})
	}
}
