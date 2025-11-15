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

package aws

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeInformer watches Kubernetes nodes and maintains an up-to-date mapping
// of AWS instance IDs to node names
type NodeInformer struct {
	k8sClient               kubernetes.Interface
	informer                cache.SharedIndexInformer
	stopCh                  chan struct{}
	nodeNameToInstanceIDMap map[string]string
	mu                      sync.RWMutex
	stopOnce                sync.Once
}

func NewNodeInformer(k8sClient kubernetes.Interface) (*NodeInformer, error) {
	ni := &NodeInformer{
		k8sClient:               k8sClient,
		stopCh:                  make(chan struct{}),
		nodeNameToInstanceIDMap: make(map[string]string),
	}

	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	informer := factory.Core().V1().Nodes().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			ni.handleNodeAdd(node)
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			ni.handleNodeDelete(node)
		},
	})
	if err != nil {
		slog.Error("Failed to add event handlers to informer", "error", err)
		return nil, fmt.Errorf("failed to add event handlers to informer: %w", err)
	}

	ni.informer = informer

	return ni, nil
}

func (ni *NodeInformer) Start(ctx context.Context) {
	slog.Info("Starting AWS node informer")

	go ni.informer.Run(ni.stopCh)

	if !cache.WaitForCacheSync(ni.stopCh, ni.informer.HasSynced) {
		slog.Error("Failed to sync node informer cache")
		ni.Stop()

		return
	}

	ni.mu.RLock()
	slog.Info("AWS node informer cache synced successfully", "nodesMap", ni.nodeNameToInstanceIDMap)
	ni.mu.RUnlock()

	go func() {
		<-ctx.Done()
		ni.Stop()
	}()
}

func (ni *NodeInformer) Stop() {
	ni.stopOnce.Do(func() {
		slog.Info("Stopping AWS node informer")
		close(ni.stopCh)
	})
}

func (ni *NodeInformer) GetInstanceIDs() map[string]string {
	ni.mu.RLock()
	defer ni.mu.RUnlock()

	// Return a copy to avoid concurrent access issues
	instanceIDsCopy := make(map[string]string, len(ni.nodeNameToInstanceIDMap))
	for k, v := range ni.nodeNameToInstanceIDMap {
		instanceIDsCopy[k] = v
	}

	return instanceIDsCopy
}

func (ni *NodeInformer) GetNodeName(instanceID string) (string, bool) {
	ni.mu.RLock()
	defer ni.mu.RUnlock()

	nodeName, ok := ni.nodeNameToInstanceIDMap[instanceID]

	return nodeName, ok
}

func (ni *NodeInformer) handleNodeAdd(node *v1.Node) {
	instanceID := extractInstanceID(node)
	if instanceID == "" {
		return
	}

	ni.mu.Lock()
	ni.nodeNameToInstanceIDMap[instanceID] = node.Name
	ni.mu.Unlock()

	slog.Info("Node added to AWS instance map",
		"node", node.Name,
		"instanceID", instanceID)
}

func (ni *NodeInformer) handleNodeDelete(node *v1.Node) {
	instanceID := extractInstanceID(node)
	if instanceID == "" {
		return
	}

	ni.mu.Lock()
	delete(ni.nodeNameToInstanceIDMap, instanceID)
	ni.mu.Unlock()

	slog.Info("Node removed from AWS instance map",
		"node", node.Name,
		"instanceID", instanceID)
}

func extractInstanceID(node *v1.Node) string {
	if node.Spec.ProviderID == "" {
		return ""
	}

	// Parse AWS provider ID format: aws:///us-east-1/i-0123456789abcdef0
	if !strings.HasPrefix(node.Spec.ProviderID, "aws:///") {
		return ""
	}

	idPart := strings.TrimPrefix(node.Spec.ProviderID, "aws:///")
	parts := strings.Split(idPart, "/")
	instanceID := parts[len(parts)-1]

	if !strings.HasPrefix(instanceID, "i-") {
		slog.Debug("Unexpected instance ID format",
			"node", node.Name,
			"instanceID", instanceID)

		return ""
	}

	return instanceID
}
