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

// Package service implements the gRPC services for the Device API Server.
package service

import (
	"context"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/cache"
)

// GpuService implements the unified GpuService API.
//
// This service provides both read and write access to GPU resources:
//   - Read operations (Get, List, Watch) for consumers like device plugins and DRA drivers
//   - Write operations (Create, Update, UpdateStatus, Delete) for providers like health monitors
//
// All read operations acquire a read lock on the cache, which will block if a
// provider is currently updating GPU state.
//
// All write operations acquire an exclusive write lock, blocking ALL consumer
// read operations until the write completes. This ensures consumers never read
// stale data during status transitions.
type GpuService struct {
	v1alpha1.UnimplementedGpuServiceServer
	cache *cache.GpuCache
}

// NewGpuService creates a new GpuService instance.
func NewGpuService(gpuCache *cache.GpuCache) *GpuService {
	return &GpuService{
		cache: gpuCache,
	}
}

// ==========================================
// Read Operations
// ==========================================

// GetGpu retrieves a single GPU resource by its unique name.
//
// This method acquires a read lock on the cache, blocking if a write is in progress.
// Returns NotFound if the GPU does not exist.
func (s *GpuService) GetGpu(ctx context.Context, req *v1alpha1.GetGpuRequest) (*v1alpha1.GetGpuResponse, error) {
	logger := klog.FromContext(ctx).WithValues("method", "GetGpu", "gpuName", req.GetName())

	// Validate request
	if req.GetName() == "" {
		logger.V(1).Info("Invalid request: empty name")

		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	// Get GPU from cache (acquires read lock, blocks during writes)
	gpu, found := s.cache.Get(req.GetName())
	if !found {
		logger.V(1).Info("GPU not found")

		return nil, status.Error(codes.NotFound, "gpu not found")
	}

	logger.V(2).Info("GPU retrieved successfully", "resourceVersion", gpu.GetResourceVersion())

	return &v1alpha1.GetGpuResponse{Gpu: gpu}, nil
}

// ListGpus retrieves a list of all GPU resources.
//
// This method acquires a read lock on the cache, blocking if a write is in progress.
// Returns an empty list if no GPUs are registered.
//
// The response includes ListMeta.ResourceVersion which can be used for
// optimistic concurrency or cache invalidation.
func (s *GpuService) ListGpus(ctx context.Context, req *v1alpha1.ListGpusRequest) (*v1alpha1.ListGpusResponse, error) {
	logger := klog.FromContext(ctx).WithValues("method", "ListGpus")

	// Get resource version first (under read lock)
	resourceVersion := s.cache.ResourceVersion()

	// List all GPUs from cache (acquires read lock, blocks during writes)
	gpus := s.cache.List()

	logger.V(2).Info("GPUs listed successfully",
		"count", len(gpus),
		"resourceVersion", resourceVersion,
	)

	return &v1alpha1.ListGpusResponse{
		GpuList: &v1alpha1.GpuList{
			Metadata: &v1alpha1.ListMeta{
				ResourceVersion: strconv.FormatInt(resourceVersion, 10),
			},
			Items: gpus,
		},
	}, nil
}

// WatchGpus streams lifecycle events for GPU resources.
//
// The stream first sends ADDED events for all existing GPUs (initial sync),
// then streams MODIFIED and DELETED events as they occur.
//
// The stream remains open until:
//   - The client disconnects
//   - The server shuts down
//   - An error occurs
func (s *GpuService) WatchGpus(req *v1alpha1.WatchGpusRequest, stream grpc.ServerStreamingServer[v1alpha1.WatchGpusResponse]) error {
	ctx := stream.Context()
	logger := klog.FromContext(ctx).WithValues("method", "WatchGpus")

	// Generate unique subscriber ID
	subscriberID := uuid.New().String()
	logger = logger.WithValues("subscriberID", subscriberID)
	logger.V(1).Info("Watch stream started")

	// Subscribe to cache events
	broadcaster := s.cache.Broadcaster()
	events := broadcaster.Subscribe(subscriberID)

	defer func() {
		broadcaster.Unsubscribe(subscriberID)
		logger.V(1).Info("Watch stream ended")
	}()

	// Send initial state (all existing GPUs as ADDED events)
	gpus := s.cache.List()

	for _, gpu := range gpus {
		if err := stream.Send(&v1alpha1.WatchGpusResponse{
			Type:   cache.EventTypeAdded,
			Object: gpu,
		}); err != nil {
			logger.Error(err, "Failed to send initial GPU state")

			return status.Error(codes.Internal, "failed to send initial state")
		}
	}

	logger.V(2).Info("Sent initial state", "gpuCount", len(gpus))

	// Stream ongoing events
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or context cancelled
			logger.V(1).Info("Watch stream context done", "reason", ctx.Err())
			return nil

		case event, ok := <-events:
			if !ok {
				// Channel closed (server shutting down)
				logger.V(1).Info("Event channel closed")
				return nil
			}

			// Send event to client
			if err := stream.Send(&v1alpha1.WatchGpusResponse{
				Type:   event.Type,
				Object: event.Object,
			}); err != nil {
				logger.Error(err, "Failed to send event",
					"eventType", event.Type,
					"gpuName", event.Object.GetMetadata().GetName(),
				)

				return status.Error(codes.Internal, "failed to send event")
			}

			logger.V(2).Info("Event sent",
				"eventType", event.Type,
				"gpuName", event.Object.GetMetadata().GetName(),
				"resourceVersion", event.Object.GetResourceVersion(),
			)
		}
	}
}

