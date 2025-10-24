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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/nodeinfo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo) // 0 resync period for tests

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

// safeReceiveSignal waits for a signal with a timeout to prevent test hangs
func safeReceiveSignal(t *testing.T, workSignal chan struct{}, expectSignal bool) bool {
	t.Helper()

	select {
	case <-workSignal:
		if !expectSignal {
			t.Log("Received unexpected signal")
		}
		return true
	case <-time.After(500 * time.Millisecond):
		if expectSignal {
			t.Error("Expected signal but none received within timeout")
		}
		return false
	}
}

func TestNodeInformer_RunAndSync(t *testing.T) {
	clientset := fake.NewSimpleClientset(newGpuNode("gpu-node-1", false))
	workSignal := make(chan struct{}, 1)
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)
	stopCh := make(chan struct{})
	defer close(stopCh)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
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
	total, unschedulableMap, err := ni.GetGpuNodeCounts()
	if err != nil {
		t.Errorf("GetGpuNodeCounts failed after sync: %v", err)
	}
	if total != 1 {
		t.Errorf("Expected 1 total GPU node after sync, got %d", total)
	}
	if len(unschedulableMap) != 0 {
		t.Errorf("Expected 0 unschedulable GPU nodes after sync, got %d", len(unschedulableMap))
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
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
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
	workSignal := make(chan struct{}, 10)
	stopCh := make(chan struct{})
	defer close(stopCh)

	nodeInfo := nodeinfo.NewNodeInfo(workSignal)
	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
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

	// Drain any initial signals from cache sync
	drainSignals := func() {
		for {
			select {
			case <-workSignal:
				// Keep draining
			default:
				return
			}
		}
	}
	drainSignals()

	// Check initial state (0 nodes)
	total, unschedulableMap, err := ni.GetGpuNodeCounts()
	if err != nil {
		t.Fatalf("GetGpuNodeCounts failed after initial sync: %v", err)
	}
	if total != 0 || len(unschedulableMap) != 0 {
		t.Fatalf("Expected 0 nodes initially, got total=%d, unschedulable=%d", total, len(unschedulableMap))
	}

	// --- Test Add ---
	node1 := newGpuNode("gpu-node-1", false)
	t.Logf("Adding node: %s", node1.Name)
	err = store.Add(node1)
	if err != nil {
		t.Fatalf("Failed to add node1 to store: %v", err)
	}
	ni.handleAddNode(node1)                // Manually trigger handler
	safeReceiveSignal(t, workSignal, true) // Expect signal, with timeout
	drainSignals()                         // Drain any extra signals
	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || len(unschedulableMap) != 0 {
		t.Errorf("After adding node1: expected total=1, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
	}
	// --- Test Update (Cordon) ---
	node1Cordoned := newGpuNode("gpu-node-1", true) // Same node, now unschedulable
	t.Logf("Updating node: %s (cordon)", node1.Name)
	err = store.Update(node1Cordoned)
	if err != nil {
		t.Fatalf("Failed to update node1 in store: %v", err)
	}
	ni.handleUpdateNode(node1, node1Cordoned) // Manually trigger handler
	safeReceiveSignal(t, workSignal, true)    // Expect signal, with timeout
	drainSignals()
	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || len(unschedulableMap) != 1 {
		t.Errorf("After cordoning node1: expected total=1, unschedulable=1, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
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
	drainSignals()
	// No signal expected for irrelevant updates
	safeReceiveSignal(t, workSignal, false) // Don't expect signal, with timeout
	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || len(unschedulableMap) != 1 {
		t.Errorf("After irrelevant update node1: expected total=1, unschedulable=1, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
	}

	// --- Test Delete ---
	t.Logf("Deleting node: %s", node1CordonedUpdated.Name)
	err = store.Delete(node1CordonedUpdated)
	if err != nil {
		t.Fatalf("Failed to delete node1 from store: %v", err)
	}
	ni.handleDeleteNode(node1CordonedUpdated) // Manually trigger handler
	safeReceiveSignal(t, workSignal, true)    // Expect signal, with timeout
	drainSignals()

	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 0 || len(unschedulableMap) != 0 {
		t.Errorf("After deleting node1: expected total=0, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
	}

	// --- Test Delete (Tombstone) ---
	node2 := newGpuNode("gpu-node-2", true)
	t.Logf("Adding node: %s", node2.Name)
	err = store.Add(node2)
	if err != nil {
		t.Fatalf("Failed to add node2 to store: %v", err)
	}
	ni.handleAddNode(node2)                // Manually trigger handler
	safeReceiveSignal(t, workSignal, true) // Expect signal, with timeout
	drainSignals()

	// Verify node2 was added
	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 1 || len(unschedulableMap) != 1 {
		t.Errorf("After adding node2: expected total=1, unschedulable=1, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
	}

	t.Logf("Deleting node with tombstone: %s", node2.Name)
	err = store.Delete(node2)
	if err != nil {
		t.Fatalf("Failed to delete node2 from store: %v", err)
	}
	tombstone := cache.DeletedFinalStateUnknown{Key: "default/gpu-node-2", Obj: node2}
	ni.handleDeleteNode(tombstone)         // Trigger handler with tombstone
	safeReceiveSignal(t, workSignal, true) // Expect signal, with timeout
	drainSignals()
	total, unschedulableMap, err = ni.GetGpuNodeCounts()
	if err != nil || total != 0 || len(unschedulableMap) != 0 {
		t.Errorf("After deleting node2 via tombstone: expected total=0, unschedulable=0, err=nil; got total=%d, unschedulable=%d, err=%v", total, len(unschedulableMap), err)
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
	node1 := newGpuNode("gpu-node-1", true)
	node2 := newGpuNode("gpu-node-2", true)
	node3 := newGpuNode("gpu-node-3", false)

	nodeInfo := nodeinfo.NewNodeInfo(workSignal)
	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Manually add nodes to the informer's store
	ni.informer.GetStore().Add(node1)
	ni.informer.GetStore().Add(node2)
	ni.informer.GetStore().Add(node3)

	// Manually mark the nodes as quarantined in the nodeInfo cache
	// since the handleAddNode isn't being called
	nodeInfo.MarkNodeQuarantineStatusCache("gpu-node-1", true, true)
	nodeInfo.MarkNodeQuarantineStatusCache("gpu-node-2", true, true)
	nodeInfo.MarkNodeQuarantineStatusCache("gpu-node-3", false, true)

	// Run recalculate directly
	_, err = ni.recalculateCounts() // Assign both bool and error, ignore bool
	if err != nil {
		t.Fatalf("recalculateCounts failed: %v", err)
	}

	// Check internal counts directly
	ni.mutex.RLock()
	total := ni.totalGpuNodes
	ni.mutex.RUnlock()
	unschedulable := ni.nodeInfo.GetQuarantinedNodesCount()

	if total != 3 {
		t.Errorf("Expected totalGpuNodes=3, got %d", total)
	}
	if unschedulable != 2 {
		t.Errorf("Expected unschedulableGpuNodes=2, got %d", unschedulable)
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

func TestNodeInformer_OnQuarantinedNodeDeletedCallback(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 1)
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Set up callback to track calls
	var callbackCalled bool
	var callbackNodeName string
	ni.SetOnQuarantinedNodeDeletedCallback(func(nodeName string) {
		callbackCalled = true
		callbackNodeName = nodeName
	})

	// Test 1: Delete a quarantined node with annotation - callback should be called
	node1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{GpuNodeLabel: "true"},
			Annotations: map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
		},
	}

	// Simulate the node being in quarantine cache
	nodeInfo.MarkNodeQuarantineStatusCache("gpu-node-1", true, true)

	// Handle delete
	ni.handleDeleteNode(node1)

	if !callbackCalled {
		t.Error("Expected callback to be called for quarantined node with annotation")
	}
	if callbackNodeName != "gpu-node-1" {
		t.Errorf("Expected callback node name to be gpu-node-1, got %s", callbackNodeName)
	}

	// Test 2: Delete a quarantined node without annotation - callback should NOT be called
	callbackCalled = false
	callbackNodeName = ""

	node2 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-2",
			Labels: map[string]string{GpuNodeLabel: "true"},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
		},
	}

	// Simulate the node being in quarantine cache but without annotation
	nodeInfo.MarkNodeQuarantineStatusCache("gpu-node-2", true, false)

	// Handle delete
	ni.handleDeleteNode(node2)

	// Test 3: Delete a non-quarantined node - callback should NOT be called
	node3 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-3",
			Labels: map[string]string{GpuNodeLabel: "true"},
		},
		Spec: v1.NodeSpec{
			Unschedulable: false,
		},
	}

	// Handle delete (node not in quarantine cache)
	ni.handleDeleteNode(node3)

	if callbackCalled {
		t.Error("Expected callback NOT to be called for non-quarantined node")
	}
}

func TestNodeInformer_OnNodeAnnotationsChangedCallback(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 1)
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Set up callback to track calls
	var callbackCalls []struct {
		nodeName    string
		annotations map[string]string
	}
	var mu sync.Mutex

	ni.SetOnNodeAnnotationsChangedCallback(func(nodeName string, annotations map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		// Make a copy of annotations to avoid race conditions
		annotationsCopy := make(map[string]string)
		for k, v := range annotations {
			annotationsCopy[k] = v
		}
		callbackCalls = append(callbackCalls, struct {
			nodeName    string
			annotations map[string]string
		}{nodeName: nodeName, annotations: annotationsCopy})
	})

	// Test 1: Add a node with quarantine annotations - callback should be called
	node1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{GpuNodeLabel: "true"},
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data",
				"other-annotation":                        "should-be-ignored",
			},
		},
	}

	ni.handleAddNode(node1)

	// Check callback was called
	mu.Lock()
	if len(callbackCalls) != 1 {
		t.Errorf("Expected 1 callback call after add, got %d", len(callbackCalls))
	} else {
		call := callbackCalls[0]
		if call.nodeName != "gpu-node-1" {
			t.Errorf("Expected node name gpu-node-1, got %s", call.nodeName)
		}
		if len(call.annotations) != 1 {
			t.Errorf("Expected 1 annotation, got %d", len(call.annotations))
		}
		if call.annotations[common.QuarantineHealthEventAnnotationKey] != "event-data" {
			t.Errorf("Expected quarantine annotation value 'event-data', got %s",
				call.annotations[common.QuarantineHealthEventAnnotationKey])
		}
		if _, exists := call.annotations["other-annotation"]; exists {
			t.Error("Non-quarantine annotation should not be included")
		}
	}
	mu.Unlock()

	// Test 2: Add a node without quarantine annotations - callback should be called with empty map
	node2 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-2",
			Labels: map[string]string{GpuNodeLabel: "true"},
			Annotations: map[string]string{
				"other-annotation": "value",
			},
		},
	}

	ni.handleAddNode(node2)

	mu.Lock()
	if len(callbackCalls) != 2 {
		t.Errorf("Expected 2 callback calls (including empty annotations), got %d", len(callbackCalls))
	} else {
		call := callbackCalls[1]
		if call.nodeName != "gpu-node-2" {
			t.Errorf("Expected node name gpu-node-2, got %s", call.nodeName)
		}
		if len(call.annotations) != 0 {
			t.Errorf("Expected 0 quarantine annotations for clean node, got %d", len(call.annotations))
		}
	}
	mu.Unlock()

	// Test 3: Update node with changed quarantine annotations - callback should be called
	node1Updated := node1.DeepCopy()
	node1Updated.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = common.QuarantineHealthEventIsCordonedAnnotationValueTrue
	node1Updated.Annotations["other-annotation"] = "new-value" // This change should be ignored

	ni.handleUpdateNode(node1, node1Updated)

	mu.Lock()
	if len(callbackCalls) != 3 {
		t.Errorf("Expected 3 callback calls after update, got %d", len(callbackCalls))
	} else {
		call := callbackCalls[2]
		if call.nodeName != "gpu-node-1" {
			t.Errorf("Expected node name gpu-node-1, got %s", call.nodeName)
		}
		if len(call.annotations) != 2 {
			t.Errorf("Expected 2 annotations, got %d", len(call.annotations))
		}
		if call.annotations[common.QuarantineHealthEventAnnotationKey] != "event-data" {
			t.Errorf("Expected quarantine annotation value 'event-data', got %s",
				call.annotations[common.QuarantineHealthEventAnnotationKey])
		}
		if call.annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] != common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
			t.Errorf("Expected cordoned annotation value 'True', got %s",
				call.annotations[common.QuarantineHealthEventIsCordonedAnnotationKey])
		}
	}
	mu.Unlock()

	// Test 4: Update node with only non-quarantine annotation changes - callback should NOT be called
	node1UpdatedAgain := node1Updated.DeepCopy()
	node1UpdatedAgain.Annotations["other-annotation"] = "another-value"
	node1UpdatedAgain.Spec.Unschedulable = true // Change something else

	ni.handleUpdateNode(node1Updated, node1UpdatedAgain)

	mu.Lock()
	if len(callbackCalls) != 3 {
		t.Errorf("Expected still 3 callback calls (no new call for non-quarantine changes), got %d", len(callbackCalls))
	}
	mu.Unlock()

	// Test 5: Delete node - callback should be called with nil annotations
	ni.handleDeleteNode(node1UpdatedAgain)

	mu.Lock()
	if len(callbackCalls) != 4 {
		t.Errorf("Expected 4 callback calls after delete, got %d", len(callbackCalls))
	} else {
		call := callbackCalls[3]
		if call.nodeName != "gpu-node-1" {
			t.Errorf("Expected node name gpu-node-1, got %s", call.nodeName)
		}
		if len(call.annotations) > 0 {
			t.Errorf("Expected nil or empty annotations for deleted node, got %v", call.annotations)
		}
	}
	mu.Unlock()

	// Test 6: Update to remove quarantine annotations - callback should be called
	node3 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-3",
			Labels: map[string]string{GpuNodeLabel: "true"},
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data",
			},
		},
	}
	node3Updated := node3.DeepCopy()
	delete(node3Updated.Annotations, common.QuarantineHealthEventAnnotationKey)

	// Reset callback calls
	mu.Lock()
	callbackCalls = callbackCalls[:0]
	mu.Unlock()

	ni.handleAddNode(node3)
	ni.handleUpdateNode(node3, node3Updated)

	mu.Lock()
	if len(callbackCalls) != 2 {
		t.Errorf("Expected 2 callback calls (add + update with annotation removal), got %d", len(callbackCalls))
	} else {
		// First call should have the annotation
		if len(callbackCalls[0].annotations) != 1 {
			t.Errorf("Expected 1 annotation in first call, got %d", len(callbackCalls[0].annotations))
		}
		// Second call should have empty annotations (all quarantine annotations removed)
		if len(callbackCalls[1].annotations) != 0 {
			t.Errorf("Expected 0 annotations in second call, got %d", len(callbackCalls[1].annotations))
		}
	}
	mu.Unlock()
}

