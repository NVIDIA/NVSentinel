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
	"fmt"
	"time"

	"github.com/google/uuid"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type CloudEvent struct {
	SpecVersion string         `json:"specversion"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	ID          string         `json:"id"`
	Time        string         `json:"time"`
	Data        map[string]any `json:"data"`
}

func ToCloudEvent(event *pb.HealthEvent, metadata map[string]string) (*CloudEvent, error) {
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	if event.GeneratedTimestamp != nil {
		timestamp = event.GeneratedTimestamp.AsTime().UTC().Format(time.RFC3339Nano)
	}

	entities := make([]map[string]any, 0, len(event.EntitiesImpacted))
	for _, e := range event.EntitiesImpacted {
		entities = append(entities, map[string]any{
			"entityType":  e.EntityType,
			"entityValue": e.EntityValue,
		})
	}

	errorCodes := make([]string, len(event.ErrorCode))

	copy(errorCodes, event.ErrorCode)

	healthEventData := map[string]any{
		"version":            event.Version,
		"agent":              event.Agent,
		"componentClass":     event.ComponentClass,
		"checkName":          event.CheckName,
		"isFatal":            event.IsFatal,
		"isHealthy":          event.IsHealthy,
		"message":            event.Message,
		"recommendedAction":  event.RecommendedAction.String(),
		"errorCode":          errorCodes,
		"entitiesImpacted":   entities,
		"generatedTimestamp": timestamp,
		"nodeName":           event.NodeName,
	}

	if len(event.Metadata) > 0 {
		healthEventData["metadata"] = event.Metadata
	}

	if event.QuarantineOverrides != nil {
		healthEventData["quarantineOverrides"] = map[string]any{
			"force": event.QuarantineOverrides.Force,
			"skip":  event.QuarantineOverrides.Skip,
		}
	}

	if event.DrainOverrides != nil {
		healthEventData["drainOverrides"] = map[string]any{
			"force": event.DrainOverrides.Force,
			"skip":  event.DrainOverrides.Skip,
		}
	}

	clusterName, ok := metadata["cluster"]
	if !ok || clusterName == "" {
		return nil, fmt.Errorf("metadata must contain a non-empty 'cluster' key")
	}

	return &CloudEvent{
		SpecVersion: "1.0",
		Type:        "com.nvidia.nvsentinel.health.v1",
		Source:      fmt.Sprintf("nvsentinel://%s/healthevents", clusterName),
		ID:          uuid.New().String(),
		Time:        timestamp,
		Data: map[string]any{
			"metadata":    metadata,
			"healthEvent": healthEventData,
		},
	}, nil
}
