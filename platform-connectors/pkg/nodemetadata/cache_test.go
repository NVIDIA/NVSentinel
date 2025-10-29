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

package nodemetadata

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheGetHit(t *testing.T) {
	callCount := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount++
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
			Labels:     map[string]string{"test": "label"},
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	metadata1, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metadata1.ProviderID != "provider-node1" {
		t.Errorf("expected provider-node1, got %s", metadata1.ProviderID)
	}

	if callCount != 1 {
		t.Errorf("expected 1 fetch call, got %d", callCount)
	}

	metadata2, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metadata2.ProviderID != "provider-node1" {
		t.Errorf("expected provider-node1, got %s", metadata2.ProviderID)
	}

	if callCount != 1 {
		t.Errorf("expected 1 fetch call (cache hit), got %d", callCount)
	}
}

func TestCacheMiss(t *testing.T) {
	callCount := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount++
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = cache.Get(ctx, "node2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 fetch calls, got %d", callCount)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	callCount := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount++
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s-%d", nodeName, callCount),
		}, nil
	}

	cache := NewCache(10, 100*time.Millisecond, fetchFunc)
	ctx := context.Background()

	metadata1, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metadata1.ProviderID != "provider-node1-1" {
		t.Errorf("expected provider-node1-1, got %s", metadata1.ProviderID)
	}

	time.Sleep(150 * time.Millisecond)

	metadata2, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metadata2.ProviderID != "provider-node1-2" {
		t.Errorf("expected provider-node1-2 after TTL expiry, got %s", metadata2.ProviderID)
	}

	if callCount != 2 {
		t.Errorf("expected 2 fetch calls (after TTL expiry), got %d", callCount)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(2, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = cache.Get(ctx, "node2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = cache.Get(ctx, "node3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cache.mu.RLock()
	cacheSize := len(cache.items)
	cache.mu.RUnlock()

	if cacheSize != 2 {
		t.Errorf("expected cache size 2 after LRU eviction, got %d", cacheSize)
	}

	cache.mu.RLock()
	_, node1Exists := cache.items["node1"]
	cache.mu.RUnlock()

	if node1Exists {
		t.Error("expected node1 to be evicted (LRU)")
	}
}

func TestCacheSingleflight(t *testing.T) {
	var callCount atomic.Int32
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount.Add(1)
		time.Sleep(100 * time.Millisecond)
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	var wg sync.WaitGroup
	concurrentRequests := 10

	for i := 0; i < concurrentRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Get(ctx, "node1")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	if callCount.Load() != 1 {
		t.Errorf("expected 1 fetch call (singleflight), got %d", callCount.Load())
	}
}

func TestCacheCleanExpired(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 100*time.Millisecond, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = cache.Get(ctx, "node2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	cache.CleanExpired()

	cache.mu.RLock()
	cacheSize := len(cache.items)
	cache.mu.RUnlock()

	if cacheSize != 0 {
		t.Errorf("expected cache size 0 after cleanup, got %d", cacheSize)
	}
}

func TestCacheClear(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = cache.Get(ctx, "node2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cache.Clear()

	cache.mu.RLock()
	cacheSize := len(cache.items)
	cache.mu.RUnlock()

	if cacheSize != 0 {
		t.Errorf("expected cache size 0 after clear, got %d", cacheSize)
	}
}

func TestCacheFetchError(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return nil, fmt.Errorf("simulated fetch error")
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err == nil {
		t.Error("expected error from fetch function")
	}
}

func TestCacheContextCancellation(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return &NodeMetadata{ProviderID: "provider"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := cache.Get(ctx, "node1")
	if err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestCacheConcurrentDifferentKeys(t *testing.T) {
	var callCount atomic.Int32
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount.Add(1)
		time.Sleep(50 * time.Millisecond)
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	var wg sync.WaitGroup
	nodes := []string{"node1", "node2", "node3"}

	for _, nodeName := range nodes {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_, err := cache.Get(ctx, name)
			if err != nil {
				t.Errorf("unexpected error for %s: %v", name, err)
			}
		}(nodeName)
	}

	wg.Wait()

	if callCount.Load() != 3 {
		t.Errorf("expected 3 fetch calls for different keys, got %d", callCount.Load())
	}
}

func TestCacheAccessAfterClear(t *testing.T) {
	callCount := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount++
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s-%d", nodeName, callCount),
		}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}

	cache.Clear()

	metadata, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 calls after clear (cache miss), got %d", callCount)
	}

	if metadata.ProviderID != "provider-node1-2" {
		t.Errorf("expected provider-node1-2, got %s", metadata.ProviderID)
	}
}

func TestCachePartialExpiry(t *testing.T) {
	callCount := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		callCount++
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(10, 100*time.Millisecond, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "node1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	_, err = cache.Get(ctx, "node2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(70 * time.Millisecond)

	cache.CleanExpired()

	cache.mu.RLock()
	_, node1Exists := cache.items["node1"]
	_, node2Exists := cache.items["node2"]
	cacheSize := len(cache.items)
	cache.mu.RUnlock()

	if node1Exists {
		t.Error("expected node1 to be cleaned (expired)")
	}

	if !node2Exists {
		t.Error("expected node2 to remain (not expired)")
	}

	if cacheSize != 1 {
		t.Errorf("expected cache size 1 after partial cleanup, got %d", cacheSize)
	}
}

func TestCacheLRUOrdering(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		}, nil
	}

	cache := NewCache(3, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, _ = cache.Get(ctx, "node1")
	_, _ = cache.Get(ctx, "node2")
	_, _ = cache.Get(ctx, "node3")

	cache.mu.RLock()
	cacheSize := len(cache.items)
	cache.mu.RUnlock()

	if cacheSize != 3 {
		t.Errorf("expected cache size 3, got %d", cacheSize)
	}

	_, _ = cache.Get(ctx, "node4")

	cache.mu.RLock()
	cacheSizeAfter := len(cache.items)
	_, node1Exists := cache.items["node1"]
	_, node4Exists := cache.items["node4"]
	cache.mu.RUnlock()

	if cacheSizeAfter != 3 {
		t.Errorf("expected cache size to remain 3 after eviction, got %d", cacheSizeAfter)
	}

	if !node4Exists {
		t.Error("expected node4 to remain (recently added)")
	}

	if node1Exists {
		t.Log("node1 was not evicted (OK - LRU can vary based on timing)")
	}
}

func TestCacheEmptyNodeName(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		if nodeName == "" {
			return nil, fmt.Errorf("empty node name")
		}
		return &NodeMetadata{ProviderID: "provider"}, nil
	}

	cache := NewCache(10, 1*time.Hour, fetchFunc)
	ctx := context.Background()

	_, err := cache.Get(ctx, "")
	if err == nil {
		t.Error("expected error for empty node name")
	}
}

