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

package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

func TestGpuCache_RegisterAndGet(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register a GPU
	spec := &v1alpha1.GpuSpec{Uuid: "GPU-1234"}
	created, version, err := c.Register("gpu-0", spec, nil, "test-provider")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if !created {
		t.Error("Expected created=true for new GPU")
	}
	if version != 1 {
		t.Errorf("Expected version=1, got %d", version)
	}

	// Get the GPU
	gpu, found := c.Get("gpu-0")
	if !found {
		t.Fatal("GPU not found")
	}
	if gpu.GetMetadata().GetName() != "gpu-0" {
		t.Errorf("Expected name=gpu-0, got %s", gpu.GetMetadata().GetName())
	}
	if gpu.Spec.Uuid != "GPU-1234" {
		t.Errorf("Expected UUID=GPU-1234, got %s", gpu.Spec.Uuid)
	}

	// Register same GPU again
	created, version, err = c.Register("gpu-0", spec, nil, "test-provider")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if created {
		t.Error("Expected created=false for existing GPU")
	}
	if version != 1 {
		t.Errorf("Expected version=1 (unchanged), got %d", version)
	}
}

func TestGpuCache_Unregister(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register a GPU
	spec := &v1alpha1.GpuSpec{Uuid: "GPU-1234"}
	c.Register("gpu-0", spec, nil, "test-provider")

	// Unregister
	deleted := c.Unregister("gpu-0")
	if !deleted {
		t.Error("Expected deleted=true")
	}

	// Verify gone
	_, found := c.Get("gpu-0")
	if found {
		t.Error("GPU should not be found after unregister")
	}

	// Unregister again
	deleted = c.Unregister("gpu-0")
	if deleted {
		t.Error("Expected deleted=false for non-existent GPU")
	}
}

func TestGpuCache_UpdateStatus(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register a GPU
	spec := &v1alpha1.GpuSpec{Uuid: "GPU-1234"}
	c.Register("gpu-0", spec, nil, "test-provider")

	// Update status
	status := &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{
			{Type: "Ready", Status: "True"},
		},
	}
	version, err := c.UpdateStatus("gpu-0", status, "test-provider")
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}
	if version != 2 {
		t.Errorf("Expected version=2, got %d", version)
	}

	// Verify status
	gpu, _ := c.Get("gpu-0")
	if len(gpu.Status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(gpu.Status.Conditions))
	}
	if gpu.Status.Conditions[0].Status != "True" {
		t.Errorf("Expected status=True, got %s", gpu.Status.Conditions[0].Status)
	}

	// Update non-existent GPU
	_, err = c.UpdateStatus("gpu-999", status, "test-provider")
	if err != ErrGpuNotFound {
		t.Errorf("Expected ErrGpuNotFound, got %v", err)
	}
}

func TestGpuCache_UpdateCondition(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register a GPU with initial status
	spec := &v1alpha1.GpuSpec{Uuid: "GPU-1234"}
	initialStatus := &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{
			{Type: "Ready", Status: "True"},
		},
	}
	c.Register("gpu-0", spec, initialStatus, "test-provider")

	// Add new condition
	condition := &v1alpha1.Condition{Type: "Healthy", Status: "True"}
	version, err := c.UpdateCondition("gpu-0", condition, "health-monitor")
	if err != nil {
		t.Fatalf("UpdateCondition failed: %v", err)
	}
	if version != 2 {
		t.Errorf("Expected version=2, got %d", version)
	}

	// Verify both conditions exist
	gpu, _ := c.Get("gpu-0")
	if len(gpu.Status.Conditions) != 2 {
		t.Errorf("Expected 2 conditions, got %d", len(gpu.Status.Conditions))
	}

	// Update existing condition
	condition = &v1alpha1.Condition{Type: "Ready", Status: "False"}
	version, err = c.UpdateCondition("gpu-0", condition, "test-provider")
	if err != nil {
		t.Fatalf("UpdateCondition failed: %v", err)
	}
	if version != 3 {
		t.Errorf("Expected version=3, got %d", version)
	}

	// Verify condition was updated, not added
	gpu, _ = c.Get("gpu-0")
	if len(gpu.Status.Conditions) != 2 {
		t.Errorf("Expected 2 conditions (updated, not added), got %d", len(gpu.Status.Conditions))
	}
}

func TestGpuCache_List(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Empty list
	gpus := c.List()
	if len(gpus) != 0 {
		t.Errorf("Expected empty list, got %d", len(gpus))
	}

	// Add GPUs
	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, nil, "p")
	c.Register("gpu-1", &v1alpha1.GpuSpec{Uuid: "GPU-1"}, nil, "p")
	c.Register("gpu-2", &v1alpha1.GpuSpec{Uuid: "GPU-2"}, nil, "p")

	gpus = c.List()
	if len(gpus) != 3 {
		t.Errorf("Expected 3 GPUs, got %d", len(gpus))
	}
}

