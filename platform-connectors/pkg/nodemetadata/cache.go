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
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// cacheEntry stores metadata with expiration timestamp and node name for efficient eviction.
type cacheEntry struct {
	nodeName  string
	metadata  *NodeMetadata
	expiresAt time.Time
}

// Cache provides thread-safe LRU caching with TTL and singleflight deduplication.
type Cache struct {
	maxSize int
	ttl     time.Duration

	mu    sync.RWMutex
	items map[string]*list.Element
	lru   *list.List

	sf singleflight.Group

	fetchFunc func(ctx context.Context, nodeName string) (*NodeMetadata, error)
}

// NewCache creates a new LRU cache with the given size and TTL.
func NewCache(maxSize int, ttl time.Duration, fetchFunc func(context.Context, string) (*NodeMetadata, error)) *Cache {
	return &Cache{
		maxSize:   maxSize,
		ttl:       ttl,
		items:     make(map[string]*list.Element),
		lru:       list.New(),
		fetchFunc: fetchFunc,
	}
}

// Get retrieves metadata from cache or fetches it using singleflight.
func (c *Cache) Get(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	if metadata := c.getFromCache(nodeName); metadata != nil {
		cacheHits.Inc()
		return metadata, nil
	}

	cacheMisses.Inc()

	// Use singleflight to deduplicate concurrent requests
	result, err, _ := c.sf.Do(nodeName, func() (interface{}, error) {
		metadata, err := c.fetchFunc(ctx, nodeName)
		if err != nil {
			return nil, err
		}

		c.put(nodeName, metadata)
		return metadata, nil
	})

	if err != nil {
		return nil, err
	}

	return result.(*NodeMetadata), nil
}

// getFromCache retrieves and validates cached entry.
func (c *Cache) getFromCache(nodeName string) *NodeMetadata {
	c.mu.RLock()
	defer c.mu.RUnlock()

	elem, exists := c.items[nodeName]
	if !exists {
		return nil
	}

	entry := elem.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		return nil
	}

	return entry.metadata
}

// put adds or updates cache entry and maintains LRU order.
func (c *Cache) put(nodeName string, metadata *NodeMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := &cacheEntry{
		nodeName:  nodeName,
		metadata:  metadata,
		expiresAt: time.Now().Add(c.ttl),
	}

	if elem, exists := c.items[nodeName]; exists {
		elem.Value = entry
		c.lru.MoveToFront(elem)
		return
	}

	if c.lru.Len() >= c.maxSize {
		c.evictOldest()
	}

	elem := c.lru.PushFront(entry)
	c.items[nodeName] = elem
	cacheSize.Set(float64(len(c.items)))
}

// evictOldest removes the least recently used entry.
func (c *Cache) evictOldest() {
	elem := c.lru.Back()
	if elem == nil {
		return
	}

	entry := elem.Value.(*cacheEntry)
	c.lru.Remove(elem)
	delete(c.items, entry.nodeName)
	cacheEvictions.Inc()
}

// CleanExpired removes expired entries from the cache.
func (c *Cache) CleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	toRemove := []string{}

	for nodeName, elem := range c.items {
		entry := elem.Value.(*cacheEntry)
		if now.After(entry.expiresAt) {
			toRemove = append(toRemove, nodeName)
		}
	}

	for _, nodeName := range toRemove {
		if elem, exists := c.items[nodeName]; exists {
			c.lru.Remove(elem)
			delete(c.items, nodeName)
			cacheEvictions.Inc()
		}
	}

	cacheSize.Set(float64(len(c.items)))
}

// Clear removes all entries from the cache.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lru.Init()
	cacheSize.Set(0)
}

