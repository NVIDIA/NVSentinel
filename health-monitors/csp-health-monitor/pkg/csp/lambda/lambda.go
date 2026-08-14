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
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	lambdaapi "github.com/nvidia/nvsentinel/commons/pkg/lambda"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// mockEventsFile is the top-level structure of the mock events JSON file (dev/test only).
type mockEventsFile struct {
	Events []lambdaapi.Event `json:"events"`
}

// eventsSource abstracts fetching maintenance events from either the real API or a local file.
type eventsSource interface {
	fetchEvents(ctx context.Context) ([]lambdaapi.Event, error)
}

// apiSource fetches events from the real Lambda maintenance API.
type apiSource struct {
	client *lambdaapi.Client
}

func (s *apiSource) fetchEvents(ctx context.Context) ([]lambdaapi.Event, error) {
	return s.client.ListMaintenanceEvents(ctx)
}

// fileSource fetches events from a local JSON file (dev/test only).
type fileSource struct {
	path string
}

func (s *fileSource) fetchEvents(_ context.Context) ([]lambdaapi.Event, error) {
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
	cfg          config.LambdaConfig
	clusterName  string
	nodeInformer *NodeInformer
	normalizer   eventpkg.Normalizer
	source       eventsSource
	// store is retained for future checkpoint / dedup use. Typed as
	// datastore.Store so wiring errors are caught by the compiler.
	store datastore.Store
}

// NewClient constructs a Lambda Client and starts the node informer.
// If cfg.MockEventsFilePath is set, a file-based source is used (dev/test).
// Otherwise, the real Lambda API is used with the LAMBDA_API_KEY env var.
func NewClient(
	ctx context.Context,
	cfg config.LambdaConfig,
	clusterName string,
	kubeconfigPath string,
	store datastore.Store,
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

	normalizer, err := eventpkg.GetNormalizer(model.CSPLambda)
	if err != nil {
		return nil, fmt.Errorf("failed to get Lambda normalizer: %w", err)
	}

	var source eventsSource

	if cfg.MockEventsFilePath != "" {
		slog.Info("Lambda client: using mock events file (dev/test mode)", "path", cfg.MockEventsFilePath)
		source = &fileSource{path: cfg.MockEventsFilePath}
	} else {
		slog.Info("Lambda client: using real API", "endpoint", cfg.APIEndpoint, "workspaceID", cfg.WorkspaceID)
		source = &apiSource{
			client: lambdaapi.NewClient(cfg.APIEndpoint, lambdaapi.WithWorkspaceID(cfg.WorkspaceID)),
		}
	}

	return &Client{
		cfg:          cfg,
		clusterName:  clusterName,
		nodeInformer: nodeInformer,
		normalizer:   normalizer,
		source:       source,
		store:        store,
	}, nil
}

// GetName returns the CSP identifier.
func (c *Client) GetName() model.CSP {
	return model.CSPLambda
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
		resolved := c.resolveLRNs(raw)
		if len(resolved) == 0 {
			slog.Warn("Lambda: skipping event, no LRNs resolved to node names",
				"eventID", raw.ID,
				"entityLRNs", raw.EntityLRNs)

			continue
		}

		for _, r := range resolved {
			// Suffix the internal event ID with the instance UUID so a single Lambda
			// event covering multiple instances upserts one MaintenanceEvent per node
			// (matches how fault-quarantine tracks entities per-instance).
			internalID := raw.ID + "-" + r.uuid

			meta := eventpkg.LambdaEventMetadata{
				ID:                internalID,
				Detail:            raw.Detail,
				Urgency:           raw.Urgency,
				Status:            raw.Status,
				NotBefore:         raw.NotBefore,
				NotBeforeDeadline: raw.NotBeforeDeadline,
				NotAfter:          raw.NotAfter,
				LastUpdated:       raw.LastUpdated,
				NodeName:          r.nodeName,
				ClusterName:       c.clusterName,
			}

			normalized, err := c.normalizer.Normalize(nil, meta)
			if err != nil {
				slog.Error("Lambda: failed to normalize event",
					"rawEventID", raw.ID, "internalEventID", internalID, "error", err)

				continue
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case eventChan <- *normalized:
				slog.Debug("Lambda: emitted event",
					"eventID", normalized.EventID, "node", normalized.NodeName, "status", normalized.Status)
			}
		}
	}

	return nil
}

// resolvedLRN is a single (uuid, nodeName) pair extracted from a Lambda event's
// entity_lrns list after informer lookup.
type resolvedLRN struct {
	uuid     string
	nodeName string
}

// resolveLRNs walks event.EntityLRNs and returns one (uuid, nodeName) entry for
// each LRN that parses as an instance and maps to a known node. LRNs that fail
// to parse or aren't in the informer's map are logged and skipped, so that an
// unresolvable LRN at position 0 doesn't cause the entire event (which may
// affect multiple instances) to be dropped.
func (c *Client) resolveLRNs(event lambdaapi.Event) []resolvedLRN {
	if len(event.EntityLRNs) == 0 {
		return nil
	}

	var resolved []resolvedLRN

	for _, lrn := range event.EntityLRNs {
		uuid := extractUUIDFromLRN(lrn)
		if uuid == "" {
			slog.Warn("Lambda: could not parse instance UUID from LRN",
				"eventID", event.ID, "lrn", lrn)

			continue
		}

		nodeName, ok := c.nodeInformer.GetNodeName(uuid)
		if !ok {
			slog.Warn("Lambda: instance UUID not found in node informer",
				"eventID", event.ID, "uuid", uuid)

			continue
		}

		resolved = append(resolved, resolvedLRN{uuid: uuid, nodeName: nodeName})
	}

	return resolved
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
	var (
		k8sCfg *rest.Config
		err    error
	)

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