func TestGpuCache_GetStats(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Add GPUs with various states
	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True"}},
	}, "p")
	c.Register("gpu-1", &v1alpha1.GpuSpec{Uuid: "GPU-1"}, &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "False"}},
	}, "p")
	c.Register("gpu-2", &v1alpha1.GpuSpec{Uuid: "GPU-2"}, nil, "p") // No status

	stats := c.GetStats()
	if stats.TotalGpus != 3 {
		t.Errorf("Expected TotalGpus=3, got %d", stats.TotalGpus)
	}
	if stats.HealthyGpus != 1 {
		t.Errorf("Expected HealthyGpus=1, got %d", stats.HealthyGpus)
	}
	if stats.UnhealthyGpus != 1 {
		t.Errorf("Expected UnhealthyGpus=1, got %d", stats.UnhealthyGpus)
	}
	if stats.UnknownGpus != 1 {
		t.Errorf("Expected UnknownGpus=1, got %d", stats.UnknownGpus)
	}
}

// TestGpuCache_ReadBlocksDuringWrite verifies the critical read-blocking behavior.
// When a write is in progress, new readers MUST block until the write completes.
func TestGpuCache_ReadBlocksDuringWrite(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register a GPU
	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, nil, "p")

	var (
		writeStarted  = make(chan struct{})
		writeComplete = make(chan struct{})
		readStarted   = make(chan struct{})
		readComplete  = make(chan struct{})
		readBlocked   atomic.Bool
	)

	// Start a slow write that holds the lock
	go func() {
		c.mu.Lock()
		close(writeStarted)
		// Hold the lock for 100ms
		time.Sleep(100 * time.Millisecond)
		c.mu.Unlock()
		close(writeComplete)
	}()

	// Wait for write to acquire lock
	<-writeStarted

	// Start a read - should block
	go func() {
		close(readStarted)
		_, _ = c.Get("gpu-0") // This should block
		close(readComplete)
	}()

	<-readStarted
	// Give the read goroutine time to attempt the lock
	time.Sleep(20 * time.Millisecond)

	// Check if read has completed (it shouldn't have)
	select {
	case <-readComplete:
		t.Fatal("Read completed while write lock was held - blocking failed!")
	default:
		readBlocked.Store(true)
	}

	// Wait for write to complete
	<-writeComplete

	// Now read should complete
	select {
	case <-readComplete:
		// Expected - read completed after write released
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Read did not complete after write released")
	}

	if !readBlocked.Load() {
		t.Error("Read was not blocked during write")
	}
}

// TestGpuCache_ConcurrentReads verifies multiple readers can access concurrently.
func TestGpuCache_ConcurrentReads(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register some GPUs
	for i := 0; i < 10; i++ {
		c.Register(
			"gpu-"+string(rune('0'+i)),
			&v1alpha1.GpuSpec{Uuid: "GPU"},
			nil,
			"p",
		)
	}

	// Start many concurrent readers
	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				gpus := c.List()
				if len(gpus) != 10 {
					errors <- nil // Signal unexpected count
				}
			}
		}()
	}

	wg.Wait()
	close(errors)

	for range errors {
		t.Error("Concurrent read returned unexpected result")
	}
}

func TestGpuCache_MarkProviderGPUsUnknown(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register GPUs from different providers
	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True"}},
	}, "provider-a")
	c.Register("gpu-1", &v1alpha1.GpuSpec{Uuid: "GPU-1"}, &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True"}},
	}, "provider-a")
	c.Register("gpu-2", &v1alpha1.GpuSpec{Uuid: "GPU-2"}, &v1alpha1.GpuStatus{
		Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True"}},
	}, "provider-b")

	// Mark provider-a's GPUs as Unknown
	count := c.MarkProviderGPUsUnknown("provider-a")
	if count != 2 {
		t.Errorf("Expected 2 GPUs marked, got %d", count)
	}

	// Verify provider-a's GPUs are now Unknown
	gpu0, _ := c.Get("gpu-0")
	var gpu0Ready *v1alpha1.Condition
	for _, cond := range gpu0.Status.Conditions {
		if cond.Type == "Ready" {
			gpu0Ready = cond
			break
		}
	}
	if gpu0Ready == nil || gpu0Ready.Status != "Unknown" {
		t.Errorf("Expected gpu-0 Ready=Unknown, got %v", gpu0Ready)
	}

	gpu1, _ := c.Get("gpu-1")
	var gpu1Ready *v1alpha1.Condition
	for _, cond := range gpu1.Status.Conditions {
		if cond.Type == "Ready" {
			gpu1Ready = cond
			break
		}
	}
	if gpu1Ready == nil || gpu1Ready.Status != "Unknown" {
		t.Errorf("Expected gpu-1 Ready=Unknown, got %v", gpu1Ready)
	}

	// Verify provider-b's GPU is still healthy
	gpu2, _ := c.Get("gpu-2")
	var gpu2Ready *v1alpha1.Condition
	for _, cond := range gpu2.Status.Conditions {
		if cond.Type == "Ready" {
			gpu2Ready = cond
			break
		}
	}
	if gpu2Ready == nil || gpu2Ready.Status != "True" {
		t.Errorf("Expected gpu-2 Ready=True (unchanged), got %v", gpu2Ready)
	}

	// Mark non-existent provider (should return 0)
	count = c.MarkProviderGPUsUnknown("provider-c")
	if count != 0 {
		t.Errorf("Expected 0 GPUs marked for non-existent provider, got %d", count)
	}
}

