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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
)

type CSPType string

const (
	CSPGCP CSPType = "gcp"
	CSPAWS CSPType = "aws"
)

type CSPMaintenanceEvent struct {
	ID              string     `json:"id,omitempty"`
	CSP             CSPType    `json:"csp"`
	InstanceID      string     `json:"instanceId"`
	NodeName        string     `json:"nodeName,omitempty"`
	Zone            string     `json:"zone,omitempty"`
	Region          string     `json:"region,omitempty"`
	ProjectID       string     `json:"projectId,omitempty"`
	AccountID       string     `json:"accountId,omitempty"`
	Status          string     `json:"status"`
	EventTypeCode   string     `json:"eventTypeCode,omitempty"`
	MaintenanceType string     `json:"maintenanceType,omitempty"`
	ScheduledStart  *time.Time `json:"scheduledStart,omitempty"`
	ScheduledEnd    *time.Time `json:"scheduledEnd,omitempty"`
	Description     string     `json:"description,omitempty"`
	EventARN        string     `json:"eventArn,omitempty"`
	EntityARN       string     `json:"entityArn,omitempty"`
}

type CSPAPIMockClient struct {
	baseURL    string
	httpClient *http.Client
	stopChan   chan struct{}
}

func NewCSPAPIMockClient(client klient.Client) (*CSPAPIMockClient, error) {
	pods := &v1.PodList{}

	err := client.Resources(NVSentinelNamespace).List(context.Background(), pods, func(o *metav1.ListOptions) {
		o.LabelSelector = "app=csp-api-mock"
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, fmt.Errorf("failed to find csp-api-mock pod: %w", err)
	}

	stopChan, readyChan := PortForwardPod(
		context.Background(),
		client.RESTConfig(),
		NVSentinelNamespace,
		pods.Items[0].Name,
		18081,
		8080,
	)

	select {
	case <-readyChan:
	case <-time.After(10 * time.Second):
		close(stopChan)
		return nil, fmt.Errorf("port-forward timeout for csp-api-mock")
	}

	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 5 * time.Second
	retryClient.Logger = log.Default()
	retryClient.HTTPClient.Timeout = 30 * time.Second

	return &CSPAPIMockClient{
		baseURL:    "http://localhost:18081",
		httpClient: retryClient.StandardClient(),
		stopChan:   stopChan,
	}, nil
}

func (c *CSPAPIMockClient) Close() {
	if c.stopChan != nil {
		close(c.stopChan)
	}
}

// InjectEvent injects or updates an event. Returns (eventID, eventARN, error).
// eventARN is only populated for AWS events.
func (c *CSPAPIMockClient) InjectEvent(event CSPMaintenanceEvent) (string, string, error) {
	endpoint := fmt.Sprintf("/%s/inject", event.CSP)

	resp, err := c.post(endpoint, event)
	if err != nil {
		return "", "", err
	}

	var result struct {
		EventID  string `json:"eventId"`
		EventARN string `json:"eventArn,omitempty"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", "", fmt.Errorf("failed to decode response: %w", err)
	}

	return result.EventID, result.EventARN, nil
}

func (c *CSPAPIMockClient) UpdateEventStatus(csp CSPType, eventID, newStatus string) error {
	_, _, err := c.InjectEvent(CSPMaintenanceEvent{ID: eventID, CSP: csp, Status: newStatus})
	return err
}

func (c *CSPAPIMockClient) UpdateGCPEventScheduledTime(eventID string, scheduledStart time.Time) error {
	_, _, err := c.InjectEvent(CSPMaintenanceEvent{ID: eventID, CSP: CSPGCP, ScheduledStart: &scheduledStart})
	return err
}

func (c *CSPAPIMockClient) ClearEvents(csp CSPType) error {
	return c.postEmpty(fmt.Sprintf("/%s/events/clear", csp))
}

func (c *CSPAPIMockClient) post(endpoint string, payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, c.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (c *CSPAPIMockClient) postEmpty(endpoint string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.baseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return nil
}
