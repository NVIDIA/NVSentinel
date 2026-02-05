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

// Package cache provides a thread-safe cache for GPU resources with
// read-blocking semantics during writes.
//
// The cache uses sync.RWMutex with writer-preference to ensure that
// consumers never read stale data when a provider is updating GPU states.
// When a provider calls an update method, a write lock is acquired which
// blocks all new read operations until the update completes.
package cache

import (
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

var (
	// ErrGpuNotFound is returned when a GPU is not found in the cache.
	ErrGpuNotFound = errors.New("gpu not found")

	// ErrGpuAlreadyExists is returned when trying to create a GPU that already exists.
	ErrGpuAlreadyExists = errors.New("gpu already exists")

	// ErrConflict is returned when a resource version conflict occurs during
	// optimistic concurrency control.
	ErrConflict = errors.New("resource version conflict")
)

// Cache operation names for metrics.
const (
	OpCreate          = "create"
	OpUpdate          = "update"
	OpUpdateStatus    = "update_status"
	OpUpdateCondition = "update_condition"
	OpDelete          = "delete"
	OpSet             = "set"
	// Deprecated: use OpCreate instead.
	OpRegister = "register"
	// Deprecated: use OpDelete instead.
	OpUnregister = "unregister"
)

// MetricsRecorder is an interface for recording cache operation metrics.
// This allows the cache to record metrics without depending on the metrics package.
type MetricsRecorder interface {
	RecordCacheOperation(operation string)
}

// cachedGpu holds a GPU and its metadata.
type cachedGpu struct {
	gpu             *v1alpha1.Gpu
	resourceVersion int64
	providerID      string
	lastUpdated     time.Time
}

// GpuCache is a thread-safe cache for GPU resources.
//
// The cache uses sync.RWMutex with writer-preference semantics:
//   - Multiple readers can access the cache concurrently
//   - When a writer requests the lock, new readers are blocked
//   - The writer waits for existing readers to finish, then proceeds
//   - Readers blocked during a write resume after the write completes
//
// This ensures consumers never read stale "healthy" data when a provider
// is updating to "unhealthy".
type GpuCache struct {
	mu              sync.RWMutex
	gpus            map[string]*cachedGpu
	resourceVersion int64
	broadcaster     *Broadcaster
	logger          klog.Logger
	metrics         MetricsRecorder
}

// New creates a new GpuCache instance.
//
// The metrics parameter is optional and can be nil if metrics recording
// is not needed (e.g., in tests).
func New(logger klog.Logger, metrics MetricsRecorder) *GpuCache {
	return &GpuCache{
		gpus:        make(map[string]*cachedGpu),
		broadcaster: NewBroadcaster(logger.WithName("broadcaster"), 100),
		logger:      logger.WithName("cache"),
		metrics:     metrics,
	}
}

// Broadcaster returns the watch event broadcaster.
func (c *GpuCache) Broadcaster() *Broadcaster {
	return c.broadcaster
}

// recordOperation records a cache operation metric if metrics are configured.
func (c *GpuCache) recordOperation(op string) {
	if c.metrics != nil {
		c.metrics.RecordCacheOperation(op)
	}
}

// Get retrieves a GPU by name.
//
// This method acquires a read lock and will block if a write is in progress.
// The returned GPU is a deep copy to prevent unintended modifications.
func (c *GpuCache) Get(name string) (*v1alpha1.Gpu, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cached, ok := c.gpus[name]
	if !ok {
		return nil, false
	}

	// Return a deep copy to prevent modifications
	return proto.Clone(cached.gpu).(*v1alpha1.Gpu), true
}

// List returns all GPUs in the cache.
//
// This method acquires a read lock and will block if a write is in progress.
// The returned GPUs are deep copies to prevent unintended modifications.
func (c *GpuCache) List() []*v1alpha1.Gpu {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]*v1alpha1.Gpu, 0, len(c.gpus))

	for _, cached := range c.gpus {
		result = append(result, proto.Clone(cached.gpu).(*v1alpha1.Gpu))
	}

	return result
}

// Count returns the number of GPUs in the cache.
func (c *GpuCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.gpus)
}

// ResourceVersion returns the current cache resource version.
//
// This can be used to populate ListMeta.ResourceVersion in list responses.
func (c *GpuCache) ResourceVersion() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.resourceVersion
}

