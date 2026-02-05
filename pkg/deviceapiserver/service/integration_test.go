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

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/cache"
)

// TestIntegration_FullFlow tests the complete Create -> Watch -> Update -> Delete flow:
// 1. Create GPU
// 2. Watch receives ADDED event
// 3. Update status
// 4. Watch receives MODIFIED event
// 5. Delete GPU
// 6. Watch receives DELETED event
func TestIntegration_FullFlow(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	// Start watching before any GPUs exist
	stream := newMockWatchStream()
	var watchErr error
	watchDone := make(chan struct{})
	go func() {
		watchErr = gpuService.WatchGpus(&v1alpha1.WatchGpusRequest{}, stream)
		close(watchDone)
	}()

	// Give watch time to start
	time.Sleep(50 * time.Millisecond)

	// Step 1: Create GPU
	t.Log("Step 1: Create GPU")
	createResp, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"},
			Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-1234"},
		},
	})
	if err != nil {
		t.Fatalf("CreateGpu failed: %v", err)
	}
	if !createResp.Created {
		t.Error("Expected Created=true")
	}

	// Wait for event
	time.Sleep(50 * time.Millisecond)

	// Step 2: Verify watch received ADDED event
	t.Log("Step 2: Verify watch received ADDED event")
	events := stream.getEvents()
	if len(events) != 1 {
		t.Errorf("Expected 1 event (ADDED), got %d", len(events))
	} else if events[0].Type != cache.EventTypeAdded {
		t.Errorf("Expected ADDED event, got %s", events[0].Type)
	} else if events[0].Object.GetMetadata().GetName() != "gpu-0" {
		t.Errorf("Expected gpu-0, got %s", events[0].Object.GetMetadata().GetName())
	}

	// Step 3: Update status
	t.Log("Step 3: Update status")
	_, err = gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
		Name: "gpu-0",
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{Type: "Ready", Status: "True", Message: "GPU is ready"},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateGpuStatus failed: %v", err)
	}

	// Wait for event
	time.Sleep(50 * time.Millisecond)

	// Step 4: Verify watch received MODIFIED event
	t.Log("Step 4: Verify watch received MODIFIED event")
	events = stream.getEvents()
	if len(events) != 2 {
		t.Errorf("Expected 2 events (ADDED + MODIFIED), got %d", len(events))
	} else if events[1].Type != cache.EventTypeModified {
		t.Errorf("Expected MODIFIED event, got %s", events[1].Type)
	}

	// Verify condition through GetGpu
	getResp, err := gpuService.GetGpu(ctx, &v1alpha1.GetGpuRequest{Name: "gpu-0"})
	if err != nil {
		t.Fatalf("GetGpu failed: %v", err)
	}
	if len(getResp.Gpu.Status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(getResp.Gpu.Status.Conditions))
	} else if getResp.Gpu.Status.Conditions[0].Status != "True" {
		t.Errorf("Expected status=True, got %s", getResp.Gpu.Status.Conditions[0].Status)
	}

	// Step 5: Delete GPU
	t.Log("Step 5: Delete GPU")
	_, err = gpuService.DeleteGpu(ctx, &v1alpha1.DeleteGpuRequest{Name: "gpu-0"})
	if err != nil {
		t.Fatalf("DeleteGpu failed: %v", err)
	}

	// Wait for event
	time.Sleep(50 * time.Millisecond)

	// Step 6: Verify watch received DELETED event
	t.Log("Step 6: Verify watch received DELETED event")
	events = stream.getEvents()
	if len(events) != 3 {
		t.Errorf("Expected 3 events (ADDED + MODIFIED + DELETED), got %d", len(events))
	} else if events[2].Type != cache.EventTypeDeleted {
		t.Errorf("Expected DELETED event, got %s", events[2].Type)
	}

	// Verify GPU is gone via ListGpus
	listResp, err := gpuService.ListGpus(ctx, &v1alpha1.ListGpusRequest{})
	if err != nil {
		t.Fatalf("ListGpus failed: %v", err)
	}
	if len(listResp.GpuList.Items) != 0 {
		t.Errorf("Expected 0 GPUs, got %d", len(listResp.GpuList.Items))
	}

	// Cleanup
	stream.cancel()
	<-watchDone

	if watchErr != nil {
		t.Errorf("Watch error: %v", watchErr)
	}
}

