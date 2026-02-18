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

package informer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

const (
	quarantineAnnotationIndexName = "quarantineAnnotation"
)

// NodeInformer watches specific nodes and provides counts.
type NodeInformer struct {
	clientset      kubernetes.Interface
	informer       cache.SharedIndexInformer
	lister         corelisters.NodeLister
	informerSynced cache.InformerSynced

	// onQuarantinedNodeDeleted is called when a quarantined node with annotations is deleted
	onQuarantinedNodeDeleted func(nodeName string)

	// onManualUncordon is called when a node is manually uncordoned while having FQ annotations
	onManualUncordon func(nodeName string) error

	// onManualUntaint is called when a node is manually untainted while having FQ annotations
	onManualUntaint func(nodeName string) error
}

// Lister returns the informer's node lister.
func (ni *NodeInformer) Lister() corelisters.NodeLister {
	return ni.lister
}

// GetInformer returns the underlying SharedIndexInformer.
func (ni *NodeInformer) GetInformer() cache.SharedIndexInformer {
	return ni.informer
}

// NewNodeInformer creates a new NodeInformer that watches all nodes.
func NewNodeInformer(clientset kubernetes.Interface,
	resyncPeriod time.Duration) (*NodeInformer, error) {
	ni := &NodeInformer{
		clientset: clientset,
	}

	informerFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)

	nodeInformerObj := informerFactory.Core().V1().Nodes()
	ni.informer = nodeInformerObj.Informer()
	ni.lister = nodeInformerObj.Lister()
	ni.informerSynced = nodeInformerObj.Informer().HasSynced

	err := ni.informer.AddIndexers(cache.Indexers{
		quarantineAnnotationIndexName: quarantineAnnotationIndexFunc,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add quarantine annotation indexer: %w", err)
	}

	_, err = ni.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ni.handleAddNode,
		UpdateFunc: ni.handleUpdateNodeWrapper,
		DeleteFunc: ni.handleDeleteNode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler: %w", err)
	}

	slog.Info("NodeInformer created, watching all nodes")

	return ni, nil
}

// Run starts the informer and waits for cache sync.
func (ni *NodeInformer) Run(stopCh <-chan struct{}) error {
	slog.Info("Starting NodeInformer")

	go ni.informer.Run(stopCh)

	slog.Info("Waiting for NodeInformer cache to sync...")

	if ok := cache.WaitForCacheSync(stopCh, ni.informerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	slog.Info("NodeInformer cache synced")

	return nil
}

// HasSynced checks if the informer's cache has been synchronized.
func (ni *NodeInformer) HasSynced() bool {
	return ni.informerSynced()
}

// WaitForSync waits for the informer cache to sync with context cancellation support.
func (ni *NodeInformer) WaitForSync(ctx context.Context) bool {
	slog.Info("Waiting for NodeInformer cache to sync...")

	if ok := cache.WaitForCacheSync(ctx.Done(), ni.informerSynced); !ok {
		slog.Warn("NodeInformer cache sync failed or context cancelled")
		return false
	}

	slog.Info("NodeInformer cache synced")

	return true
}

// quarantineAnnotationIndexFunc is the indexer function for quarantined nodes
func quarantineAnnotationIndexFunc(obj interface{}) ([]string, error) {
	node, ok := obj.(*v1.Node)
	if !ok {
		return nil, fmt.Errorf("expected node object, got %T", obj)
	}

	if _, exists := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		return []string{"quarantined"}, nil
	}

	return []string{}, nil
}

// GetNodeCounts returns the current counts of total nodes and quarantined nodes.
func (ni *NodeInformer) GetNodeCounts() (totalNodes int, quarantinedNodesMap map[string]bool, err error) {
	if !ni.HasSynced() {
		return 0, nil, fmt.Errorf("node informer cache not synced yet")
	}

	allObjs := ni.informer.GetIndexer().List()
	total := len(allObjs)

	quarantinedObjs, err := ni.informer.GetIndexer().ByIndex(quarantineAnnotationIndexName, "quarantined")
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get quarantined nodes from index: %w", err)
	}

	quarantinedMap := make(map[string]bool, len(quarantinedObjs))

	for _, obj := range quarantinedObjs {
		if node, ok := obj.(*v1.Node); ok {
			quarantinedMap[node.Name] = true
		}
	}

	return total, quarantinedMap, nil
}

// GetNode retrieves a node from the informer's cache.
func (ni *NodeInformer) GetNode(name string) (*v1.Node, error) {
	return ni.lister.Get(name)
}

// ListNodes lists all nodes from the informer's cache.
func (ni *NodeInformer) ListNodes() ([]*v1.Node, error) {
	return ni.lister.List(labels.Everything())
}