// Set creates or updates a GPU in the cache.
//
// If the GPU exists, it is replaced entirely.
// If the GPU doesn't exist, it is created.
//
// This method acquires a write lock, blocking all readers.
// Returns the new resource version.
func (c *GpuCache) Set(gpu *v1alpha1.Gpu) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.resourceVersion++

	// Clone to prevent external modifications
	gpuCopy := proto.Clone(gpu).(*v1alpha1.Gpu)
	gpuCopy.ResourceVersion = c.resourceVersion

	name := gpu.GetMetadata().GetName()
	eventType := EventTypeAdded
	if _, exists := c.gpus[name]; exists {
		eventType = EventTypeModified
	}

	c.gpus[name] = &cachedGpu{
		gpu:             gpuCopy,
		resourceVersion: c.resourceVersion,
		lastUpdated:     time.Now(),
	}

	c.logger.V(1).Info("GPU set",
		"name", name,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpSet)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   eventType,
		Object: proto.Clone(gpuCopy).(*v1alpha1.Gpu),
	})

	return c.resourceVersion
}

// Create adds a new GPU to the cache.
//
// If the GPU already exists, returns (nil, ErrGpuAlreadyExists).
// If the GPU is new, returns (gpu, nil) where gpu has the assigned resource_version.
//
// This method acquires a write lock, blocking all readers.
func (c *GpuCache) Create(gpu *v1alpha1.Gpu) (*v1alpha1.Gpu, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	name := gpu.GetMetadata().GetName()

	// Check if GPU already exists
	if _, ok := c.gpus[name]; ok {
		c.logger.V(2).Info("GPU already exists", "name", name)
		return nil, ErrGpuAlreadyExists
	}

	// Create new GPU
	c.resourceVersion++

	// Clone to prevent external modifications
	gpuCopy := proto.Clone(gpu).(*v1alpha1.Gpu)
	gpuCopy.ResourceVersion = c.resourceVersion

	c.gpus[name] = &cachedGpu{
		gpu:             gpuCopy,
		resourceVersion: c.resourceVersion,
		lastUpdated:     time.Now(),
	}

	c.logger.V(1).Info("GPU created",
		"name", name,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpCreate)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeAdded,
		Object: proto.Clone(gpuCopy).(*v1alpha1.Gpu),
	})

	return proto.Clone(gpuCopy).(*v1alpha1.Gpu), nil
}

// Update replaces an existing GPU in the cache.
//
// If expectedVersion is > 0, performs optimistic concurrency check.
// If the GPU doesn't exist, returns (nil, ErrGpuNotFound).
// If version mismatch, returns (nil, ErrConflict).
//
// This method acquires a write lock, blocking all readers.
func (c *GpuCache) Update(gpu *v1alpha1.Gpu, expectedVersion int64) (*v1alpha1.Gpu, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	name := gpu.GetMetadata().GetName()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for update", "name", name)
		return nil, ErrGpuNotFound
	}

	// Check for optimistic concurrency conflict
	if expectedVersion > 0 && cached.resourceVersion != expectedVersion {
		c.logger.V(2).Info("Resource version conflict",
			"name", name,
			"expected", expectedVersion,
			"actual", cached.resourceVersion,
		)
		return nil, ErrConflict
	}

	// Update GPU
	c.resourceVersion++

	// Clone to prevent external modifications
	gpuCopy := proto.Clone(gpu).(*v1alpha1.Gpu)
	gpuCopy.ResourceVersion = c.resourceVersion

	c.gpus[name] = &cachedGpu{
		gpu:             gpuCopy,
		resourceVersion: c.resourceVersion,
		providerID:      cached.providerID, // Preserve provider ID
		lastUpdated:     time.Now(),
	}

	c.logger.V(1).Info("GPU updated",
		"name", name,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpUpdate)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeModified,
		Object: proto.Clone(gpuCopy).(*v1alpha1.Gpu),
	})

	return proto.Clone(gpuCopy).(*v1alpha1.Gpu), nil
}

// UpdateStatusWithVersion updates only the status of a GPU with optimistic concurrency.
//
// If expectedVersion is > 0, performs optimistic concurrency check.
// If the GPU doesn't exist, returns (nil, ErrGpuNotFound).
// If version mismatch, returns (nil, ErrConflict).
//
// This method acquires a write lock, blocking ALL readers until complete.
func (c *GpuCache) UpdateStatusWithVersion(name string, status *v1alpha1.GpuStatus, expectedVersion int64) (*v1alpha1.Gpu, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for status update", "name", name)
		return nil, ErrGpuNotFound
	}

	// Check for optimistic concurrency conflict
	if expectedVersion > 0 && cached.resourceVersion != expectedVersion {
		c.logger.V(2).Info("Resource version conflict",
			"name", name,
			"expected", expectedVersion,
			"actual", cached.resourceVersion,
		)
		return nil, ErrConflict
	}

	// Update status
	c.resourceVersion++
	cached.gpu.Status = status
	cached.gpu.ResourceVersion = c.resourceVersion
	cached.resourceVersion = c.resourceVersion
	cached.lastUpdated = time.Now()

	c.logger.V(1).Info("GPU status updated",
		"name", name,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpUpdateStatus)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeModified,
		Object: proto.Clone(cached.gpu).(*v1alpha1.Gpu),
	})

	return proto.Clone(cached.gpu).(*v1alpha1.Gpu), nil
}

