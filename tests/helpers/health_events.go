// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package helpers

import (
	"encoding/json"
	"fmt"
	"os"
)

type HealthEventTemplate struct {
	Version             int                  `json:"version"`
	Agent               string               `json:"agent"`
	ComponentClass      string               `json:"componentClass,omitempty"`
	CheckName           string               `json:"checkName"`
	IsFatal             bool                 `json:"isFatal"`
	IsHealthy           bool                 `json:"isHealthy"`
	Message             string               `json:"message"`
	RecommendedAction   int                  `json:"recommendedAction,omitempty"`
	ErrorCode           []string             `json:"errorCode,omitempty"`
	EntitiesImpacted    []EntityImpacted     `json:"entitiesImpacted,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
	QuarantineOverrides *QuarantineOverrides `json:"quarantineOverrides,omitempty"`
	NodeName            string               `json:"nodeName"`
}

type EntityImpacted struct {
	EntityType  string `json:"entityType"`
	EntityValue string `json:"entityValue"`
}

type QuarantineOverrides struct {
	Force bool `json:"force"`
}

func NewHealthEvent(nodeName string) *HealthEventTemplate {
	return &HealthEventTemplate{
		Version:        1,
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		IsFatal:        true,
		IsHealthy:      false,
		NodeName:       nodeName,
		EntitiesImpacted: []EntityImpacted{
			{
				EntityType:  "GPU",
				EntityValue: "0",
			},
		},
	}
}

func (h *HealthEventTemplate) WithAgent(agent string) *HealthEventTemplate {
	h.Agent = agent
	return h
}

func (h *HealthEventTemplate) WithCheckName(checkName string) *HealthEventTemplate {
	h.CheckName = checkName
	return h
}

func (h *HealthEventTemplate) WithErrorCode(codes ...string) *HealthEventTemplate {
	h.ErrorCode = codes
	return h
}

func (h *HealthEventTemplate) WithComponentClass(class string) *HealthEventTemplate {
	h.ComponentClass = class
	return h
}

func (h *HealthEventTemplate) WithEntity(entityType, entityValue string) *HealthEventTemplate {
	h.EntitiesImpacted = append(h.EntitiesImpacted, EntityImpacted{
		EntityType:  entityType,
		EntityValue: entityValue,
	})
	return h
}

func (h *HealthEventTemplate) WithFatal(isFatal bool) *HealthEventTemplate {
	h.IsFatal = isFatal
	return h
}

func (h *HealthEventTemplate) WithHealthy(isHealthy bool) *HealthEventTemplate {
	h.IsHealthy = isHealthy
	return h
}

func (h *HealthEventTemplate) WithMessage(message string) *HealthEventTemplate {
	h.Message = message
	return h
}

func (h *HealthEventTemplate) WithForceOverride() *HealthEventTemplate {
	h.QuarantineOverrides = &QuarantineOverrides{Force: true}
	if h.Metadata == nil {
		h.Metadata = make(map[string]string)
	}
	h.Metadata["creator_id"] = "test"
	return h
}

func (h *HealthEventTemplate) WithMetadata(key, value string) *HealthEventTemplate {
	if h.Metadata == nil {
		h.Metadata = make(map[string]string)
	}
	h.Metadata[key] = value
	return h
}

func (h *HealthEventTemplate) WriteToTempFile() (string, error) {
	tempFile, err := os.CreateTemp("", "health-event-*.json")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	content, err := json.MarshalIndent(h, "", "    ")
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return "", fmt.Errorf("failed to marshal health event: %w", err)
	}

	if _, err := tempFile.Write(content); err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}

	tempFile.Close()
	return tempFile.Name(), nil
}

func SendHealthEventWithTemplate(nodeName string, event *HealthEventTemplate) (string, error) {
	tempFile, err := event.WriteToTempFile()
	if err != nil {
		return "", err
	}

	err = SendHealthEventsToNodes([]string{nodeName}, tempFile)
	if err != nil {
		os.Remove(tempFile)
		return "", err
	}

	return tempFile, nil
}
