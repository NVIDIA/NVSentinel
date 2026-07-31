// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	lambdaapi "github.com/nvidia/nvsentinel/commons/pkg/lambda"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	// CSPLambda is the CSP identifier for Lambda.
	CSPLambda model.CSP = "lambda"

	maintenanceEventsPath = "/api/v1/maintenance_events"
)

// Event mirrors the Lambda maintenance API event shape.
type Event struct {
	ID                string     `json:"id"`
	EntityLRNs        []string   `json:"entity_lrns"`
	MaintenanceType   *string    `json:"maintenance_type"` // null in current API
	WorkspaceID       string     `json:"workspace_id"`
	Detail            string     `json:"detail"`
	Urgency           string     `json:"urgency"`
	Status            string     `json:"status"`
	NotBefore         *time.Time `json:"not_before"`
	NotBeforeDeadline *time.Time `json:"not_before_deadline"`
	NotAfter          *time.Time `json:"not_after"`
	LastUpdated       *time.Time `json:"last_updated"`
}

// apiResponse is the top-level structure of the Lambda maintenance events API response.
type apiResponse struct {
	Data struct {
		MaintenanceEvents []Event `json:"maintenance_events"`
		PageToken         *string `json:"page_token"`
	} `json:"data"`
}

// mockEventsFile is the top-level structure of the mock events JSON file (dev/test only).
type mockEventsFile struct {
	Events []Event `json:"events"`
}

// eventsSource abstracts fetching maintenance events from either the real API or a local file.
type eventsSource interface {
	fetchEvents(ctx context.Context) ([]Event, error)
}

// apiSource fetches events from the real Lambda maintenance API.
type apiSource struct {
	client *lambdaapi.Client
}

func (s *apiSource) fetchEvents(ctx context.Context) ([]Event, error) {
	var allEvents []Event

	var pageToken *string

	for {
		q := url.Values{}
		if pageToken != nil {
			q.Set("page_token", *pageToken)
		}

		var parsed apiResponse
		if err := s.client.Get(ctx, maintenanceEventsPath, q, &parsed); err != nil {
			return nil, err
		}

		allEvents = append(allEvents, parsed.Data.MaintenanceEvents...)

		if parsed.Data.PageToken == nil {
			break
		}

		pageToken = parsed.Data.PageToken
	}

	return allEvents, nil
}

// fileSource fetches events from a local JSON file (dev/test only).
type fileSource struct {
	path string
}

func (s *fileSource) fetchEvents(_ context.Context) ([]Event, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", s.path, err)
	}

	var f mockEventsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return f.Events, nil
}

// Client implements csp.Monitor for Lambda.
type Client struct {
	cfg              config.LambdaConfig
	clusterName      string
	triggerTimeLimit time.Duration
	nodeInformer     *NodeInformer
	normalizer       eventpkg.Normalizer
	source           eventsSource
}

// NewClient constructs a Lambda Client and starts the node informer.
// If cfg.MockEventsFilePath is set, a file-based source is used (dev/test).
// Otherwise, the real Lambda API is used with the LAMBDA_API_KEY env var.
// triggerTimeLimit must match TriggerQuarantineWorkflowTimeLimitMinutes from the
// top-level config so emergency events stay within the trigger-engine query window.
func NewClient(
	ctx context.Context,
	cfg config.LambdaConfig,
	clusterName string,
	triggerTimeLimit time.Duration,
	kubeconfigPath string,
	_ interface{}, // store — reserved for future use
) (*Client, error) {
	k8sClient, err := buildK8sClient(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes client: %w", err)
	}

	nodeInformer, err := NewNodeInformer(k8sClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create Lambda node informer: %w", err)
	}

	nodeInformer.Start(ctx)

	normalizer, err := eventpkg.GetNormalizer(CSPLambda)
	if err != nil {
		return nil, fmt.Errorf("failed to get Lambda normalizer: %w", err)
	}

	var source eventsSource
	if cfg.MockEventsFilePath != "" {
		slog.Info("Lambda client: using mock events file (dev/test mode)", "path", cfg.MockEventsFilePath)
		source = &fileSource{path: cfg.MockEventsFilePath}
	} else {
		slog.Info("Lambda client: using real API", "endpoint", cfg.APIEndpoint)
		source = &apiSource{client: lambdaapi.NewClient(cfg.APIEndpoint)}
	}

	return &Client{
		cfg:              cfg,
		clusterName:      clusterName,
		triggerTimeLimit: triggerTimeLimit,
		nodeInformer:     nodeInformer,
		normalizer:       normalizer,
		source:           source,
	}, nil
}