// Delete removes a GPU from the cache.
//
// Returns ErrGpuNotFound if the GPU doesn't exist.
//
// This method acquires a write lock, blocking all readers.
func (c *GpuCache) Delete(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for delete", "name", name)
		return ErrGpuNotFound
	}

	// Store last known state for watchers
	lastState := proto.Clone(cached.gpu).(*v1alpha1.Gpu)

	// Remove from cache
	delete(c.gpus, name)

	c.logger.V(1).Info("GPU deleted",
		"name", name,
		"resourceVersion", cached.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpDelete)

	// Notify watchers with last known state
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeDeleted,
		Object: lastState,
	})

	return nil
}

// Register adds a new GPU to the cache.
//
// If the GPU already exists, this returns (false, currentVersion, nil).
// If the GPU is new, this returns (true, newVersion, nil).
//
// This method acquires a write lock, blocking all readers.
func (c *GpuCache) Register(
	name string,
	spec *v1alpha1.GpuSpec,
	initialStatus *v1alpha1.GpuStatus,
	providerID string,
) (bool, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if GPU already exists
	if cached, ok := c.gpus[name]; ok {
		c.logger.V(2).Info("GPU already registered", "name", name, "resourceVersion", cached.resourceVersion)
		return false, cached.resourceVersion, nil
	}

	// Create new GPU
	c.resourceVersion++
	gpu := &v1alpha1.Gpu{
		Metadata:        &v1alpha1.ObjectMeta{Name: name},
		Spec:            spec,
		Status:          initialStatus,
		ResourceVersion: c.resourceVersion,
	}

	c.gpus[name] = &cachedGpu{
		gpu:             gpu,
		resourceVersion: c.resourceVersion,
		providerID:      providerID,
		lastUpdated:     time.Now(),
	}

	c.logger.V(1).Info("GPU registered",
		"name", name,
		"providerID", providerID,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpRegister)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeAdded,
		Object: proto.Clone(gpu).(*v1alpha1.Gpu),
	})

	return true, c.resourceVersion, nil
}

// Unregister removes a GPU from the cache.
//
// Returns true if the GPU was found and removed, false if not found.
//
// This method acquires a write lock, blocking all readers.
func (c *GpuCache) Unregister(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for unregister", "name", name)
		return false
	}

	// Remove from cache
	delete(c.gpus, name)

	c.logger.V(1).Info("GPU unregistered",
		"name", name,
		"resourceVersion", cached.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpUnregister)

	// Notify watchers with last known state
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeDeleted,
		Object: proto.Clone(cached.gpu).(*v1alpha1.Gpu),
	})

	return true
}

// UpdateStatus replaces the entire status of a GPU.
//
// This method acquires a write lock, blocking ALL readers until complete.
// This is the critical path that prevents consumers from reading stale
// "healthy" states when a GPU is transitioning to "unhealthy".
//
// Returns the new resource version, or an error if the GPU is not found.
func (c *GpuCache) UpdateStatus(name string, status *v1alpha1.GpuStatus, providerID string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for status update", "name", name)

		return 0, ErrGpuNotFound
	}

	// Update status
	c.resourceVersion++
	cached.gpu.Status = status
	cached.gpu.ResourceVersion = c.resourceVersion
	cached.resourceVersion = c.resourceVersion
	cached.lastUpdated = time.Now()

	if providerID != "" {
		cached.providerID = providerID
	}

	c.logger.V(1).Info("GPU status updated",
		"name", name,
		"providerID", providerID,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpUpdateStatus)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeModified,
		Object: proto.Clone(cached.gpu).(*v1alpha1.Gpu),
	})

	return c.resourceVersion, nil
}

