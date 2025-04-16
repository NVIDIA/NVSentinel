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

package informer

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	// GpuNodeLabel is the label used to identify nodes with GPUs relevant to NVSentinel.
	GpuNodeLabel = "nvidia.com/gpu.present"
)

// NodeInfoProvider defines the interface for getting node counts.
type NodeInfoProvider interface {
	// GetGpuNodeCounts returns the total number of nodes with the GpuNodeLabel
	// and the number of those nodes that are currently unschedulable (cordoned).
	GetGpuNodeCounts() (totalGpuNodes int, unschedulableGpuNodes int, err error)
	// HasSynced returns true if the underlying informer cache has synced.
	HasSynced() bool
}

// NodeInformer watches specific nodes and provides counts.
type NodeInformer struct {
	clientset kubernetes.Interface
	informer  cache.SharedIndexInformer
	lister    corelisters.NodeLister

	// Mutex protects access to the counts below
	mutex                 sync.RWMutex
	totalGpuNodes         int
	unschedulableGpuNodes int

	informerSynced cache.InformerSynced

	// workSignal is used to notify the reconciler about relevant node changes
	workSignal chan struct{}
}

// Lister returns the informer's node lister.
func (ni *NodeInformer) Lister() corelisters.NodeLister {
	return ni.lister
}

// NewNodeInformer creates a new NodeInformer focused on nodes with the GpuNodeLabel.
func NewNodeInformer(clientset kubernetes.Interface,
	resyncPeriod time.Duration, workSignal chan struct{}) (*NodeInformer, error) {
	// Filter nodes based on the presence of the GPU label
	gpuNodeSelector := labels.Set{GpuNodeLabel: "true"}.AsSelector()

	tweakListOptions := func(options *metav1.ListOptions) {
		options.LabelSelector = gpuNodeSelector.String()
	}

	// Create an informer factory filtered for the specific label
	informerFactory := informers.NewSharedInformerFactoryWithOptions(clientset, resyncPeriod,
		informers.WithTweakListOptions(tweakListOptions))
	nodeInformer := informerFactory.Core().V1().Nodes()

	ni := &NodeInformer{
		clientset:      clientset,
		informer:       nodeInformer.Informer(),
		lister:         nodeInformer.Lister(),
		informerSynced: nodeInformer.Informer().HasSynced,
		workSignal:     workSignal,
	}

	// Register event handlers
	_, err := ni.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ni.handleAddNode,
		UpdateFunc: ni.handleUpdateNode,
		DeleteFunc: ni.handleDeleteNode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler: %w", err)
	}

	klog.Infof("NodeInformer created, watching nodes with label %s=true", GpuNodeLabel)

	return ni, nil
}

// Run starts the informer and waits for cache sync.
func (ni *NodeInformer) Run(stopCh <-chan struct{}) error {
	klog.Info("Starting NodeInformer")

	// Start the informer goroutine
	go ni.informer.Run(stopCh)

	// Wait for the initial cache synchronization
	klog.Info("Waiting for NodeInformer cache to sync...")

	if ok := cache.WaitForCacheSync(stopCh, ni.informerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("NodeInformer cache synced")

	_, err := ni.recalculateCounts()
	if err != nil {
		// Log the error but allow the informer to continue running
		klog.Errorf("Initial count calculation failed: %v", err)
	}

	return nil
}

// HasSynced checks if the informer's cache has been synchronized.
func (ni *NodeInformer) HasSynced() bool {
	return ni.informerSynced()
}

// GetGpuNodeCounts returns the current counts of total and unschedulable GPU nodes.
func (ni *NodeInformer) GetGpuNodeCounts() (totalGpuNodes int, unschedulableGpuNodes int, err error) {
	if !ni.HasSynced() {
		return 0, 0, fmt.Errorf("node informer cache not synced yet")
	}

	ni.mutex.RLock()
	defer ni.mutex.RUnlock()

	return ni.totalGpuNodes, ni.unschedulableGpuNodes, nil
}

// handleAddNode recalculates counts when a node is added.
func (ni *NodeInformer) handleAddNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		klog.Errorf("Add event: expected Node object, got %T", obj)
		return
	}

	klog.V(4).Infof("Node added: %s", node.Name)

	ni.mutex.Lock()
	defer ni.mutex.Unlock()

	ni.totalGpuNodes++
	if node.Spec.Unschedulable {
		ni.unschedulableGpuNodes++
	}
	// Signal reconciler only if counts actually changed
	ni.signalWork()
}