// handleAddNode logs when a node is added and checks for stale state.
// This is important during informer restart/resync when we get ADD events
// for all existing nodes. If a node was manually uncordoned/untainted while
// FQ was down, we need to detect and handle it.
func (ni *NodeInformer) handleAddNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		slog.Error("Add event received unexpected type",
			"expected", "*v1.Node",
			"actualType", fmt.Sprintf("%T", obj))

		return
	}

	slog.Debug("Node added", "node", node.Name)

	// Check for stale state: node has FQ annotations but state doesn't match
	// This can happen if FQ crashed and node was manually uncordoned/untainted
	ni.checkStaleStateOnAdd(node)
}

// checkStaleStateOnAdd checks if a node added during initial sync has stale FQ state.
// This handles the case where FQ crashed, node was manually uncordoned/untainted,
// and FQ restarted (getting ADD events instead of UPDATE events).
func (ni *NodeInformer) checkStaleStateOnAdd(node *v1.Node) {
	// Check for stale uncordon state: node has FQ cordon annotation but is not cordoned
	if ni.checkStaleUncordonState(node) {
		return
	}

	// Check for stale untaint state: node has FQ taint annotation but expected taints are missing
	ni.checkStaleUntaintState(node)
}

// checkStaleUncordonState checks if node has stale uncordon state and handles it.
// Returns true if stale uncordon was detected (and handled), false otherwise.
func (ni *NodeInformer) checkStaleUncordonState(node *v1.Node) bool {
	_, hasCordonAnnotation := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if !hasCordonAnnotation || node.Spec.Unschedulable {
		return false
	}

	slog.Info("Detected stale uncordon state on node add (node has FQ annotation but is not cordoned)",
		"node", node.Name)

	if err := ni.onManualUncordon(node.Name); err != nil {
		slog.Error("Failed to handle stale uncordon state on node add", "node", node.Name, "error", err)
	} else {
		slog.Debug("Successfully handled stale uncordon state on node add", "node", node.Name)
	}

	return true
}

// checkStaleUntaintState checks if node has stale untaint state and handles it.
func (ni *NodeInformer) checkStaleUntaintState(node *v1.Node) {
	taintsAnnotation, hasTaintsAnnotation := node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
	if !hasTaintsAnnotation || taintsAnnotation == "" {
		return
	}

	// Parse expected taints from annotation
	var expectedTaints []config.Taint
	if err := json.Unmarshal([]byte(taintsAnnotation), &expectedTaints); err != nil {
		slog.Debug("Failed to unmarshal taints annotation during stale state check on add",
			"node", node.Name, "error", err)

		return
	}

	// Check if any expected taints are missing from the node
	if !ni.hasMissingTaints(node, expectedTaints) {
		return
	}

	slog.Info("Detected stale untaint state on node add (node has FQ taint annotation but taints are missing)",
		"node", node.Name)

	if err := ni.onManualUntaint(node.Name); err != nil {
		slog.Error("Failed to handle stale untaint state on node add", "node", node.Name, "error", err)
	} else {
		slog.Debug("Successfully handled stale untaint state on node add", "node", node.Name)
	}
}

// hasMissingTaints checks if any expected taints are missing from the node.
func (ni *NodeInformer) hasMissingTaints(node *v1.Node, expectedTaints []config.Taint) bool {
	for _, expectedTaint := range expectedTaints {
		if !hasTaint(node, expectedTaint) {
			slog.Debug("FQ taint is missing from node (stale state detected on add)",
				"node", node.Name,
				"missingTaint", fmt.Sprintf("%s=%s:%s", expectedTaint.Key, expectedTaint.Value, expectedTaint.Effect))

			return true
		}
	}

	return false
}

// handleUpdateNodeWrapper is a wrapper for handleUpdateNode that converts interface{} to *v1.Node.
func (ni *NodeInformer) handleUpdateNodeWrapper(oldObj, newObj interface{}) {
	oldNode, okOld := oldObj.(*v1.Node)
	newNode, okNew := newObj.(*v1.Node)

	if !okOld || !okNew {
		slog.Error("Update event: expected Node objects",
			"oldType", fmt.Sprintf("%T", oldObj), "newType", fmt.Sprintf("%T", newObj))

		return
	}

	ni.handleUpdateNode(oldNode, newNode)
}

// detectAndHandleManualUncordon checks if a node was manually uncordoned and handles it
func (ni *NodeInformer) detectAndHandleManualUncordon(oldNode, newNode *v1.Node) bool {
	// Check if node transitioned from unschedulable to schedulable
	if !oldNode.Spec.Unschedulable || newNode.Spec.Unschedulable {
		return false
	}

	slog.Debug("Node transitioned from cordoned to uncordoned", "node", newNode.Name)

	// Check if node has FQ quarantine annotations
	_, hasCordonAnnotation := newNode.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if !hasCordonAnnotation {
		slog.Debug("Node was uncordoned but has no FQ quarantine annotation", "node", newNode.Name)

		return false
	}

	slog.Info("Detected manual uncordon of FQ-quarantined node", "node", newNode.Name)

	if ni.onManualUncordon != nil {
		slog.Debug("Invoking manual uncordon callback", "node", newNode.Name)

		if err := ni.onManualUncordon(newNode.Name); err != nil {
			slog.Error("Manual uncordon callback failed", "node", newNode.Name, "error", err)
		} else {
			slog.Debug("Manual uncordon callback completed successfully", "node", newNode.Name)
		}
	} else {
		slog.Warn("Manual uncordon callback is NOT REGISTERED - manual uncordon will NOT be handled!", "node", newNode.Name)
	}

	return true
}

