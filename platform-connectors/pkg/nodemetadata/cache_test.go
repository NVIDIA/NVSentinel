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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Test constants for consistent configuration across tests
const (
	testCacheSize     = 10
	testSmallCache    = 3
	testLargeCache    = 100
	testShortTTL      = 200 * time.Millisecond
	testLongTTL       = 1 * time.Hour
	testTTLBuffer     = 100 * time.Millisecond
	testConcurrentOps = 100
)

// mockFetchFunc creates a simple fetch function for testing.
func mockFetchFunc(t *testing.T) func(context.Context, string) (*NodeMetadata, error) {
	t.Helper()
	return func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return newTestMetadata(t, nodeName), nil
	}
}

// ============================================================================
// Test Helpers
// ============================================================================

// newTestMetadata creates a test NodeMetadata with consistent structure.
// Using t.Helper() ensures test failures point to the calling line.
func newTestMetadata(t *testing.T, nodeName string) *NodeMetadata {
	t.Helper()
		return &NodeMetadata{
			ProviderID: fmt.Sprintf("provider-%s", nodeName),
		Labels:     map[string]string{"test": "true", "node": nodeName},
	}
}

// populateCache adds multiple nodes to the cache for testing.
func populateCache(t *testing.T, cache *Cache, nodeNames ...string) {
	t.Helper()
	for _, nodeName := range nodeNames {
		cache.Add(nodeName, newTestMetadata(t, nodeName))
	}
}

// assertCacheContains verifies a node exists in cache with correct data.
func assertCacheContains(t *testing.T, cache *Cache, nodeName string) {
	t.Helper()
	metadata, found := cache.Get(nodeName)
	assert.True(t, found, "expected node %s to be in cache", nodeName)
	assert.NotNil(t, metadata, "metadata should not be nil")
	assert.Equal(t, fmt.Sprintf("provider-%s", nodeName), metadata.ProviderID)
}

// assertCacheNotContains verifies a node does not exist in cache.
func assertCacheNotContains(t *testing.T, cache *Cache, nodeName string) {
	t.Helper()
	_, found := cache.Get(nodeName)
	assert.False(t, found, "expected node %s to not be in cache", nodeName)
}

// ============================================================================
// Table-Driven Tests - Basic Operations
// ============================================================================

// TestCache_Operations validates basic cache operations using table-driven tests.
func TestCache_Operations(t *testing.T) {
	tests := []struct {
		name          string
		cacheSize     int
		ttl           time.Duration
		setup         func(*testing.T, *Cache)
		operation     string
		verify        func(*testing.T, *Cache)
	}{
		{
			name:      "Get_EmptyCache_ReturnsFalse",
			cacheSize: testCacheSize,
			ttl:       testLongTTL,
			setup:     func(t *testing.T, c *Cache) {},
			operation: "Get on empty cache",
			verify: func(t *testing.T, c *Cache) {
				assertCacheNotContains(t, c, "node1")
				assert.Equal(t, 0, c.Len(), "empty cache should have length 0")
			},
		},
		{
			name:      "Add_NewEntry_Success",
			cacheSize: testCacheSize,
			ttl:       testLongTTL,
			setup: func(t *testing.T, c *Cache) {
				c.Add("node1", newTestMetadata(t, "node1"))
			},
			operation: "Add and retrieve entry",
			verify: func(t *testing.T, c *Cache) {
				assertCacheContains(t, c, "node1")
				assert.Equal(t, 1, c.Len(), "cache should have length 1")
			},
		},
		{
			name:      "Update_ExistingEntry_Success",
			cacheSize: testCacheSize,
			ttl:       testLongTTL,
			setup: func(t *testing.T, c *Cache) {
				c.Add("node1", &NodeMetadata{ProviderID: "old-provider"})
				c.Add("node1", &NodeMetadata{ProviderID: "new-provider"})
			},
			operation: "Update existing entry",
			verify: func(t *testing.T, c *Cache) {
				metadata, found := c.Get("node1")
				assert.True(t, found)
				assert.Equal(t, "new-provider", metadata.ProviderID)
				assert.Equal(t, 1, c.Len(), "cache size should remain 1 after update")
			},
		},
		{
			name:      "Remove_ExistingEntry_Success",
			cacheSize: testCacheSize,
			ttl:       testLongTTL,
			setup: func(t *testing.T, c *Cache) {
				c.Add("node1", newTestMetadata(t, "node1"))
				c.Remove("node1")
			},
			operation: "Remove entry",
			verify: func(t *testing.T, c *Cache) {
				assertCacheNotContains(t, c, "node1")
				assert.Equal(t, 0, c.Len(), "cache should be empty after remove")
			},
		},
		{
			name:      "Clear_MultipleEntries_Success",
			cacheSize: testCacheSize,
			ttl:       testLongTTL,
			setup: func(t *testing.T, c *Cache) {
				populateCache(t, c, "node1", "node2", "node3")
				c.Clear()
			},
			operation: "Clear all entries",
			verify: func(t *testing.T, c *Cache) {
				assert.Equal(t, 0, c.Len(), "cache should be empty after clear")
				assertCacheNotContains(t, c, "node1")
				assertCacheNotContains(t, c, "node2")
				assertCacheNotContains(t, c, "node3")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewCache(tt.cacheSize, tt.ttl, mockFetchFunc(t))
			tt.setup(t, cache)
			tt.verify(t, cache)
		})
	}
}