// handleUpdateNode recalculates counts when a node is updated.
func (ni *NodeInformer) handleUpdateNode(oldObj, newObj interface{}) {
	oldNode, okOld := oldObj.(*v1.Node)

	newNode, okNew := newObj.(*v1.Node)
	if !okOld || !okNew {
		klog.Errorf("Update event: expected Node objects, got %T and %T", oldObj, newObj)
		return
	}

	// Recalculate only if the Unschedulable status changed (relevant for MaxPercentage rule)
	// We assume labels don't change in a way that affects filtering due to informer's label selector
	if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable {
		klog.V(4).Infof("Node updated: %s (Unschedulable: %t -> %t)", newNode.Name,
			oldNode.Spec.Unschedulable, newNode.Spec.Unschedulable)

		ni.mutex.Lock()
		// Signal reconciler after successful count update due to relevant change
		if newNode.Spec.Unschedulable {
			ni.unschedulableGpuNodes++
		} else {
			ni.unschedulableGpuNodes--
		}
		ni.mutex.Unlock() // Unlock before signalling

		ni.signalWork()
	} else {
		klog.V(5).Infof("Node update ignored (no relevant change): %s", newNode.Name)
	}
}

// handleDeleteNode recalculates counts when a node is deleted.
func (ni *NodeInformer) handleDeleteNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		// Handle deletion notifications potentially wrapped in DeletedFinalStateUnknown
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Delete event: expected Node object or DeletedFinalStateUnknown, got %T", obj)
			return
		}

		node, ok = tombstone.Obj.(*v1.Node)
		if !ok {
			klog.Errorf("Delete event: DeletedFinalStateUnknown contained non-Node object %T", tombstone.Obj)
			return
		}
	}

	klog.V(4).Infof("Node deleted: %s", node.Name)

	ni.mutex.Lock()
	defer ni.mutex.Unlock()

	if node.Spec.Unschedulable {
		ni.unschedulableGpuNodes--
	}

	ni.totalGpuNodes--

	// Signal reconciler only if counts actually changed
	ni.signalWork()
}

// recalculateCounts lists all relevant nodes from the cache and updates the counts.
// It returns true if the counts changed, false otherwise.
func (ni *NodeInformer) recalculateCounts() (bool, error) {
	// Use List with Everything selector as the lister is already filtered by the factory
	nodes, err := ni.lister.List(labels.Everything())
	if err != nil {
		return false, fmt.Errorf("failed to list nodes from informer cache: %w", err)
	}

	total := 0
	unschedulable := 0

	for _, node := range nodes {
		// Double-check the label, although the informer should only list matching nodes
		if _, exists := node.Labels[GpuNodeLabel]; exists {
			total++

			if node.Spec.Unschedulable {
				unschedulable++
			}
		} else {
			klog.Warningf("Node %s found in informer cache despite missing label %s", node.Name, GpuNodeLabel)
		}
	}

	ni.mutex.Lock()
	changed := ni.totalGpuNodes != total || ni.unschedulableGpuNodes != unschedulable
	ni.totalGpuNodes = total
	ni.unschedulableGpuNodes = unschedulable
	ni.mutex.Unlock()

	if changed {
		klog.V(2).Infof("Node counts updated: Total GPU Nodes=%d, Unschedulable GPU Nodes=%d", total, unschedulable)
	} else {
		klog.V(4).Infof("Node counts recalculated, no change: Total GPU Nodes=%d, Unschedulable GPU Nodes=%d",
			total, unschedulable)
	}

	return changed, nil
}

// signalWork sends a non-blocking signal to the reconciler's work channel.
func (ni *NodeInformer) signalWork() {
	if ni.workSignal == nil {
		klog.Errorf("No channel configured for node informer")
		return // No channel configured
	}
	select {
	case ni.workSignal <- struct{}{}:
		klog.V(3).Infof("Signalled work channel due to node change.")
	default:
		klog.V(3).Infof("Work channel already signalled, skipping signal for node change.")
	}
}