// Test edge cases for quarantine annotation tracking
func TestHasQuarantineAnnotationsChanged_EdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		oldAnnotations map[string]string
		newAnnotations map[string]string
		expectChanged  bool
	}{
		{
			name:           "nil to nil annotations",
			oldAnnotations: nil,
			newAnnotations: nil,
			expectChanged:  false,
		},
		{
			name:           "nil to empty annotations",
			oldAnnotations: nil,
			newAnnotations: map[string]string{},
			expectChanged:  false,
		},
		{
			name:           "empty to nil annotations",
			oldAnnotations: map[string]string{},
			newAnnotations: nil,
			expectChanged:  false,
		},
		{
			name:           "nil to quarantine annotations",
			oldAnnotations: nil,
			newAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data",
			},
			expectChanged: true,
		},
		{
			name: "quarantine annotations to nil",
			oldAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data",
			},
			newAnnotations: nil,
			expectChanged:  true,
		},
		{
			name: "only non-quarantine annotations change",
			oldAnnotations: map[string]string{
				"other-annotation":                        "value1",
				common.QuarantineHealthEventAnnotationKey: "event-data",
			},
			newAnnotations: map[string]string{
				"other-annotation":                        "value2",
				common.QuarantineHealthEventAnnotationKey: "event-data",
			},
			expectChanged: false,
		},
		{
			name: "quarantine annotation value changes",
			oldAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data-1",
			},
			newAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event-data-2",
			},
			expectChanged: true,
		},
		{
			name: "multiple quarantine annotations, one changes",
			oldAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:              "event-data",
				common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
				common.QuarantineHealthEventAppliedTaintsAnnotationKey: "[taint1]",
			},
			newAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:              "event-data",
				common.QuarantineHealthEventIsCordonedAnnotationKey:    "False", // changed
				common.QuarantineHealthEventAppliedTaintsAnnotationKey: "[taint1]",
			},
			expectChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := hasQuarantineAnnotationsChanged(tt.oldAnnotations, tt.newAnnotations)
			if changed != tt.expectChanged {
				t.Errorf("hasQuarantineAnnotationsChanged() = %v, want %v", changed, tt.expectChanged)
			}
		})
	}
}

