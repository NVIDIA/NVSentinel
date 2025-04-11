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
	"context"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Helper function to create a node object
func newNode(name string, labels map[string]string, unschedulable bool) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: v1.NodeSpec{
			Unschedulable: unschedulable,
		},
	}
}

// Helper function to create a GPU node object
func newGpuNode(name string, unschedulable bool) *v1.Node {
	return newNode(name, map[string]string{GpuNodeLabel: "true"}, unschedulable)
}

func TestNewNodeInformer(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 1) // Buffered channel

	ni, err := NewNodeInformer(clientset, 0, workSignal) // 0 resync period for tests

	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}
	if ni == nil {
		t.Fatal("NewNodeInformer returned nil informer")
	}
	if ni.clientset != clientset {
		t.Error("Clientset not stored correctly")
	}
	if ni.informer == nil {
		t.Error("Informer not created")
	}
	if ni.lister == nil {
		t.Error("Lister not created")
	}
	if ni.informerSynced == nil {
		t.Error("InformerSynced function not set")
	}
	if ni.workSignal != workSignal {
		t.Error("WorkSignal channel not stored correctly")
	}
}

// waitForSync waits for the informer cache to sync or times out.
func waitForSync(t *testing.T, stopCh chan struct{}, informerSynced cache.InformerSynced) {
	t.Helper()
	syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // Timeout for sync
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informerSynced) {
		t.Fatal("Timed out waiting for caches to sync")
	}
}

func TestNodeInformer_RunAndSync(t *testing.T) {
	clientset := fake.NewSimpleClientset(newGpuNode("gpu-node-1", false))
	workSignal := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	defer close(stopCh)

	ni, err := NewNodeInformer(clientset, 0, workSignal)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	var runErr error        // Variable to store error from the Run goroutine
	var runErrMu sync.Mutex // Mutex to protect runErr
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		localErr := ni.Run(stopCh) // Use a local variable inside goroutine
		if localErr != nil {
			runErrMu.Lock()
			runErr = localErr // Assign protected by mutex
			runErrMu.Unlock()
		}
	}()

	// Wait for sync completion which happens inside Run
	waitForSync(t, stopCh, ni.informerSynced)

	if !ni.HasSynced() {
		t.Error("Expected HasSynced to be true after Run completed sync")
	}

	// Check initial counts after sync
	total, unschedulable, err := ni.GetGpuNodeCounts()
	if err != nil {
		t.Errorf("GetGpuNodeCounts failed after sync: %v", err)
	}
	if total != 1 {
		t.Errorf("Expected 1 total GPU node after sync, got %d", total)
	}
	if unschedulable != 0 {
		t.Errorf("Expected 0 unschedulable GPU nodes after sync, got %d", unschedulable)
	}

	// Stop the informer and wait for Run goroutine to exit
	// The deferred close(stopCh) will signal the Run goroutine to stop.
	// wg.Wait() ensures we wait for the Run goroutine to finish processing the stop signal.
	wg.Wait()

	runErrMu.Lock()       // Lock before reading runErr
	finalRunErr := runErr // Read protected by mutex
	runErrMu.Unlock()     // Unlock after reading

	if finalRunErr != nil {
		// We expect nil error on clean shutdown, potentially error if sync failed before shutdown
		// Allow the specific sync error in case waitForSync timed out but Run exited cleanly later
		if finalRunErr.Error() != "failed to wait for caches to sync" {
			t.Errorf("ni.Run returned unexpected error: %v", finalRunErr)
		}
	}
}

func TestNodeInformer_GetGpuNodeCounts_NotSynced(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 1)

	ni, err := NewNodeInformer(clientset, 0, workSignal)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Don't run the informer, so it won't be synced
	_, _, err = ni.GetGpuNodeCounts()
	if err == nil {
		t.Error("Expected error when getting counts before cache sync, got nil")
	} else if err.Error() != "node informer cache not synced yet" {
		t.Errorf("Expected specific 'not synced' error, got: %v", err)
	}
}

