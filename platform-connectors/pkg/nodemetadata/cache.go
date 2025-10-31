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
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Cache wraps hashicorp/golang-lru for node metadata storage with coordinated fetching.
type Cache struct {
	lru      *expirable.LRU[string, *NodeMetadata]
	fetchMu  sync.Mutex
	fetchFn  func(context.Context, string) (*NodeMetadata, error)
}

// NewCache creates a new LRU cache with the given size and TTL.
func NewCache(maxSize int, ttl time.Duration, fetchFn func(context.Context, string) (*NodeMetadata, error)) *Cache {
	lru := expirable.NewLRU[string, *NodeMetadata](
		maxSize,
		nil,
		ttl,
	)

	return &Cache{
		lru:     lru,
		fetchFn: fetchFn,
	}
}

// Get retrieves metadata from cache.
func (c *Cache) Get(nodeName string) (*NodeMetadata, bool) {
	return c.lru.Get(nodeName)
}

// GetOrFetch retrieves metadata from cache or fetches it if not present.
// Uses double-check pattern to prevent duplicate fetches.
func (c *Cache) GetOrFetch(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	// Fast path: check cache without lock
	if metadata, found := c.lru.Get(nodeName); found {
		return metadata, nil
	}

	// Slow path: acquire lock and check again
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()

	// Double-check: another goroutine might have fetched while we waited
	if metadata, found := c.lru.Get(nodeName); found {
		return metadata, nil
	}

	// Cache miss: fetch from source
	metadata, err := c.fetchFn(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	// Store in cache
	c.lru.Add(nodeName, metadata)
	return metadata, nil
}

// Add adds or updates a cache entry.
func (c *Cache) Add(nodeName string, metadata *NodeMetadata) {
	c.lru.Add(nodeName, metadata)
}

// Remove removes an entry from the cache.
func (c *Cache) Remove(nodeName string) {
	c.lru.Remove(nodeName)
}

// Clear removes all entries from the cache.
func (c *Cache) Clear() {
	c.lru.Purge()
}

// Len returns the number of entries in the cache.
func (c *Cache) Len() int {
	return c.lru.Len()
}
