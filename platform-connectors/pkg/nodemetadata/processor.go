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

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// processor implements the Processor interface using LRU cache with coordinated fetching.
type processor struct {
	config    *Config
	clientset kubernetes.Interface
	cache     *Cache
	stopCh    chan struct{}
}

// NewProcessor creates a new node metadata processor.
func NewProcessor(ctx context.Context, config *Config) (Processor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	restConfig.QPS = config.QPS
	restConfig.Burst = config.Burst
	restConfig.Timeout = config.APITimeout

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	p := &processor{
		config:    config,
		clientset: clientset,
		stopCh:    make(chan struct{}),
	}

	// Create cache with fetch function
	p.cache = NewCache(config.CacheSize, config.CacheTTL, p.fetchNodeMetadata)

	slog.Info("Node metadata processor initialized",
		"cacheSize", config.CacheSize,
		"cacheTTL", config.CacheTTL,
		"apiTimeout", config.APITimeout,
		"allowedLabels", config.AllowedLabels)

	return p, nil
}

// AugmentHealthEvent enriches a health event with node metadata.
func (p *processor) AugmentHealthEvent(ctx context.Context, event *pb.HealthEvent) error {
	if event.NodeName == "" {
		return fmt.Errorf("event has empty node name")
	}

	metadata, err := p.cache.GetOrFetch(ctx, event.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get metadata for node %s: %w", event.NodeName, err)
	}

	if event.Metadata == nil {
		event.Metadata = make(map[string]string)
	}

	if metadata.ProviderID != "" {
		if p.config.DecodeProviderID {
			decoded := DecodeProviderID(metadata.ProviderID)
			for k, v := range decoded {
				event.Metadata[k] = v
			}
		} else {
			event.Metadata["node.providerID"] = metadata.ProviderID
		}
	}

	for _, labelKey := range p.config.AllowedLabels {
		if labelValue, exists := metadata.Labels[labelKey]; exists {
			event.Metadata[fmt.Sprintf("node.label.%s", labelKey)] = labelValue
		}
	}

	return nil
}

// fetchNodeMetadata retrieves node metadata from Kubernetes API.
func (p *processor) fetchNodeMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, p.config.APITimeout)
	defer cancel()

	node, err := p.clientset.CoreV1().Nodes().Get(ctxWithTimeout, nodeName, metav1.GetOptions{})
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

// Start initializes background tasks.
// The LRU library handles TTL expiration automatically.
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

// Stop gracefully shuts down the processor.
func (p *processor) Stop() {
	close(p.stopCh)
}