func TestNodeInformer_EventHandlers(t *testing.T) {
	clientset := fake.NewSimpleClientset() // Start with no nodes
	workSignal := make(chan struct{}, 5)   // Buffer to avoid blocking on signals
	stopCh := make(chan struct{})
	defer close(stopCh)

	ni, err := NewNodeInformer(clientset, 0, workSignal)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Need access to the informer's store to add/update/delete objects directly
	store := ni.informer.GetStore()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run blocks until sync or stopCh is closed
		_ = ni.Run(stopCh)
	}()

	// Wait for initial sync
	waitForSync(t, stopCh, ni.informerSynced)
	t.Log("Initial sync complete")

	// Check initial state (0 nodes)
	total, unschedulable, err := ni.GetGpuNodeCounts()
	if err != nil {
		t.Fatalf("GetGpuNodeCounts failed after initial sync: %v", err)
	}
	if total != 0 || unschedulable != 0 {
		t.Fatalf("Expected 0 nodes initially, got total=%d, unschedulable=%d", total, unschedulable)
	}

	// --- Test Add ---
	node1 := newGpuNode("gpu-node-1", false)
	t.Logf("Adding node: %s", node1.Name)
	err = store.Add(node1)
	if err != nil {
		t.Fatalf("Failed to add node1 to store: %v", err)
	}
	ni.handleAddNode(node1) // Manually trigger handler
	<-workSignal            // Wait for signal
	total, unschedulable, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || unschedulable != 0 {
		t.Errorf("After adding node1: expected total=1, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, unschedulable, err)
	}
	// --- Test Update (Cordon) ---
	node1Cordoned := newGpuNode("gpu-node-1", true) // Same node, now unschedulable
	t.Logf("Updating node: %s (cordon)", node1.Name)
	err = store.Update(node1Cordoned)
	if err != nil {
		t.Fatalf("Failed to update node1 in store: %v", err)
	}
	ni.handleUpdateNode(node1, node1Cordoned) // Manually trigger handler
	<-workSignal                              // Wait for signal
	total, unschedulable, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || unschedulable != 1 {
		t.Errorf("After cordoning node1: expected total=1, unschedulable=1, err=nil; got total=%d, unschedulable=%d, err=%v", total, unschedulable, err)
	}

	// --- Test Update (No relevant change) ---
	node1CordonedUpdated := node1Cordoned.DeepCopy()
	node1CordonedUpdated.Annotations = map[string]string{"new": "annotation"} // Change something irrelevant
	t.Logf("Updating node: %s (irrelevant change)", node1Cordoned.Name)
	err = store.Update(node1CordonedUpdated)
	if err != nil {
		t.Fatalf("Failed to update node1 again in store: %v", err)
	}
	ni.handleUpdateNode(node1Cordoned, node1CordonedUpdated) // Manually trigger handler
	// No signal expected
	select {
	case <-workSignal:
		t.Error("Unexpected work signal after irrelevant node update")
	default:
		// Expected path
	}
	total, unschedulable, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || unschedulable != 1 {
		t.Errorf("After irrelevant update node1: expected total=1, unschedulable=1, err=nil; got total=%d, unschedulable=%d, err=%v", total, unschedulable, err)
	}

	// --- Test Delete ---
	t.Logf("Deleting node: %s", node1CordonedUpdated.Name)
	err = store.Delete(node1CordonedUpdated)
	if err != nil {
		t.Fatalf("Failed to delete node1 from store: %v", err)
	}
	ni.handleDeleteNode(node1CordonedUpdated) // Manually trigger handler
	// No signal expected
	<-workSignal
	total, unschedulable, err = ni.GetGpuNodeCounts()
	if err != nil || total != 0 || unschedulable != 0 {
		t.Errorf("After deleting node1: expected total=0, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, unschedulable, err)
	}

	// --- Test Delete (Tombstone) ---
	node2 := newGpuNode("gpu-node-2", true)
	t.Logf("Adding node: %s", node2.Name)
	store.Add(node2)
	ni.handleAddNode(node2)
	<-workSignal // Consume signal
	t.Logf("Deleting node with tombstone: %s", node2.Name)
	tombstone := cache.DeletedFinalStateUnknown{Key: "default/gpu-node-2", Obj: node2}
	store.Delete(node2)            // Ensure it's removed from the store view for recalculate
	ni.handleDeleteNode(tombstone) // Trigger handler with tombstone
	<-workSignal                   // Wait for signal
	total, unschedulable, err = ni.GetGpuNodeCounts()
	if err != nil || total != 0 || unschedulable != 0 {
		t.Errorf("After deleting node2 via tombstone: expected total=0, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, unschedulable, err)
	}

	// The deferred close(stopCh) will signal the Run goroutine to stop.
	wg.Wait()
}

func TestNodeInformer_RecalculateCounts(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	defer close(stopCh)

	// Pre-populate nodes directly (won't trigger handlers)
	node1 := newGpuNode("gpu-node-1", false)
	node2 := newGpuNode("gpu-node-2", true)
	node3 := newGpuNode("gpu-node-3", false)

	// Use a fake informer manually for this test to control the store directly
	fakeInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				// Filter list based on label selector if needed, though Lister does it
				return clientset.CoreV1().Nodes().List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return clientset.CoreV1().Nodes().Watch(context.TODO(), options)
			},
		},
		&v1.Node{},
		0, // No resync
		cache.Indexers{},
	)
	lister := corelisters.NewNodeLister(fakeInformer.GetIndexer())

	ni := &NodeInformer{
		clientset:      clientset,
		informer:       fakeInformer, // Use manually created informer
		lister:         lister,       // Use lister associated with the manual informer
		informerSynced: fakeInformer.HasSynced,
		workSignal:     workSignal,
	}

	// Manually add nodes to the informer's store
	fakeInformer.GetStore().Add(node1)
	fakeInformer.GetStore().Add(node2)
	fakeInformer.GetStore().Add(node3)

	// Run recalculate directly
	_, err := ni.recalculateCounts() // Assign both bool and error, ignore bool
	if err != nil {
		t.Fatalf("recalculateCounts failed: %v", err)
	}

	// Check internal counts directly
	ni.mutex.RLock()
	total := ni.totalGpuNodes
	unschedulable := ni.unschedulableGpuNodes
	ni.mutex.RUnlock()

	if total != 3 {
		t.Errorf("Expected totalGpuNodes=3, got %d", total)
	}
	if unschedulable != 1 {
		t.Errorf("Expected unschedulableGpuNodes=1, got %d", unschedulable)
	}
}

func TestNodeInformer_SignalWork(t *testing.T) {
	// Test signal sent
	workSignal := make(chan struct{}, 1)
	ni := &NodeInformer{workSignal: workSignal}
	ni.signalWork()
	select {
	case <-workSignal:
		// Expected path
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for work signal")
	}

	// Test non-blocking behavior (channel full)
	ni.signalWork() // Should not block
	select {
	case workSignal <- struct{}{}:
		t.Error("Should not have been able to send to already full channel")
	default:
		// Expected path, signal was dropped
	}

	// Test nil channel
	niNil := &NodeInformer{workSignal: nil}
	// Should not panic
	niNil.signalWork()
}