// UpdateCondition updates or adds a single condition on a GPU.
//
// If a condition with the same type exists, it is replaced.
// If no condition with that type exists, it is added.
//
// This method acquires a write lock, blocking ALL readers.
//
// Returns the new resource version, or an error if the GPU is not found.
func (c *GpuCache) UpdateCondition(name string, condition *v1alpha1.Condition, providerID string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.gpus[name]
	if !ok {
		c.logger.V(2).Info("GPU not found for condition update", "name", name)

		return 0, ErrGpuNotFound
	}

	// Ensure status exists
	if cached.gpu.Status == nil {
		cached.gpu.Status = &v1alpha1.GpuStatus{}
	}

	// Update last transition time if not set
	if condition.LastTransitionTime == nil {
		condition.LastTransitionTime = timestamppb.Now()
	}

	// Find and update existing condition, or append new one
	found := false

	for i, existing := range cached.gpu.Status.Conditions {
		if existing.Type == condition.Type {
			cached.gpu.Status.Conditions[i] = condition
			found = true

			break
		}
	}

	if !found {
		cached.gpu.Status.Conditions = append(cached.gpu.Status.Conditions, condition)
	}

	// Update version
	c.resourceVersion++
	cached.gpu.ResourceVersion = c.resourceVersion
	cached.resourceVersion = c.resourceVersion
	cached.lastUpdated = time.Now()

	if providerID != "" {
		cached.providerID = providerID
	}

	c.logger.V(1).Info("GPU condition updated",
		"name", name,
		"conditionType", condition.Type,
		"conditionStatus", condition.Status,
		"providerID", providerID,
		"resourceVersion", c.resourceVersion,
	)

	// Record metric
	c.recordOperation(OpUpdateCondition)

	// Notify watchers
	c.broadcaster.Notify(WatchEvent{
		Type:   EventTypeModified,
		Object: proto.Clone(cached.gpu).(*v1alpha1.Gpu),
	})

	return c.resourceVersion, nil
}

// Stats returns cache statistics.
type Stats struct {
	TotalGpus       int
	HealthyGpus     int
	UnhealthyGpus   int
	UnknownGpus     int
	ResourceVersion int64
}

// MarkProviderGPUsUnknown marks all GPUs registered by a specific provider
// with an Unknown status condition. This is used when a provider's heartbeat
// times out to indicate that GPU health data may be stale.
//
// This method acquires a write lock, blocking all readers.
// Returns the number of GPUs that were marked as Unknown.
func (c *GpuCache) MarkProviderGPUsUnknown(providerID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for name, cached := range c.gpus {
		if cached.providerID != providerID {
			continue
		}

		// Ensure status exists
		if cached.gpu.Status == nil {
			cached.gpu.Status = &v1alpha1.GpuStatus{}
		}

		// Update or add Ready condition to Unknown
		unknownCondition := &v1alpha1.Condition{
			Type:               "Ready",
			Status:             "Unknown",
			Message:            "Provider heartbeat timeout - GPU health status unknown",
			LastTransitionTime: timestamppb.Now(),
		}

		found := false
		for i, cond := range cached.gpu.Status.Conditions {
			if cond.Type == "Ready" {
				cached.gpu.Status.Conditions[i] = unknownCondition
				found = true
				break
			}
		}
		if !found {
			cached.gpu.Status.Conditions = append(cached.gpu.Status.Conditions, unknownCondition)
		}

		// Update version
		c.resourceVersion++
		cached.gpu.ResourceVersion = c.resourceVersion
		cached.resourceVersion = c.resourceVersion
		cached.lastUpdated = time.Now()

		c.logger.Info("Marked GPU as Unknown due to provider heartbeat timeout",
			"name", name,
			"providerID", providerID,
			"resourceVersion", c.resourceVersion,
		)

		// Notify watchers
		c.broadcaster.Notify(WatchEvent{
			Type:   EventTypeModified,
			Object: proto.Clone(cached.gpu).(*v1alpha1.Gpu),
		})

		count++
	}

	return count
}

// ListProviderGPUs returns all GPU names registered by a specific provider.
//
// This method acquires a read lock.
func (c *GpuCache) ListProviderGPUs(providerID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var names []string
	for name, cached := range c.gpus {
		if cached.providerID == providerID {
			names = append(names, name)
		}
	}
	return names
}

// GetStats returns current cache statistics.
func (c *GpuCache) GetStats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := Stats{
		TotalGpus:       len(c.gpus),
		ResourceVersion: c.resourceVersion,
	}

	for _, cached := range c.gpus {
		if cached.gpu.Status == nil || len(cached.gpu.Status.Conditions) == 0 {
			stats.UnknownGpus++

			continue
		}

		// Check Ready condition
		healthy := true

		for _, cond := range cached.gpu.Status.Conditions {
			if cond.Type == "Ready" {
				switch cond.Status {
				case "True":
					// healthy
				case "False":
					healthy = false
				default:
					stats.UnknownGpus++
					healthy = false
				}

				break
			}
		}

		if healthy {
			stats.HealthyGpus++
		} else {
			stats.UnhealthyGpus++
		}
	}

	return stats
}
