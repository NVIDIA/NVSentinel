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
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
)

// portAllocator tracks used ports to avoid conflicts between tests
var portAllocator = struct {
	sync.Mutex
	nextPort int
}{nextPort: 18081}

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
	localPort  int
	closed     bool
	closeMu    sync.Mutex
}

// NewCSPAPIMockClient creates a new client for the CSP API Mock service.
// It sets up port-forwarding and registers cleanup with t.Cleanup() to ensure
// the port-forward is closed even if the test fails.
func NewCSPAPIMockClient(t *testing.T, client klient.Client) (*CSPAPIMockClient, error) {
	t.Helper()

	pods := &v1.PodList{}

	err := client.Resources(NVSentinelNamespace).List(context.Background(), pods, func(o *metav1.ListOptions) {
		o.LabelSelector = "app=csp-api-mock"
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, fmt.Errorf("failed to find csp-api-mock pod: %w", err)
	}

	// Get a unique local port to avoid conflicts between tests
	localPort := getNextAvailablePort()

	stopChan, readyChan := PortForwardPod(
		context.Background(),
		client.RESTConfig(),
		NVSentinelNamespace,
		pods.Items[0].Name,
		localPort,
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

	mockClient := &CSPAPIMockClient{
		baseURL:    fmt.Sprintf("http://localhost:%d", localPort),
		httpClient: retryClient.StandardClient(),
		stopChan:   stopChan,
		localPort:  localPort,
	}

	// Register cleanup with t.Cleanup() to ensure port-forward is closed
	// even if the test fails at any point
	t.Cleanup(func() {
		mockClient.Close()
	})

	return mockClient, nil
}

// getNextAvailablePort finds an available port for port-forwarding.
// It tries ports starting from the last used port to avoid conflicts.
func getNextAvailablePort() int {
	portAllocator.Lock()
	defer portAllocator.Unlock()

	lc := &net.ListenConfig{}

	// Try up to 100 ports to find an available one
	for i := 0; i < 100; i++ {
		port := portAllocator.nextPort
		portAllocator.nextPort++

		if portAllocator.nextPort > 19000 {
			portAllocator.nextPort = 18081
		}

		// Check if port is available
		listener, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("localhost:%d", port))
		if err == nil {
			listener.Close()
			return port
		}
	}

	// Fallback to the base port if all else fails
	return portAllocator.nextPort
}

func (c *CSPAPIMockClient) Close() {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	if c.closed {
		return
	}

	c.closed = true

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

// GetEventCount returns the number of events in the mock store for the given CSP.
func (c *CSPAPIMockClient) GetEventCount(csp CSPType) (int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		c.baseURL+fmt.Sprintf("/%s/events", csp), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var events []interface{}
	if err := json.Unmarshal(respBody, &events); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return len(events), nil
}

// GetPollCount returns the number of times the CSP health monitor has polled the mock.
func (c *CSPAPIMockClient) GetPollCount(csp CSPType) (int64, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		c.baseURL+fmt.Sprintf("/%s/stats", csp), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var stats struct {
		PollCount int64 `json:"pollCount"`
	}
	if err := json.Unmarshal(respBody, &stats); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return stats.PollCount, nil
}

// ResetPollCount resets the poll counter for the given CSP.
func (c *CSPAPIMockClient) ResetPollCount(csp CSPType) error {
	return c.postEmpty(fmt.Sprintf("/%s/stats/reset", csp))
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