// Test race conditions in annotation change callbacks
func TestNodeInformer_AnnotationCallbackRaceCondition(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	workSignal := make(chan struct{}, 100) // larger buffer for concurrent events
	nodeInfo := nodeinfo.NewNodeInfo(workSignal)

	ni, err := NewNodeInformer(clientset, 0, workSignal, nodeInfo)
	if err != nil {
		t.Fatalf("NewNodeInformer failed: %v", err)
	}

	// Track callback invocations with thread-safe access
	var mu sync.Mutex
	callbackInvocations := make(map[string][]map[string]string)

	ni.SetOnNodeAnnotationsChangedCallback(func(nodeName string, annotations map[string]string) {
		mu.Lock()
		defer mu.Unlock()

		// Deep copy annotations to avoid race conditions
		annotationsCopy := make(map[string]string)
		for k, v := range annotations {
			annotationsCopy[k] = v
		}

		callbackInvocations[nodeName] = append(callbackInvocations[nodeName], annotationsCopy)
	})

	// Create multiple goroutines that concurrently update nodes
	var wg sync.WaitGroup
	nodeCount := 10
	updateCount := 5

	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node-%d", i)

		// Initial node
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{GpuNodeLabel: "true"},
			},
		}

		wg.Add(1)
		go func(n *v1.Node) {
			defer wg.Done()

			// Add node
			ni.handleAddNode(n)

			// Perform multiple concurrent updates
			for j := 0; j < updateCount; j++ {
				oldNode := n.DeepCopy()
				newNode := n.DeepCopy()

				// Alternate between adding and removing quarantine annotations
				if j%2 == 0 {
					newNode.Annotations = map[string]string{
						common.QuarantineHealthEventAnnotationKey: fmt.Sprintf("event-%d", j),
					}
				} else {
					newNode.Annotations = map[string]string{}
				}

				ni.handleUpdateNode(oldNode, newNode)
				n = newNode
			}

			// Delete node
			ni.handleDeleteNode(n)
		}(node)
	}

	wg.Wait()

	// Verify callback was called correct number of times for each node
	mu.Lock()
	defer mu.Unlock()

	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		invocations := callbackInvocations[nodeName]

		// Expected: 1 add + updateCount updates + 1 delete
		expectedCallbacks := 1 + updateCount + 1

		if len(invocations) != expectedCallbacks {
			t.Errorf("Node %s: expected %d callbacks, got %d", nodeName, expectedCallbacks, len(invocations))
		}

		// Verify last invocation is for deletion (nil or empty map)
		lastInvocation := invocations[len(invocations)-1]
		if len(lastInvocation) > 0 {
			t.Errorf("Node %s: expected last callback to be for deletion (empty annotations), got %v",
				nodeName, lastInvocation)
		}
	}
}