// ==========================================
// Write Operations
// ==========================================

// CreateGpu registers a new GPU with the server.
//
// The GPU name (metadata.name) must be unique. If a GPU with the
// same name already exists, returns ALREADY_EXISTS.
//
// This operation acquires a write lock, blocking consumer reads
// until the operation completes.
func (s *GpuService) CreateGpu(ctx context.Context, req *v1alpha1.CreateGpuRequest) (*v1alpha1.CreateGpuResponse, error) {
	logger := klog.FromContext(ctx).WithValues(
		"method", "CreateGpu",
		"gpuName", req.GetGpu().GetMetadata().GetName(),
	)

	// Validate request
	if req.GetGpu() == nil {
		logger.V(1).Info("Invalid request: nil gpu")
		return nil, status.Error(codes.InvalidArgument, "gpu is required")
	}

	if req.GetGpu().GetMetadata() == nil || req.GetGpu().GetMetadata().GetName() == "" {
		logger.V(1).Info("Invalid request: empty gpu name")
		return nil, status.Error(codes.InvalidArgument, "gpu.metadata.name is required")
	}

	// Create GPU (acquires write lock, blocks consumers)
	gpu, err := s.cache.Create(req.GetGpu())
	if err != nil {
		if errors.Is(err, cache.ErrGpuAlreadyExists) {
			logger.V(1).Info("GPU already exists")
			// Return the existing GPU for idempotent registration
			existing, _ := s.cache.Get(req.GetGpu().GetMetadata().GetName())
			return &v1alpha1.CreateGpuResponse{
				Gpu:     existing,
				Created: false,
			}, nil
		}

		logger.Error(err, "Failed to create GPU")
		return nil, status.Errorf(codes.Internal, "failed to create gpu: %v", err)
	}

	logger.Info("GPU created", "resourceVersion", gpu.GetResourceVersion())

	return &v1alpha1.CreateGpuResponse{
		Gpu:     gpu,
		Created: true,
	}, nil
}

// UpdateGpu replaces an existing GPU resource.
//
// The entire GPU (spec and status) is replaced. Use UpdateGpuStatus
// if you only need to update the status.
//
// Optimistic concurrency: If resource_version is provided in the GPU and does
// not match the current version, returns ABORTED.
//
// This operation acquires a write lock.
func (s *GpuService) UpdateGpu(ctx context.Context, req *v1alpha1.UpdateGpuRequest) (*v1alpha1.UpdateGpuResponse, error) {
	logger := klog.FromContext(ctx).WithValues(
		"method", "UpdateGpu",
		"gpuName", req.GetGpu().GetMetadata().GetName(),
	)

	// Validate request
	if req.GetGpu() == nil {
		logger.V(1).Info("Invalid request: nil gpu")
		return nil, status.Error(codes.InvalidArgument, "gpu is required")
	}

	if req.GetGpu().GetMetadata() == nil || req.GetGpu().GetMetadata().GetName() == "" {
		logger.V(1).Info("Invalid request: empty gpu name")
		return nil, status.Error(codes.InvalidArgument, "gpu.metadata.name is required")
	}

	// Update GPU (acquires write lock, blocks consumers)
	expectedVersion := req.GetGpu().GetResourceVersion()
	gpu, err := s.cache.Update(req.GetGpu(), expectedVersion)
	if err != nil {
		if errors.Is(err, cache.ErrGpuNotFound) {
			logger.V(1).Info("GPU not found")
			return nil, status.Error(codes.NotFound, "gpu not found")
		}
		if errors.Is(err, cache.ErrConflict) {
			logger.V(1).Info("Resource version conflict", "expectedVersion", expectedVersion)
			return nil, status.Error(codes.Aborted, "resource version conflict")
		}

		logger.Error(err, "Failed to update GPU")
		return nil, status.Errorf(codes.Internal, "failed to update gpu: %v", err)
	}

	logger.V(1).Info("GPU updated", "resourceVersion", gpu.GetResourceVersion())

	return &v1alpha1.UpdateGpuResponse{Gpu: gpu}, nil
}