// detectAndHandleManualUntaint checks if a node was manually untainted and handles it
func (ni *NodeInformer) detectAndHandleManualUntaint(oldNode, newNode *v1.Node) bool {
	// Check if node has FQ taint annotation
	taintsAnnotation, hasTaintsAnnotation := newNode.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
	if !hasTaintsAnnotation || taintsAnnotation == "" {
		return false
	}

	slog.Debug("Node has FQ taints annotation", "node", newNode.Name)

	// Parse expected taints from annotation
	var expectedTaints []config.Taint
	if err := json.Unmarshal([]byte(taintsAnnotation), &expectedTaints); err != nil {
		slog.Error("Failed to unmarshal taints annotation during manual untaint detection",
			"node", newNode.Name, "error", err)

		return false
	}

	// Check if any expected FQ taint was REMOVED in this update
	// Must have been present in oldNode and now missing in newNode
	taintWasRemoved := false

	for _, expectedTaint := range expectedTaints {
		wasPresentBefore := hasTaint(oldNode, expectedTaint)
		isPresentNow := hasTaint(newNode, expectedTaint)

		if wasPresentBefore && !isPresentNow {
			taintWasRemoved = true

			slog.Debug("FQ taint was removed from node",
				"node", newNode.Name,
				"removedTaint", fmt.Sprintf("%s=%s:%s", expectedTaint.Key, expectedTaint.Value, expectedTaint.Effect))

			break
		}
	}

	// If no taint was actually removed in this update, don't trigger
	if !taintWasRemoved {
		return false
	}

	slog.Info("Detected manual untaint of FQ-quarantined node", "node", newNode.Name)

	if err := ni.onManualUntaint(newNode.Name); err != nil {
		slog.Error("Manual untaint callback failed", "node", newNode.Name, "error", err)
	}

	return true
}

// handleUpdateNode detects and handles manual uncordon and manual untaint of quarantined nodes.
func (ni *NodeInformer) handleUpdateNode(oldNode, newNode *v1.Node) {
	// Check manual uncordon first - if it triggers, it does full cleanup including taints
	ni.detectAndHandleManualUncordon(oldNode, newNode)
	ni.detectAndHandleManualUntaint(oldNode, newNode)
}

// SetOnQuarantinedNodeDeletedCallback sets the callback function for when a quarantined node is deleted
func (ni *NodeInformer) SetOnQuarantinedNodeDeletedCallback(callback func(nodeName string)) {
	ni.onQuarantinedNodeDeleted = callback
}

// SetOnManualUncordonCallback sets the callback function for when a node is manually uncordoned
func (ni *NodeInformer) SetOnManualUncordonCallback(callback func(nodeName string) error) {
	ni.onManualUncordon = callback
}

// SetOnManualUntaintCallback sets the callback function for when a node is manually untainted
func (ni *NodeInformer) SetOnManualUntaintCallback(callback func(nodeName string) error) {
	ni.onManualUntaint = callback
}

// handleDeleteNode handles node deletion events.
func (ni *NodeInformer) handleDeleteNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			slog.Error("Delete event received unexpected type",
				"expected", "*v1.Node or DeletedFinalStateUnknown",
				"actualType", fmt.Sprintf("%T", obj))

			return
		}

		node, ok = tombstone.Obj.(*v1.Node)
		if !ok {
			slog.Error("Delete event tombstone contained unexpected type",
				"expected", "*v1.Node",
				"actualType", fmt.Sprintf("%T", tombstone.Obj))

			return
		}
	}

	slog.Info("Node deleted", "node", node.Name)

	_, hadQuarantineAnnotation := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]

	// If the node was quarantined and had the annotation, call the callback so that
	// currentQuarantinedNodes metric is decremented
	if hadQuarantineAnnotation && ni.onQuarantinedNodeDeleted != nil {
		ni.onQuarantinedNodeDeleted(node.Name)
	}
}

// hasTaint checks if a node has a specific taint
func hasTaint(node *v1.Node, expectedTaint config.Taint) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == expectedTaint.Key &&
			taint.Value == expectedTaint.Value &&
			string(taint.Effect) == expectedTaint.Effect {
			return true
		}
	}

	return false
}
