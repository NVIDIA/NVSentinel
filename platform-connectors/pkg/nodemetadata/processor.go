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

package nodemetadata

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hashicorp/golang-lru/v2/expirable"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type processor struct {
	config    *Config
	clientset kubernetes.Interface
	cache     *expirable.LRU[string, *NodeMetadata]
	fetchMu   sync.Mutex
	stopCh    chan struct{}
}

// NewProcessor creates a new node metadata processor using an existing Kubernetes client.
func NewProcessor(ctx context.Context, config *Config, clientset kubernetes.Interface) (Processor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if clientset == nil {
		return nil, fmt.Errorf("clientset cannot be nil")
	}

	cache := expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	p := &processor{
		config:    config,
		clientset: clientset,
		cache:     cache,
		stopCh:    make(chan struct{}),
	}

	slog.Info("Node metadata processor initialized",
		"cacheSize", config.CacheSize,
		"cacheTTL", config.CacheTTL,
		"allowedLabels", config.AllowedLabels)

	return p, nil
}

func (p *processor) AugmentHealthEvent(ctx context.Context, event *pb.HealthEvent) error {
	if event.NodeName == "" {
		return fmt.Errorf("event has empty node name")
	}

	metadata, err := p.getOrFetchMetadata(ctx, event.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get metadata for node %s: %w", event.NodeName, err)
	}

	if event.Metadata == nil {
		event.Metadata = make(map[string]string)
	}

	if metadata.ProviderID != "" {
		event.Metadata["node.providerID"] = metadata.ProviderID
	}

	for _, labelKey := range p.config.AllowedLabels {
		if labelValue, exists := metadata.Labels[labelKey]; exists {
			event.Metadata[labelKey] = labelValue
		}
	}

	return nil
}

// getOrFetchMetadata retrieves metadata from cache or fetches it if not present.
// Uses double-check pattern to prevent duplicate fetches.
func (p *processor) getOrFetchMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	if metadata, found := p.cache.Get(nodeName); found {
		return metadata, nil
	}

	p.fetchMu.Lock()
	defer p.fetchMu.Unlock()

	// Double-check: another goroutine might have fetched while we waited
	if metadata, found := p.cache.Get(nodeName); found {
		return metadata, nil
	}

	metadata, err := p.fetchNodeMetadata(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	p.cache.Add(nodeName, metadata)
	return metadata, nil
}

func (p *processor) fetchNodeMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	node, err := p.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node from API: %w", err)
	}

	metadata := &NodeMetadata{
		ProviderID: node.Spec.ProviderID,
		Labels:     make(map[string]string),
	}

	if len(p.config.AllowedLabels) > 0 {
		for _, labelKey := range p.config.AllowedLabels {
			if labelValue, exists := node.Labels[labelKey]; exists {
				metadata.Labels[labelKey] = labelValue
			}
		}
	}

	return metadata, nil
}

// LRU library handles TTL expiration automatically.
func (p *processor) Start(ctx context.Context) {
	slog.Info("Node metadata processor started (cache handles TTL automatically)")

	select {
	case <-p.stopCh:
		slog.Info("Stopping node metadata processor")
		return
	case <-ctx.Done():
		slog.Info("Context cancelled, stopping node metadata processor")
		return
	}
}

func (p *processor) Stop() {
	close(p.stopCh)
}