func TestGpuCache_ListProviderGPUs(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	// Register GPUs from different providers
	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, nil, "provider-a")
	c.Register("gpu-1", &v1alpha1.GpuSpec{Uuid: "GPU-1"}, nil, "provider-a")
	c.Register("gpu-2", &v1alpha1.GpuSpec{Uuid: "GPU-2"}, nil, "provider-b")

	// List provider-a's GPUs
	names := c.ListProviderGPUs("provider-a")
	if len(names) != 2 {
		t.Errorf("Expected 2 GPUs for provider-a, got %d", len(names))
	}

	// List provider-b's GPUs
	names = c.ListProviderGPUs("provider-b")
	if len(names) != 1 {
		t.Errorf("Expected 1 GPU for provider-b, got %d", len(names))
	}

	// List non-existent provider's GPUs
	names = c.ListProviderGPUs("provider-c")
	if len(names) != 0 {
		t.Errorf("Expected 0 GPUs for non-existent provider, got %d", len(names))
	}
}

// TestGpuCache_WriteBlocksNewReaders verifies writer-preference behavior.
// When a write is pending, new readers should block (not cut in line).
func TestGpuCache_WriteBlocksNewReaders(t *testing.T) {
	logger := klog.Background()
	c := New(logger, nil)

	c.Register("gpu-0", &v1alpha1.GpuSpec{Uuid: "GPU-0"}, nil, "p")

	var (
		firstReadDone   = make(chan struct{})
		writePending    = make(chan struct{})
		secondReadStart = make(chan struct{})
		order           []string
		orderMu         sync.Mutex
	)

	// First reader - gets the lock
	go func() {
		c.mu.RLock()
		orderMu.Lock()
		order = append(order, "read1-start")
		orderMu.Unlock()

		// Wait for write to be pending
		<-writePending
		time.Sleep(50 * time.Millisecond)

		orderMu.Lock()
		order = append(order, "read1-end")
		orderMu.Unlock()
		c.mu.RUnlock()
		close(firstReadDone)
	}()

	// Give first reader time to acquire lock
	time.Sleep(10 * time.Millisecond)

	// Writer - will wait for first reader, then block second reader
	go func() {
		close(writePending)
		c.mu.Lock()
		orderMu.Lock()
		order = append(order, "write")
		orderMu.Unlock()
		c.mu.Unlock()
	}()

	// Second reader - should wait for writer
	go func() {
		<-secondReadStart
		c.mu.RLock()
		orderMu.Lock()
		order = append(order, "read2")
		orderMu.Unlock()
		c.mu.RUnlock()
	}()

	// Start second reader after write is pending
	time.Sleep(30 * time.Millisecond)
	close(secondReadStart)

	// Wait for everything to complete
	<-firstReadDone
	time.Sleep(100 * time.Millisecond)

	orderMu.Lock()
	defer orderMu.Unlock()

	// Expected order: read1-start, read1-end, write, read2
	// Writer should execute before second reader due to writer-preference
	if len(order) < 4 {
		t.Fatalf("Not all operations completed: %v", order)
	}

	// The write should come before read2
	writeIdx := -1
	read2Idx := -1
	for i, op := range order {
		if op == "write" {
			writeIdx = i
		}
		if op == "read2" {
			read2Idx = i
		}
	}

	if writeIdx == -1 || read2Idx == -1 {
		t.Fatalf("Missing operations: %v", order)
	}

	if writeIdx > read2Idx {
		t.Errorf("Writer should execute before second reader (writer-preference). Order: %v", order)
	}
}
