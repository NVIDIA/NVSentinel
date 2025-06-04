// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
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

func (n *NodeInfo) GetQuarantinedNodesMap() *map[string]bool {
	return &n.quarantinedNodesMap
}

func (n *NodeInfo) BuildQuarantinedNodesMap(k8sClient kubernetes.Interface) error {
	nodes, err := k8sClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	n.mutex.Lock()
	defer n.mutex.Unlock()

	for _, node := range nodes.Items {
		if node.Annotations[common.QuarantineHealthEventAnnotationKey] == "true" {
			n.quarantinedNodesMap[node.Name] = true
		}
	}

	return nil
}

func (n *NodeInfo) MarkNodeQuarantineStatusCache(nodeName string, isQuarantined bool) {
	n.mutex.Lock()

	if isQuarantined {
		n.quarantinedNodesMap[nodeName] = true
	} else {
		delete(n.quarantinedNodesMap, nodeName)
	}
	n.mutex.Unlock()
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
