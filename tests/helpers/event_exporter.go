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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const EventExporterName = "event-exporter"

// GetMockEvents retrieves all events from the event-exporter mock server
func GetMockEvents(t *testing.T, c *envconf.Config) []map[string]any {
	client, err := c.NewClient()
	if err != nil {
		t.Logf("Failed to create client: %v", err)
		return nil
	}

	pods := &v1.PodList{}

	err = client.Resources(NVSentinelNamespace).List(context.Background(), pods, func(o *metav1.ListOptions) {
		o.LabelSelector = "app=event-exporter-mock"
	})
	if err != nil || len(pods.Items) == 0 {
		t.Logf("Failed to find mock pod: %v", err)
		return nil
	}

	podName := pods.Items[0].Name
	localPort := 18443

	stopChan, readyChan := PortForwardPod(
		context.Background(),
		client.RESTConfig(),
		NVSentinelNamespace,
		podName,
		localPort,
		8443,
	)
	defer close(stopChan)

	select {
	case <-readyChan:
	case <-time.After(10 * time.Second):
		t.Log("Port-forward timeout")
		return nil
	}

	ctx := context.Background()
	url := fmt.Sprintf("http://localhost:%d/events/list", localPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Logf("Failed to create request: %v", err)
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("Failed to get events: %v", err)
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Events []map[string]any `json:"events"`
		Count  int              `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Logf("Failed to decode events: %v", err)
		return nil
	}

	return result.Events
}

// GetMockEventCount retrieves the total count of events from the event-exporter mock server
func GetMockEventCount(t *testing.T, c *envconf.Config) int64 {
	t.Helper()

	body := fetchMockMetrics(t, c)
	if body == nil {
		return 0
	}

	return parseEventCount(body)
}

func fetchMockMetrics(t *testing.T, c *envconf.Config) []byte {
	t.Helper()

	client, err := c.NewClient()
	if err != nil {
		t.Logf("Failed to create client: %v", err)
		return nil
	}

	pods := &v1.PodList{}

	err = client.Resources(NVSentinelNamespace).List(context.Background(), pods, func(o *metav1.ListOptions) {
		o.LabelSelector = "app=event-exporter-mock"
	})
	if err != nil || len(pods.Items) == 0 {
		t.Logf("Failed to find mock pod: %v", err)
		return nil
	}

	podName := pods.Items[0].Name
	localPort := 18443

	stopChan, readyChan := PortForwardPod(
		context.Background(),
		client.RESTConfig(),
		NVSentinelNamespace,
		podName,
		localPort,
		8443,
	)
	defer close(stopChan)

	select {
	case <-readyChan:
	case <-time.After(10 * time.Second):
		t.Log("Port-forward timeout")
		return nil
	}

	ctx := context.Background()
	url := fmt.Sprintf("http://localhost:%d/metrics", localPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Logf("Failed to create request: %v", err)
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("Failed to get metrics: %v", err)
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	return body
}

func parseEventCount(body []byte) int64 {
	var count int64

	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, "mock_events_received_total") {
			_, _ = fmt.Sscanf(line, "mock_events_received_total %d", &count)
			break
		}
	}

	return count
}

// FindEventByNodeAndMessage searches for a CloudEvent matching the given nodeName and message
func FindEventByNodeAndMessage(events []map[string]any, nodeName, message string) (map[string]any, bool) {
	for _, event := range events {
		data, ok := event["data"].(map[string]any)
		if !ok {
			continue
		}

		healthEvent, ok := data["healthEvent"].(map[string]any)
		if !ok {
			continue
		}

		eventNodeName, ok := healthEvent["nodeName"].(string)
		if !ok {
			continue
		}

		eventMessage, ok := healthEvent["message"].(string)
		if !ok {
			continue
		}

		if eventNodeName == nodeName && eventMessage == message {
			return event, true
		}
	}

	return nil, false
}

// ValidateCloudEvent validates that a CloudEvent has the expected structure and content
func ValidateCloudEvent(
	t *testing.T,
	event map[string]any,
	expectedNodeName, expectedMessage, expectedCheckName, expectedErrorCode string,
	expectedProcessingStrategy string,
) {
	t.Helper()
	t.Logf("Validating CloudEvent: %+v", event)

	require.Equal(t, "1.0", event["specversion"])
	require.Equal(t, "com.nvidia.nvsentinel.health.v1", event["type"])
	require.Contains(t, event["source"], "nvsentinel")
	require.NotEmpty(t, event["id"])
	require.NotEmpty(t, event["time"])

	data, ok := event["data"].(map[string]any)
	require.True(t, ok, "event data should be a map")

	healthEvent, ok := data["healthEvent"].(map[string]any)
	require.True(t, ok, "data.healthEvent should be a map")

	require.Equal(t, expectedCheckName, healthEvent["checkName"])
	require.Equal(t, expectedNodeName, healthEvent["nodeName"])
	require.Equal(t, expectedMessage, healthEvent["message"])
	require.Equal(t, expectedProcessingStrategy, healthEvent["processingStrategy"])
	require.Contains(t, healthEvent["errorCode"], expectedErrorCode)
}