// Test that getQuarantineAnnotations filters correctly
func TestGetQuarantineAnnotations(t *testing.T) {
	tests := []struct {
		name           string
		allAnnotations map[string]string
		expectedResult map[string]string
	}{
		{
			name:           "nil annotations",
			allAnnotations: nil,
			expectedResult: map[string]string{},
		},
		{
			name:           "empty annotations",
			allAnnotations: map[string]string{},
			expectedResult: map[string]string{},
		},
		{
			name: "only quarantine annotations",
			allAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:              "event1",
				common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
				common.QuarantineHealthEventAppliedTaintsAnnotationKey: "[taint]",
			},
			expectedResult: map[string]string{
				common.QuarantineHealthEventAnnotationKey:              "event1",
				common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
				common.QuarantineHealthEventAppliedTaintsAnnotationKey: "[taint]",
			},
		},
		{
			name: "mixed annotations",
			allAnnotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:           "event1",
				"kubernetes.io/some-annotation":                     "value",
				"custom-annotation":                                 "custom-value",
				common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
			},
			expectedResult: map[string]string{
				common.QuarantineHealthEventAnnotationKey:           "event1",
				common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
			},
		},
		{
			name: "no quarantine annotations",
			allAnnotations: map[string]string{
				"kubernetes.io/annotation1": "value1",
				"custom-annotation":         "value2",
			},
			expectedResult: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getQuarantineAnnotations(tt.allAnnotations)

			if len(result) != len(tt.expectedResult) {
				t.Errorf("Expected %d quarantine annotations, got %d",
					len(tt.expectedResult), len(result))
			}

			for key, expectedValue := range tt.expectedResult {
				if actualValue, exists := result[key]; !exists {
					t.Errorf("Expected annotation %s not found in result", key)
				} else if actualValue != expectedValue {
					t.Errorf("Annotation %s: expected value %s, got %s",
						key, expectedValue, actualValue)
				}
			}

			// Ensure no extra annotations
			for key := range result {
				if _, expected := tt.expectedResult[key]; !expected {
					t.Errorf("Unexpected annotation %s in result", key)
				}
			}
		})
	}
}
