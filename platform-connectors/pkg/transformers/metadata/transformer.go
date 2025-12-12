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

package metadata

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

type NodeMetadata struct {
	ProviderID string
	Labels     map[string]string
}

type Augmentor struct {
	config    *Config
	clientset kubernetes.Interface
	cache     *expirable.LRU[string, *NodeMetadata]
	fetchMu   sync.Mutex
}

func New(ctx context.Context, config *Config, clientset kubernetes.Interface) (*Augmentor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cache := expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	slog.Info("Metadata augmentor initialized",
		"cacheSize", config.CacheSize,
		"cacheTTL", config.CacheTTL,
		"allowedLabels", config.AllowedLabels)

	return &Augmentor{
		config:    config,
		clientset: clientset,
		cache:     cache,
	}, nil
}

func (a *Augmentor) Transform(ctx context.Context, event *pb.HealthEvent) error {
	if event.NodeName == "" {
		return fmt.Errorf("event has empty node name")
	}

	metadata, err := a.getOrFetchMetadata(ctx, event.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get metadata for node %s: %w", event.NodeName, err)
	}

	if event.Metadata == nil {
		event.Metadata = make(map[string]string)
	}

	if metadata.ProviderID != "" {
		event.Metadata["providerID"] = metadata.ProviderID
	}

	for _, labelKey := range a.config.AllowedLabels {
		if labelValue, exists := metadata.Labels[labelKey]; exists {
			event.Metadata[labelKey] = labelValue
		}
	}

	slog.Info("Metadata augmented",
		"node", event.NodeName,
		"providerID", metadata.ProviderID,
		"labelsAdded", len(metadata.Labels))

	return nil
}

func (a *Augmentor) Name() string {
	return "MetadataAugmentor"
}

func (a *Augmentor) getOrFetchMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	if metadata, found := a.cache.Get(nodeName); found {
		return metadata, nil
	}

	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()

	if metadata, found := a.cache.Get(nodeName); found {
		return metadata, nil
	}

	metadata, err := a.fetchNodeMetadata(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	a.cache.Add(nodeName, metadata)

	return metadata, nil
}

func (a *Augmentor) fetchNodeMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	node, err := a.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node from API: %w", err)
	}

	metadata := &NodeMetadata{
		ProviderID: node.Spec.ProviderID,
		Labels:     make(map[string]string),
	}

	for _, labelKey := range a.config.AllowedLabels {
		if labelValue, exists := node.Labels[labelKey]; exists {
			metadata.Labels[labelKey] = labelValue
		}
	}

	return metadata, nil
}