// ============================================================================
// LRU Eviction Tests
// ============================================================================

// TestCache_LRU_Eviction validates that the cache evicts least recently used entries
// when the cache reaches its maximum size.
func TestCache_LRU_Eviction(t *testing.T) {
	cache := NewCache(testSmallCache, testLongTTL, mockFetchFunc(t)) // Max size = 3

	// Fill cache to capacity
	populateCache(t, cache, "node1", "node2", "node3")

	// Verify all entries are present
	assert.Equal(t, testSmallCache, cache.Len(), "cache should be at max capacity")
	assertCacheContains(t, cache, "node1")
	assertCacheContains(t, cache, "node2")
	assertCacheContains(t, cache, "node3")

	// Add 4th entry - should evict oldest (node1)
	cache.Add("node4", newTestMetadata(t, "node4"))

	// Verify LRU eviction
	assertCacheNotContains(t, cache, "node1")
	assertCacheContains(t, cache, "node2")
	assertCacheContains(t, cache, "node3")
	assertCacheContains(t, cache, "node4")
	assert.Equal(t, testSmallCache, cache.Len(), "cache should maintain max size")
}

// ============================================================================
// TTL Expiration Tests
// ============================================================================

// TestCache_TTL_Expiration validates that cache entries expire after their TTL.
func TestCache_TTL_Expiration(t *testing.T) {
	cache := NewCache(testCacheSize, testShortTTL, mockFetchFunc(t))
	cache.Add("node1", newTestMetadata(t, "node1"))

	// Entry should be valid immediately
	assertCacheContains(t, cache, "node1")

	// Wait for TTL to expire
	time.Sleep(testShortTTL + testTTLBuffer)

	// Entry should be expired
	assertCacheNotContains(t, cache, "node1")
}

// TestCache_TTL_MultipleEntries validates TTL behavior with multiple entries
// added at different times.
func TestCache_TTL_MultipleEntries(t *testing.T) {
	cache := NewCache(testCacheSize, testShortTTL, mockFetchFunc(t))

	// Add entries at different times to create staggered expiration
	cache.Add("node1", newTestMetadata(t, "node1"))
	time.Sleep(100 * time.Millisecond)
	cache.Add("node2", newTestMetadata(t, "node2"))
	time.Sleep(100 * time.Millisecond)
	cache.Add("node3", newTestMetadata(t, "node3"))

	// At this point:
	// - node1 is ~200ms old (about to expire)
	// - node2 is ~100ms old (will expire soon)
	// - node3 is ~0ms old (freshly added)

	// Wait for node1 to expire
	time.Sleep(50 * time.Millisecond)

	// Verify expiration behavior
	assertCacheNotContains(t, cache, "node1")
	assertCacheContains(t, cache, "node2")
	assertCacheContains(t, cache, "node3")
}

// ============================================================================
// Edge Case Tests
// ============================================================================

// TestCache_ZeroSize validates cache behavior with size 0.
// The library should handle this gracefully without panicking.
func TestCache_ZeroSize(t *testing.T) {
	cache := NewCache(0, testLongTTL, mockFetchFunc(t))

	// Operations should not panic
	assert.NotPanics(t, func() {
		cache.Add("node1", newTestMetadata(t, "node1"))
		cache.Get("node1")
		cache.Remove("node1")
		cache.Clear()
	}, "cache with size 0 should not panic")
}

// ============================================================================
// Concurrency Tests
// ============================================================================

// TestCache_ConcurrentAccess validates thread-safety with concurrent reads and writes.
func TestCache_ConcurrentAccess(t *testing.T) {
	cache := NewCache(testLargeCache, testLongTTL, mockFetchFunc(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel) // Ensures cleanup even if test fails

	var wg sync.WaitGroup
	wg.Add(2)

	// Concurrent writer
		go func() {
			defer wg.Done()
		for i := 0; i < testConcurrentOps; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				nodeName := fmt.Sprintf("node%d", i)
				cache.Add(nodeName, newTestMetadata(t, nodeName))
			}
		}
	}()

	// Concurrent reader
	go func() {
		defer wg.Done()
		for i := 0; i < testConcurrentOps; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				nodeName := fmt.Sprintf("node%d", i)
				cache.Get(nodeName)
			}
		}
	}()

	wg.Wait()

	// Verify cache is in valid state
	assert.LessOrEqual(t, cache.Len(), testLargeCache, "cache size should not exceed max")
}