// TestIntegration_IdempotentCreate tests that CreateGpu is idempotent
func TestIntegration_IdempotentCreate(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	// First create
	createResp1, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"},
			Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-1234"},
		},
	})
	if err != nil {
		t.Fatalf("First CreateGpu failed: %v", err)
	}
	if !createResp1.Created {
		t.Error("Expected Created=true for first create")
	}

	// Second create (should return existing without error)
	createResp2, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"},
			Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-1234"},
		},
	})
	if err != nil {
		t.Fatalf("Second CreateGpu failed: %v", err)
	}
	if createResp2.Created {
		t.Error("Expected Created=false for second create")
	}

	// Verify we still have exactly one GPU
	listResp, err := gpuService.ListGpus(ctx, &v1alpha1.ListGpusRequest{})
	if err != nil {
		t.Fatalf("ListGpus failed: %v", err)
	}
	if len(listResp.GpuList.Items) != 1 {
		t.Errorf("Expected 1 GPU, got %d", len(listResp.GpuList.Items))
	}
}

// TestIntegration_ConcurrentReadersAndWriters tests concurrent access patterns
func TestIntegration_ConcurrentReadersAndWriters(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	// Create some GPUs
	for i := 0; i < 5; i++ {
		_, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
			Gpu: &v1alpha1.Gpu{
				Metadata: &v1alpha1.ObjectMeta{Name: "gpu-" + string(rune('0'+i))},
				Spec:     &v1alpha1.GpuSpec{Uuid: "GPU"},
			},
		})
		if err != nil {
			t.Fatalf("CreateGpu failed: %v", err)
		}
	}

	var wg sync.WaitGroup
	errors := make(chan error, 200)

	// Start multiple concurrent writers updating status
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				gpuName := "gpu-" + string(rune('0'+(i%5)))
				_, err := gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
					Name: gpuName,
					Status: &v1alpha1.GpuStatus{
						Conditions: []*v1alpha1.Condition{
							{Type: "TestCondition", Status: "True"},
						},
					},
				})
				if err != nil {
					errors <- err
				}
			}
		}()
	}

	// Start multiple concurrent readers
	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := gpuService.ListGpus(ctx, &v1alpha1.ListGpusRequest{})
				if err != nil {
					errors <- err
				}
				gpuName := "gpu-" + string(rune('0'+(i%5)))
				_, _ = gpuService.GetGpu(ctx, &v1alpha1.GetGpuRequest{Name: gpuName})
			}
		}()
	}

	wg.Wait()
	close(errors)

	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent operation error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Errorf("Total errors: %d", errorCount)
	}
}

// TestIntegration_WatchWithHighUpdateRate tests watch under high update rates
func TestIntegration_WatchWithHighUpdateRate(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	// Create a GPU
	_, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"},
			Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-0"},
		},
	})
	if err != nil {
		t.Fatalf("CreateGpu failed: %v", err)
	}

	// Start watching
	stream := newMockWatchStream()
	watchDone := make(chan struct{})
	go func() {
		_ = gpuService.WatchGpus(&v1alpha1.WatchGpusRequest{}, stream)
		close(watchDone)
	}()

	// Wait for watch to start
	time.Sleep(50 * time.Millisecond)

	// Send rapid updates
	const numUpdates = 100
	for i := 0; i < numUpdates; i++ {
		_, err := gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
			Name: "gpu-0",
			Status: &v1alpha1.GpuStatus{
				Conditions: []*v1alpha1.Condition{
					{Type: "TestCondition", Status: "True", Message: "Update " + string(rune('0'+i%10))},
				},
			},
		})
		if err != nil {
			t.Fatalf("UpdateGpuStatus failed at %d: %v", i, err)
		}
	}

	// Wait for events to propagate
	time.Sleep(100 * time.Millisecond)

	// Cancel watch
	stream.cancel()
	<-watchDone

	events := stream.getEvents()

	// Should have at least 1 initial ADDED + some MODIFIED events
	// Note: Some events may be dropped if buffer is full (expected behavior)
	if len(events) < 2 {
		t.Errorf("Expected at least 2 events, got %d", len(events))
	}

	t.Logf("Received %d events out of %d updates", len(events)-1, numUpdates)
}

