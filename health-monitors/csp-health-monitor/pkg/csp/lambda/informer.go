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
	"fmt"
	"log/slog"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const lambdaProviderIDPrefix = "lambda://"

// NodeInformer watches Kubernetes nodes and maintains an up-to-date mapping
// of Lambda instance UUIDs to node names, keyed on the UUID extracted from
// node.Spec.ProviderID (format: lambda://<uuid>).
type NodeInformer struct {
	k8sClient          kubernetes.Interface
	informer           cache.SharedIndexInformer
	stopCh             chan struct{}
	instanceToNodeName map[string]string
	mu                 sync.RWMutex
	stopOnce           sync.Once
}

func NewNodeInformer(k8sClient kubernetes.Interface) (*NodeInformer, error) {
	ni := &NodeInformer{
		k8sClient:          k8sClient,
		stopCh:             make(chan struct{}),
		instanceToNodeName: make(map[string]string),
	}

	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	informer := factory.Core().V1().Nodes().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			ni.handleNodeAdd(node)
		},
		DeleteFunc: func(obj interface{}) {
			// client-go can deliver a DeletedFinalStateUnknown tombstone when
			// the informer misses a delete event (e.g. after a watch/relist
			// disruption). Unwrap it before the type assertion, otherwise a
			// missed delete panics the entire process.
			node, ok := obj.(*v1.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					slog.Error("Lambda node informer: unexpected delete object type",
						"type", fmt.Sprintf("%T", obj))

					return
				}

				node, ok = tombstone.Obj.(*v1.Node)
				if !ok {
					slog.Error("Lambda node informer: tombstone contained non-Node object",
						"type", fmt.Sprintf("%T", tombstone.Obj))

					return
				}
			}

			ni.handleNodeDelete(node)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handlers to node informer: %w", err)
	}

	ni.informer = informer

	return ni, nil
}

func (ni *NodeInformer) Start(ctx context.Context) {
	slog.Info("Starting Lambda node informer")

	// Wire ctx cancellation to Stop() *before* the blocking cache sync wait so
	// that a cancelled ctx during startup (e.g. app shutdown, unreachable API
	// server) closes stopCh promptly. WaitForCacheSync returns as soon as
	// stopCh is closed; otherwise it would block indefinitely.
	go func() {
		<-ctx.Done()
		ni.Stop()
	}()

	go ni.informer.Run(ni.stopCh)

	if !cache.WaitForCacheSync(ni.stopCh, ni.informer.HasSynced) {
		slog.Error("Failed to sync Lambda node informer cache")
		ni.Stop()

		return
	}

	ni.mu.RLock()
	slog.Info("Lambda node informer cache synced successfully", "instanceToNodeMap", ni.instanceToNodeName)
	ni.mu.RUnlock()
}

func (ni *NodeInformer) Stop() {
	ni.stopOnce.Do(func() {
		slog.Info("Stopping Lambda node informer")
		close(ni.stopCh)
	})
}

// GetNodeName returns the Kubernetes node name for a given Lambda instance UUID.
func (ni *NodeInformer) GetNodeName(instanceUUID string) (string, bool) {
	ni.mu.RLock()
	defer ni.mu.RUnlock()

	nodeName, ok := ni.instanceToNodeName[instanceUUID]

	return nodeName, ok
}

func (ni *NodeInformer) handleNodeAdd(node *v1.Node) {
	uuid := extractInstanceUUID(node)
	if uuid == "" {
		return
	}

	ni.mu.Lock()
	ni.instanceToNodeName[uuid] = node.Name
	ni.mu.Unlock()

	slog.Info("Node added to Lambda instance map", "node", node.Name, "instanceUUID", uuid)
}

func (ni *NodeInformer) handleNodeDelete(node *v1.Node) {
	uuid := extractInstanceUUID(node)
	if uuid == "" {
		return
	}

	ni.mu.Lock()
	delete(ni.instanceToNodeName, uuid)
	ni.mu.Unlock()

	slog.Info("Node removed from Lambda instance map", "node", node.Name, "instanceUUID", uuid)
}

// extractInstanceUUID parses the Lambda providerID format "lambda://<uuid>"
// and returns the UUID portion. Returns "" if the node has no providerID or
// the prefix does not match.
func extractInstanceUUID(node *v1.Node) string {
	if !strings.HasPrefix(node.Spec.ProviderID, lambdaProviderIDPrefix) {
		return ""
	}

	uuid := strings.TrimPrefix(node.Spec.ProviderID, lambdaProviderIDPrefix)
	if uuid == "" {
		slog.Debug("Empty UUID in Lambda providerID", "node", node.Name)
		return ""
	}

	return uuid
}
