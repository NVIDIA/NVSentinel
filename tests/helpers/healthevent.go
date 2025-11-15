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

package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func (h *HealthEventTemplate) WithRecommendedAction(action int) *HealthEventTemplate {
	h.RecommendedAction = action
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

func SendHealthEventsToNodes(nodeNames []string, eventFilePath string) error {
	eventData, err := os.ReadFile(eventFilePath)
	if err != nil {
		return fmt.Errorf("failed to read health event file %s: %w", eventFilePath, err)
	}

	return sendHealthEventData(nodeNames, eventData)
}

func sendHealthEventData(nodeNames []string, eventData []byte) error {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, nodeName := range nodeNames {
		wg.Add(1)

		go func(nodeName string) {
			defer wg.Done()

			eventJSON := strings.ReplaceAll(string(eventData), "NODE_NAME", nodeName)

			req, err := http.NewRequestWithContext(
				context.Background(), "POST", "http://localhost:8080/health-event",
				strings.NewReader(eventJSON),
			)
			if err != nil {
				mu.Lock()
				defer mu.Unlock()

				errs = append(errs,
					fmt.Errorf("failed to create request for node %s: %w", nodeName, err))

				return
			}

			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				defer mu.Unlock()

				errs = append(errs,
					fmt.Errorf("failed to send health event to node %s: %w", nodeName, err))

				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)

				mu.Lock()
				defer mu.Unlock()

				errs = append(errs, fmt.Errorf(
					"health event to node %s failed: expected status 200, got %d. "+
						"Response: %s",
					nodeName, resp.StatusCode, string(body),
				))
			}
		}(nodeName)
	}

	wg.Wait()

	return errors.Join(errs...)
}

func SendHealthEvent(ctx context.Context, t *testing.T, event *HealthEventTemplate) {
	t.Helper()
	t.Logf("Sending health event to node %s: checkName=%s, isFatal=%v",
		event.NodeName, event.CheckName, event.IsFatal)

	eventData, err := json.MarshalIndent(event, "", "    ")
	require.NoError(t, err)

	err = sendHealthEventData([]string{event.NodeName}, eventData)
	require.NoError(t, err)

	t.Logf("Health event sent successfully")
}

func SendHealthyEvent(ctx context.Context, t *testing.T, nodeName string) {
	t.Helper()
	t.Logf("Sending generic healthy event to node %s", nodeName)
	event := NewHealthEvent(nodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		WithComponentClass("GPU")

	SendHealthEvent(ctx, t, event)
}