// TestIntegration_MultipleGPUs tests managing multiple GPUs
func TestIntegration_MultipleGPUs(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	const numGPUs = 8 // Typical 8-GPU node

	// Create all GPUs
	t.Logf("Creating %d GPUs", numGPUs)
	for i := 0; i < numGPUs; i++ {
		name := "gpu-" + string(rune('0'+i))
		_, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
			Gpu: &v1alpha1.Gpu{
				Metadata: &v1alpha1.ObjectMeta{Name: name},
				Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-UUID-" + string(rune('0'+i))},
				Status: &v1alpha1.GpuStatus{
					Conditions: []*v1alpha1.Condition{
						{Type: "Ready", Status: "True"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("CreateGpu %s failed: %v", name, err)
		}
	}

	// Verify via ListGpus
	resp, err := gpuService.ListGpus(ctx, &v1alpha1.ListGpusRequest{})
	if err != nil {
		t.Fatalf("ListGpus failed: %v", err)
	}
	if len(resp.GpuList.Items) != numGPUs {
		t.Errorf("Expected %d GPUs, got %d", numGPUs, len(resp.GpuList.Items))
	}

	// Mark one GPU as unhealthy (simulating XID error)
	t.Log("Marking gpu-3 as unhealthy (XID error)")
	_, err = gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
		Name: "gpu-3",
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{Type: "Ready", Status: "False", Message: "XID 79 - GPU has fallen off the bus"},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateGpuStatus failed: %v", err)
	}

	// Verify mixed health states
	healthyCount := 0
	unhealthyCount := 0
	for i := 0; i < numGPUs; i++ {
		name := "gpu-" + string(rune('0'+i))
		gpu, _ := gpuCache.Get(name)
		for _, c := range gpu.Status.Conditions {
			if c.Type == "Ready" {
				if c.Status == "True" {
					healthyCount++
				} else {
					unhealthyCount++
				}
			}
		}
	}

	if healthyCount != numGPUs-1 {
		t.Errorf("Expected %d healthy GPUs, got %d", numGPUs-1, healthyCount)
	}
	if unhealthyCount != 1 {
		t.Errorf("Expected 1 unhealthy GPU, got %d", unhealthyCount)
	}

	t.Logf("Multi-GPU test completed: %d healthy, %d unhealthy", healthyCount, unhealthyCount)
}

// TestIntegration_OptimisticConcurrency tests resource version conflict detection
func TestIntegration_OptimisticConcurrency(t *testing.T) {
	logger := klog.Background()
	gpuCache := cache.New(logger, nil)
	gpuService := NewGpuService(gpuCache)
	ctx := context.Background()

	// Create a GPU
	createResp, err := gpuService.CreateGpu(ctx, &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"},
			Spec:     &v1alpha1.GpuSpec{Uuid: "GPU-1234"},
		},
	})
	if err != nil {
		t.Fatalf("CreateGpu failed: %v", err)
	}

	initialVersion := createResp.Gpu.ResourceVersion

	// Update status with correct version
	updateResp, err := gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
		Name:            "gpu-0",
		ResourceVersion: initialVersion,
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{Type: "Ready", Status: "True"},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateGpuStatus with correct version failed: %v", err)
	}

	// Try to update with stale version (should fail)
	_, err = gpuService.UpdateGpuStatus(ctx, &v1alpha1.UpdateGpuStatusRequest{
		Name:            "gpu-0",
		ResourceVersion: initialVersion, // This is now stale
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{Type: "Ready", Status: "False"},
			},
		},
	})
	if err == nil {
		t.Error("Expected conflict error with stale version, got nil")
	}

	// Verify the status was not changed by the conflicting update
	getResp, err := gpuService.GetGpu(ctx, &v1alpha1.GetGpuRequest{Name: "gpu-0"})
	if err != nil {
		t.Fatalf("GetGpu failed: %v", err)
	}
	if getResp.Gpu.Status.Conditions[0].Status != "True" {
		t.Errorf("Expected status=True (unchanged), got %s", getResp.Gpu.Status.Conditions[0].Status)
	}
	if getResp.Gpu.ResourceVersion != updateResp.Gpu.ResourceVersion {
		t.Errorf("Version should not have changed")
	}
}

// mockWatchStream implements grpc.ServerStreamingServer for testing
type mockWatchStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	events []*v1alpha1.WatchGpusResponse
	mu     sync.Mutex
}

func newMockWatchStream() *mockWatchStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &mockWatchStream{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *mockWatchStream) Send(resp *v1alpha1.WatchGpusResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, resp)
	return nil
}

func (m *mockWatchStream) Context() context.Context {
	return m.ctx
}

func (m *mockWatchStream) getEvents() []*v1alpha1.WatchGpusResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*v1alpha1.WatchGpusResponse, len(m.events))
	copy(result, m.events)
	return result
}

// These methods are required by the grpc.ServerStreamingServer interface
func (m *mockWatchStream) SetHeader(md metadata.MD) error  { return nil }
func (m *mockWatchStream) SendHeader(md metadata.MD) error { return nil }
func (m *mockWatchStream) SetTrailer(md metadata.MD)       {}
func (m *mockWatchStream) SendMsg(msg interface{}) error   { return nil }
func (m *mockWatchStream) RecvMsg(msg interface{}) error   { return nil }
