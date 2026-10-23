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

// Package akamai implements the csp.Monitor for Akamai Cloud (Linode). It polls
// the account maintenance API and emits a normalized MaintenanceEvent for each
// event that affects a node of this cluster.
package akamai

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	akamaiapi "github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// maintenanceAPI is the subset of the shared Akamai client this package needs.
type maintenanceAPI interface {
	ListMaintenance(ctx context.Context) ([]akamaiapi.Maintenance, error)
}

// nodeResolver maps Linode instance IDs to the names of the nodes they back.
type nodeResolver interface {
	InstanceNodeNames(ctx context.Context) (map[int]string, error)
}

// Client implements csp.Monitor for Akamai Cloud.
type Client struct {
	cfg         config.AkamaiConfig
	clusterName string
	api         maintenanceAPI
	nodes       nodeResolver
	normalizer  eventpkg.Normalizer
}

// newAPI checks the token and the endpoint, then builds the API client. It runs
// before anything else in NewClient, so a missing token or an endpoint the
// credential may not be sent to fails at startup, before a client that holds
// the credential exists.
func newAPI(cfg config.AkamaiConfig) (*akamaiapi.Client, string, error) {
	if os.Getenv(akamaiapi.APITokenEnvVar) == "" {
		return nil, "", fmt.Errorf("no Akamai API token: set %s", akamaiapi.APITokenEnvVar)
	}

	endpoint, err := akamaiapi.NormalizeEndpoint("akamai.apiEndpoint", cfg.APIEndpoint)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve Akamai API endpoint: %w", err)
	}

	return akamaiapi.NewClient(endpoint), endpoint, nil
}

// NewClient constructs an Akamai Client. The API
// token is read from the LINODE_TOKEN environment variable.
func NewClient(
	ctx context.Context,
	cfg config.AkamaiConfig,
	clusterName string,
	kubeconfigPath string,
) (*Client, error) {
	api, endpoint, err := newAPI(cfg)
	if err != nil {
		return nil, err
	}

	k8sClient, err := buildK8sClient(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes client: %w", err)
	}

	normalizer, err := eventpkg.GetNormalizer(model.CSPAkamai)
	if err != nil {
		return nil, fmt.Errorf("failed to get Akamai normalizer: %w", err)
	}

	slog.Info("Akamai client: using real API", "endpoint", endpoint)

	return &Client{
		cfg:         cfg,
		clusterName: clusterName,
		api:         api,
		nodes:       NewNodeResolver(k8sClient),
		normalizer:  normalizer,
	}, nil
}

// GetName returns the CSP identifier.
func (c *Client) GetName() model.CSP {
	return model.CSPAkamai
}

// StartMonitoring polls for maintenance events on each tick and emits normalized
// MaintenanceEvents onto eventChan.
func (c *Client) StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	ticker := time.NewTicker(time.Duration(c.cfg.PollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	if err := c.pollEvents(ctx, eventChan); err != nil {
		slog.Error("Akamai: initial poll error", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.pollEvents(ctx, eventChan); err != nil {
				slog.Error("Akamai: poll error", "error", err)
			}
		}
	}
}

func (c *Client) pollEvents(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	events, err := c.api.ListMaintenance(ctx)
	if err != nil {
		return err
	}

	slog.Debug("Akamai: fetched events", "count", len(events))

	if len(events) == 0 {
		return nil
	}

	// One List serves every event in this poll.
	instanceNodes, err := c.nodes.InstanceNodeNames(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve nodes: %w", err)
	}

	for _, raw := range events {
		// The account lists maintenance for every instance in it, most of which
		// are not nodes of this cluster. That is the normal case, not a warning.
		nodeName, ok := resolveNode(raw, instanceNodes)
		if !ok {
			slog.Debug("Akamai: skipping event, entity is not a node of this cluster",
				"eventID", raw.ID, "entityType", raw.Entity.Type, "entityID", raw.Entity.ID)

			continue
		}

		normalized, err := c.normalizer.Normalize(raw, eventpkg.AkamaiEventContext{
			NodeName:    nodeName,
			ClusterName: c.clusterName,
		})
		if err != nil {
			slog.Error("Akamai: failed to normalize event", "eventID", raw.ID, "error", err)

			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- *normalized:
			slog.Debug("Akamai: emitted event",
				"eventID", normalized.EventID, "node", normalized.NodeName, "status", normalized.Status)
		}
	}

	return nil
}

// resolveNode returns the name of the node the event applies to. It reports
// false for an entity that is not a Linode instance or not a node of this
// cluster.
func resolveNode(raw akamaiapi.Maintenance, instanceNodes map[int]string) (string, bool) {
	if raw.Entity.Type != akamaiapi.MaintenanceEntityLinode {
		return "", false
	}

	nodeName, ok := instanceNodes[raw.Entity.ID]

	return nodeName, ok
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
