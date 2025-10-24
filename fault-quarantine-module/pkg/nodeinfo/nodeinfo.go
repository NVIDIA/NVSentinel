// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodeinfo

import (
	"context"
	"sync"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type NodeInfo struct {
	quarantinedNodesMap map[string]bool
	mutex               sync.RWMutex
	workSignal          chan struct{}
}

func NewNodeInfo(workSignal chan struct{}) *NodeInfo {
	return &NodeInfo{
		quarantinedNodesMap: make(map[string]bool),
		workSignal:          workSignal,
	}
}

// GetQuarantinedNodesCount returns the number of quarantined nodes in a thread-safe manner
func (n *NodeInfo) GetQuarantinedNodesCount() int {
	n.mutex.RLock()
	defer n.mutex.RUnlock()

	return len(n.quarantinedNodesMap)
}

// GetQuarantinedNodesCopy returns a copy of the quarantined nodes map in a thread-safe manner
func (n *NodeInfo) GetQuarantinedNodesCopy() map[string]bool {
	n.mutex.RLock()
	defer n.mutex.RUnlock()

	copy := make(map[string]bool)
	for k, v := range n.quarantinedNodesMap {
		copy[k] = v
	}

	return copy
}

func (n *NodeInfo) BuildQuarantinedNodesMap(k8sClient kubernetes.Interface) error {
	nodes, err := k8sClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	n.mutex.Lock()
	defer n.mutex.Unlock()

	for _, node := range nodes.Items {
		key := common.QuarantineHealthEventIsCordonedAnnotationKey
		if node.Annotations[key] == common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
			n.quarantinedNodesMap[node.Name] = true
		}
	}

	return nil
}

func (n *NodeInfo) MarkNodeQuarantineStatusCache(nodeName string, isQuarantined bool, annotationExist bool) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if isQuarantined {
		n.quarantinedNodesMap[nodeName] = true
	} else if !annotationExist {
		// this is a case where a node is uncordoned  manually, but annotation is still present.
		// So, we should not remove it from the cache.
		delete(n.quarantinedNodesMap, nodeName)
	}

	klog.V(3).Infof("Quarantined nodes map: %+v, Total length: %d", n.quarantinedNodesMap, len(n.quarantinedNodesMap))

	n.signalWork()
}

func (n *NodeInfo) GetNodeQuarantineStatusCache(nodeName string) bool {
	n.mutex.RLock()
	defer n.mutex.RUnlock()

	if _, exists := n.quarantinedNodesMap[nodeName]; !exists {
		return false
	}

	return true
}

// signalWork sends a non-blocking signal to the reconciler's work channel.
func (n *NodeInfo) signalWork() {
	if n.workSignal == nil {
		klog.Errorf("No channel configured for node informer")
		return // No channel configured
	}
	select {
	case n.workSignal <- struct{}{}:
		klog.V(3).Infof("Signalled work channel due to node change.")
	default:
		klog.V(3).Infof("Work channel already signalled, skipping signal for node change.")
	}
}