// UpdateGpuStatus updates only the status of an existing GPU.
//
// This is the primary method for health monitors to report GPU state.
// The spec is not modified.
//
// IMPORTANT: This operation acquires an exclusive write lock, blocking
// ALL consumer read operations (GetGpu, ListGpus) until the update completes.
// This ensures consumers never read stale data during status transitions.
//
// Optimistic concurrency: If resource_version is provided and does not match
// the current version, returns ABORTED.
func (s *GpuService) UpdateGpuStatus(ctx context.Context, req *v1alpha1.UpdateGpuStatusRequest) (*v1alpha1.UpdateGpuStatusResponse, error) {
	logger := klog.FromContext(ctx).WithValues(
		"method", "UpdateGpuStatus",
		"gpuName", req.GetName(),
	)

	// Validate request
	if req.GetName() == "" {
		logger.V(1).Info("Invalid request: empty name")
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	if req.GetStatus() == nil {
		logger.V(1).Info("Invalid request: nil status")
		return nil, status.Error(codes.InvalidArgument, "status is required")
	}

	// Update status (acquires write lock, BLOCKS ALL CONSUMER READS)
	gpu, err := s.cache.UpdateStatusWithVersion(req.GetName(), req.GetStatus(), req.GetResourceVersion())
	if err != nil {
		if errors.Is(err, cache.ErrGpuNotFound) {
			logger.V(1).Info("GPU not found")
			return nil, status.Error(codes.NotFound, "gpu not found")
		}
		if errors.Is(err, cache.ErrConflict) {
			logger.V(1).Info("Resource version conflict", "expectedVersion", req.GetResourceVersion())
			return nil, status.Error(codes.Aborted, "resource version conflict")
		}

		logger.Error(err, "Failed to update GPU status")
		return nil, status.Errorf(codes.Internal, "failed to update status: %v", err)
	}

	logger.V(1).Info("GPU status updated", "resourceVersion", gpu.GetResourceVersion())

	return &v1alpha1.UpdateGpuStatusResponse{Gpu: gpu}, nil
}

// DeleteGpu removes a GPU from the server.
//
// After deletion, the GPU will no longer appear in ListGpus or GetGpu
// responses. Active WatchGpus streams will receive a DELETED event.
//
// This operation acquires a write lock, blocking all consumer reads.
func (s *GpuService) DeleteGpu(ctx context.Context, req *v1alpha1.DeleteGpuRequest) (*emptypb.Empty, error) {
	logger := klog.FromContext(ctx).WithValues(
		"method", "DeleteGpu",
		"gpuName", req.GetName(),
	)

	// Validate request
	if req.GetName() == "" {
		logger.V(1).Info("Invalid request: empty name")
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	// Delete GPU (acquires write lock, blocks consumers)
	err := s.cache.Delete(req.GetName())
	if err != nil {
		if errors.Is(err, cache.ErrGpuNotFound) {
			logger.V(1).Info("GPU not found")
			return nil, status.Error(codes.NotFound, "gpu not found")
		}

		logger.Error(err, "Failed to delete GPU")
		return nil, status.Errorf(codes.Internal, "failed to delete gpu: %v", err)
	}

	logger.Info("GPU deleted")

	return &emptypb.Empty{}, nil
}

// Compile-time check that GpuService implements GpuServiceServer.
var _ v1alpha1.GpuServiceServer = (*GpuService)(nil)