// TestCache_ConcurrentSameKey validates that concurrent operations on the same key
// are handled safely without data races.
func TestCache_ConcurrentSameKey(t *testing.T) {
	cache := NewCache(testCacheSize, testLongTTL, mockFetchFunc(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Multiple goroutines accessing same key
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				select {
				case <-ctx.Done():
					return
				default:
					// Mix of Add and Get operations
					if j%2 == 0 {
						cache.Add("shared-node", &NodeMetadata{
							ProviderID: fmt.Sprintf("provider-%d-%d", id, j),
						})
					} else {
						cache.Get("shared-node")
					}
				}
			}
		}(i)
	}

	wg.Wait()

	// Cache should still be in valid state
	assert.LessOrEqual(t, cache.Len(), testCacheSize)
}

// ============================================================================
// GetOrFetch Tests (Double-Check Pattern)
// ============================================================================

// TestCache_GetOrFetch_CacheHit validates that GetOrFetch returns cached data without calling fetch.
func TestCache_GetOrFetch_CacheHit(t *testing.T) {
	fetchCalls := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		fetchCalls++
		return newTestMetadata(t, nodeName), nil
	}
	
	cache := NewCache(testCacheSize, testLongTTL, fetchFunc)
	
	// Pre-populate cache
	cache.Add("node1", newTestMetadata(t, "node1"))
	
	// GetOrFetch should return cached value without calling fetch
	metadata, err := cache.GetOrFetch(context.Background(), "node1")
	
	assert.NoError(t, err)
	assert.NotNil(t, metadata)
	assert.Equal(t, 0, fetchCalls, "fetch should not be called for cache hit")
}

// TestCache_GetOrFetch_CacheMiss validates that GetOrFetch fetches and caches on miss.
func TestCache_GetOrFetch_CacheMiss(t *testing.T) {
	fetchCalls := 0
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		fetchCalls++
		return newTestMetadata(t, nodeName), nil
	}
	
	cache := NewCache(testCacheSize, testLongTTL, fetchFunc)
	
	// GetOrFetch should fetch from source
	metadata, err := cache.GetOrFetch(context.Background(), "node1")
	
	assert.NoError(t, err)
	assert.NotNil(t, metadata)
	assert.Equal(t, 1, fetchCalls, "fetch should be called once for cache miss")
	
	// Verify it was cached
	cachedMetadata, found := cache.Get("node1")
	assert.True(t, found)
	assert.Equal(t, metadata, cachedMetadata)
}

// TestCache_GetOrFetch_ConcurrentSameKey validates that concurrent GetOrFetch
// for the same key only calls fetch once (double-check pattern).
func TestCache_GetOrFetch_ConcurrentSameKey(t *testing.T) {
	var fetchCalls int
	var fetchMu sync.Mutex
	
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		fetchMu.Lock()
		fetchCalls++
		fetchMu.Unlock()
		
		// Simulate slow fetch
		time.Sleep(10 * time.Millisecond)
		return newTestMetadata(t, nodeName), nil
	}
	
	cache := NewCache(testCacheSize, testLongTTL, fetchFunc)
	
	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	
	// Launch concurrent GetOrFetch for same key
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			metadata, err := cache.GetOrFetch(context.Background(), "node1")
			assert.NoError(t, err)
			assert.NotNil(t, metadata)
		}()
	}
	
	wg.Wait()
	
	// With double-check pattern, only first goroutine fetches
	// All others wait for lock and hit cache on second check
	assert.Equal(t, 1, fetchCalls, "double-check pattern should result in exactly 1 fetch")
}

// TestCache_GetOrFetch_ContextCancellation validates context cancellation handling.
func TestCache_GetOrFetch_ContextCancellation(t *testing.T) {
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return newTestMetadata(t, nodeName), nil
		}
	}
	
	cache := NewCache(testCacheSize, testLongTTL, fetchFunc)
	
	// Create cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	
	// GetOrFetch should respect cancellation
	_, err := cache.GetOrFetch(ctx, "node1")
	
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestCache_GetOrFetch_FetchError validates error handling from fetch function.
func TestCache_GetOrFetch_FetchError(t *testing.T) {
	expectedErr := fmt.Errorf("fetch failed")
	fetchFunc := func(ctx context.Context, nodeName string) (*NodeMetadata, error) {
		return nil, expectedErr
	}
	
	cache := NewCache(testCacheSize, testLongTTL, fetchFunc)
	
	// GetOrFetch should return fetch error
	metadata, err := cache.GetOrFetch(context.Background(), "node1")
	
	assert.Error(t, err)
	assert.Nil(t, metadata)
	assert.Equal(t, expectedErr, err)
	
	// Error should not be cached
	_, found := cache.Get("node1")
	assert.False(t, found, "errors should not be cached")
}
