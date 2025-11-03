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

package model

import (
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Status string

const (
	StatusNotStarted Status = "NotStarted"
	StatusInProgress Status = "InProgress"
	StatusFailed     Status = "Failed"
	StatusSucceeded  Status = "Succeeded"
	AlreadyDrained   Status = "AlreadyDrained"
)

const (
	UnQuarantined      Status = "UnQuarantined"
	Quarantined        Status = "Quarantined"
	AlreadyQuarantined Status = "AlreadyQuarantined"
)

type OperationStatus struct {
	Status  Status `bson:"status"`
	Message string `bson:"message,omitempty"`
}

type HealthEventStatus struct {
	NodeQuarantined          *Status         `bson:"nodequarantined"`
	UserPodsEvictionStatus   OperationStatus `bson:"userpodsevictionstatus"`
	FaultRemediated          *bool           `bson:"faultremediated"`
	LastRemediationTimestamp *time.Time      `bson:"lastremediationtimestamp,omitempty"`
}

type HealthEventWithStatus struct {
	CreatedAt         time.Time           `bson:"createdAt"`
	HealthEvent       *protos.HealthEvent `bson:"healthevent,omitempty"`
	HealthEventStatus HealthEventStatus   `bson:"healtheventstatus"`
}

// MarshalBSON implements custom BSON marshaling for HealthEventWithStatus.
// This is necessary because protobuf-generated structs don't have BSON tags,
// so we need to manually convert the HealthEvent to a BSON-compatible format.
func (h *HealthEventWithStatus) MarshalBSON() ([]byte, error) {
	if h.HealthEvent == nil {
		return bson.Marshal(bson.M{
			"createdAt":         h.CreatedAt,
			"healtheventstatus": h.HealthEventStatus,
		})
	}

	// Convert protobuf HealthEvent to BSON-compatible map
	healthEventDoc := bson.M{
		"version":       h.HealthEvent.Version,
		"agent":         h.HealthEvent.Agent,
		"componentClass": h.HealthEvent.ComponentClass,
		"checkName":     h.HealthEvent.CheckName,
		"isFatal":       h.HealthEvent.IsFatal,
		"isHealthy":     h.HealthEvent.IsHealthy,
		"message":       h.HealthEvent.Message,
		"recommendedAction": h.HealthEvent.RecommendedAction.String(),
		"errorCode":     h.HealthEvent.ErrorCode,
		"nodeName":      h.HealthEvent.NodeName,
	}

	// Add metadata map (this is where your enriched fields are!)
	if h.HealthEvent.Metadata != nil && len(h.HealthEvent.Metadata) > 0 {
		healthEventDoc["metadata"] = h.HealthEvent.Metadata
	}

	// Convert timestamp
	if h.HealthEvent.GeneratedTimestamp != nil {
		healthEventDoc["generatedTimestamp"] = h.HealthEvent.GeneratedTimestamp.AsTime()
	}

	// Convert entitiesImpacted
	if len(h.HealthEvent.EntitiesImpacted) > 0 {
		entities := make([]bson.M, 0, len(h.HealthEvent.EntitiesImpacted))
		for _, entity := range h.HealthEvent.EntitiesImpacted {
			entities = append(entities, bson.M{
				"entityType":  entity.EntityType,
				"entityValue": entity.EntityValue,
			})
		}
		healthEventDoc["entitiesImpacted"] = entities
	}

	// Convert overrides
	if h.HealthEvent.QuarantineOverrides != nil {
		healthEventDoc["quarantineOverrides"] = bson.M{
			"force": h.HealthEvent.QuarantineOverrides.Force,
			"skip":  h.HealthEvent.QuarantineOverrides.Skip,
		}
	}

	if h.HealthEvent.DrainOverrides != nil {
		healthEventDoc["drainOverrides"] = bson.M{
			"force": h.HealthEvent.DrainOverrides.Force,
			"skip":  h.HealthEvent.DrainOverrides.Skip,
		}
	}

	// Marshal the complete document
	return bson.Marshal(bson.M{
		"createdAt":         h.CreatedAt,
		"healthevent":       healthEventDoc,
		"healtheventstatus": h.HealthEventStatus,
	})
}

// UnmarshalBSON implements custom BSON unmarshaling for HealthEventWithStatus.
// This is the reverse operation - converting BSON back to protobuf struct.
func (h *HealthEventWithStatus) UnmarshalBSON(data []byte) error {
	type Alias struct {
		CreatedAt         time.Time         `bson:"createdAt"`
		HealthEventStatus HealthEventStatus `bson:"healtheventstatus"`
		HealthEventDoc    bson.M            `bson:"healthevent"`
	}

	var aux Alias
	if err := bson.Unmarshal(data, &aux); err != nil {
		return err
	}

	h.CreatedAt = aux.CreatedAt
	h.HealthEventStatus = aux.HealthEventStatus

	if aux.HealthEventDoc == nil {
		return nil
	}

	// Convert BSON map back to protobuf HealthEvent
	h.HealthEvent = &protos.HealthEvent{}

	if val, ok := aux.HealthEventDoc["version"].(int32); ok {
		h.HealthEvent.Version = uint32(val)
	}
	if val, ok := aux.HealthEventDoc["agent"].(string); ok {
		h.HealthEvent.Agent = val
	}
	if val, ok := aux.HealthEventDoc["componentClass"].(string); ok {
		h.HealthEvent.ComponentClass = val
	}
	if val, ok := aux.HealthEventDoc["checkName"].(string); ok {
		h.HealthEvent.CheckName = val
	}
	if val, ok := aux.HealthEventDoc["isFatal"].(bool); ok {
		h.HealthEvent.IsFatal = val
	}
	if val, ok := aux.HealthEventDoc["isHealthy"].(bool); ok {
		h.HealthEvent.IsHealthy = val
	}
	if val, ok := aux.HealthEventDoc["message"].(string); ok {
		h.HealthEvent.Message = val
	}
	if val, ok := aux.HealthEventDoc["nodeName"].(string); ok {
		h.HealthEvent.NodeName = val
	}

	// Unmarshal metadata (your enriched fields!)
	if metadataRaw, ok := aux.HealthEventDoc["metadata"]; ok {
		if metadata, ok := metadataRaw.(bson.M); ok {
			h.HealthEvent.Metadata = make(map[string]string)
			for k, v := range metadata {
				if strVal, ok := v.(string); ok {
					h.HealthEvent.Metadata[k] = strVal
				}
			}
		}
	}

	// Unmarshal timestamp
	if tsRaw, ok := aux.HealthEventDoc["generatedTimestamp"]; ok {
		if ts, ok := tsRaw.(time.Time); ok {
			h.HealthEvent.GeneratedTimestamp = timestamppb.New(ts)
		}
	}

	return nil
}