// GetName returns the CSP identifier.
func (c *Client) GetName() model.CSP {
	return CSPLambda
}

// StartMonitoring polls for maintenance events on each tick and emits normalized
// MaintenanceEvents onto eventChan.
func (c *Client) StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	ticker := time.NewTicker(time.Duration(c.cfg.PollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	if err := c.pollEvents(ctx, eventChan); err != nil {
		slog.Error("Lambda: initial poll error", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.pollEvents(ctx, eventChan); err != nil {
				slog.Error("Lambda: poll error", "error", err)
			}
		}
	}
}

func (c *Client) pollEvents(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	events, err := c.source.fetchEvents(ctx)
	if err != nil {
		return err
	}

	slog.Debug("Lambda: fetched events", "count", len(events))

	for _, raw := range events {
		nodeName := c.resolveNodeName(raw)
		if nodeName == "" {
			slog.Warn("Lambda: skipping event, could not resolve node name",
				"eventID", raw.ID,
				"entityLRNs", raw.EntityLRNs)
			continue
		}

		meta := eventpkg.LambdaEventMetadata{
			ID:                raw.ID,
			Detail:            raw.Detail,
			Urgency:           raw.Urgency,
			Status:            raw.Status,
			NotBefore:         raw.NotBefore,
			NotBeforeDeadline: raw.NotBeforeDeadline,
			NotAfter:          raw.NotAfter,
			LastUpdated:       raw.LastUpdated,
			NodeName:          nodeName,
			ClusterName:       c.clusterName,
			TriggerTimeLimit:  c.triggerTimeLimit,
		}

		normalized, err := c.normalizer.Normalize(nil, meta)
		if err != nil {
			slog.Error("Lambda: failed to normalize event", "eventID", raw.ID, "error", err)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- *normalized:
			slog.Debug("Lambda: emitted event", "eventID", normalized.EventID, "status", normalized.Status)
		}
	}

	return nil
}

// resolveNodeName extracts the instance UUID from the first entity LRN and
// looks it up in the node informer map.
func (c *Client) resolveNodeName(event Event) string {
	if len(event.EntityLRNs) == 0 {
		return ""
	}

	uuid := extractUUIDFromLRN(event.EntityLRNs[0])
	if uuid == "" {
		slog.Warn("Lambda: could not parse instance UUID from LRN", "lrn", event.EntityLRNs[0])
		return ""
	}

	nodeName, ok := c.nodeInformer.GetNodeName(uuid)
	if !ok {
		slog.Warn("Lambda: instance UUID not found in node informer", "uuid", uuid)
		return ""
	}

	return nodeName
}

// extractUUIDFromLRN parses "lrn:cloud:instance:<uuid>" and returns the UUID.
func extractUUIDFromLRN(lrn string) string {
	parts := strings.Split(lrn, ":")
	for i, part := range parts {
		if part == "instance" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return ""
}

func buildK8sClient(kubeconfigPath string) (kubernetes.Interface, error) {
	var k8sCfg *rest.Config
	var err error

	if kubeconfigPath != "" {
		k8sCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		k8sCfg, err = rest.InClusterConfig()
	}

	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}

	return kubernetes.NewForConfig(k8sCfg)
}
