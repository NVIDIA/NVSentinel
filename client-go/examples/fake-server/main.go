//  Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

// Package main implements a fake NVIDIA Device API server.
//
// It simulates a running device-api service over a Unix Domain Socket (UDS),
// maintaining an in-memory inventory of GPU resources and periodically
// toggling their readiness status to generate Watch events.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	pb "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"github.com/nvidia/nvsentinel/client-go/nvgrpc"
)

func main() {
	fmt.Printf("Starting server...\n")

	// Determine the connection target.
	// If the environment variable NVIDIA_DEVICE_API_TARGET is not set, use the
	// default socket path: unix:///var/run/nvidia-device-api/device-api.sock
	target := os.Getenv(nvgrpc.NvidiaDeviceAPITargetEnvVar)
	if target == "" {
		target = nvgrpc.DefaultNvidiaDeviceAPISocket
	}

	socketPath := strings.TrimPrefix(target, "unix://")
	fmt.Printf("socketPath: %s\n", socketPath)

	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		log.Fatalf("failed to create socket directory: %v", err)
	}

	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil {
			log.Fatalf("failed to remove stale socket: %v", err)
		}
	}

	lc := net.ListenConfig{}

	listener, err := lc.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", socketPath, err)
	}

	serverImpl := newFakeServer()
	go serverImpl.simulateChanges()

	srv := grpc.NewServer()
	pb.RegisterGpuServiceServer(srv, serverImpl)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		fmt.Println("\nStopping server...")
		srv.GracefulStop()
		os.Remove(socketPath)
		os.Exit(0)
	}()

	fmt.Printf("Fake Device API listening on %s\n", socketPath)

	if err := srv.Serve(listener); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

type fakeServer struct {
	pb.UnimplementedGpuServiceServer
	mu        sync.RWMutex
	gpus      []devicev1alpha1.GPU
	listeners map[chan struct{}]chan devicev1alpha1.GPU
	currentRV int
}

func newFakeServer() *fakeServer {
	s := &fakeServer{
		gpus:      make([]devicev1alpha1.GPU, 8),
		listeners: make(map[chan struct{}]chan devicev1alpha1.GPU),
	}

	for i := 0; i < 8; i++ {
		s.gpus[i] = devicev1alpha1.GPU{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("gpu-%d", i),
				ResourceVersion: "1",
			},
			Spec: devicev1alpha1.GPUSpec{
				UUID: generateGPUUUID(),
			},
			Status: devicev1alpha1.GPUStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "DriverReady",
						Message: "driver is posting ready status",
					},
				},
			},
		}
	}

	return s
}

// simulateChanges flips the Ready status of a random GPU every few seconds
// to generate events for active Watch streams.
func (s *fakeServer) simulateChanges() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()

		// Pick a random GPU to update
		idx := 0

		if len(s.gpus) > 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(s.gpus))))
			if err != nil {
				idx = 0
			} else {
				idx = int(n.Int64())
			}
		}

		gpu := &s.gpus[idx]

		// Increment ResourceVersion for K8s watch semantics
		s.currentRV++
		newRVStr := strconv.Itoa(s.currentRV)
		gpu.ResourceVersion = newRVStr

		// Toggle the Ready condition
		isReady := gpu.Status.Conditions[0].Status == metav1.ConditionTrue

		var newStatus metav1.ConditionStatus
		if isReady {
			newStatus = metav1.ConditionFalse
		} else {
			newStatus = metav1.ConditionTrue
		}

		gpu.Status.Conditions[0].Status = newStatus
		gpu.Status.Conditions[0].LastTransitionTime = metav1.Now()

		updatedGPU := *gpu.DeepCopy()

		s.mu.Unlock()

		s.broadcast(updatedGPU)
	}
}

func (s *fakeServer) broadcast(gpu devicev1alpha1.GPU) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.listeners {
		select {
		case ch <- gpu:
		default:
		}
	}
}

func (s *fakeServer) GetGpu(ctx context.Context, req *pb.GetGpuRequest) (*pb.GetGpuResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, gpu := range s.gpus {
		if req.Name == gpu.Name {
			return &pb.GetGpuResponse{Gpu: devicev1alpha1.ToProto(&gpu)}, nil
		}
	}

	return nil, status.Errorf(codes.NotFound, "gpu %q not found", req.Name)
}

func (s *fakeServer) ListGpus(ctx context.Context, req *pb.ListGpusRequest) (*pb.ListGpusResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	gpuList := &pb.GpuList{
		Items: make([]*pb.Gpu, 0, len(s.gpus)),
	}

	for _, gpu := range s.gpus {
		gpuList.Items = append(gpuList.Items, devicev1alpha1.ToProto(&gpu))
	}

	gpuList.Metadata = &pb.ListMeta{
		ResourceVersion: strconv.Itoa(s.currentRV),
	}

	return &pb.ListGpusResponse{GpuList: gpuList}, nil
}

//nolint:cyclop // This is an example; complexity is acceptable for clarity.
func (s *fakeServer) WatchGpus(req *pb.WatchGpusRequest, stream pb.GpuService_WatchGpusServer) error {
	var requestRV int
	if req.Opts.ResourceVersion != "" {
		requestRV, _ = strconv.Atoi(req.Opts.ResourceVersion)
	}

	if requestRV == 0 {
		// Send Initial State (ADDED events)
		var initial []devicev1alpha1.GPU

		s.mu.RLock()

		initial = make([]devicev1alpha1.GPU, len(s.gpus))
		for i, g := range s.gpus {
			initial[i] = *g.DeepCopy()
		}

		s.mu.RUnlock()

		for _, gpu := range initial {
			if err := stream.Send(&pb.WatchGpusResponse{
				Type:   "ADDED",
				Object: devicev1alpha1.ToProto(&gpu),
			}); err != nil {
				return err
			}
		}
	}

	// Register for updates
	updateCh := make(chan devicev1alpha1.GPU, 10)
	stopKey := make(chan struct{})

	s.mu.Lock()
	s.listeners[stopKey] = updateCh
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.listeners, stopKey)
		s.mu.Unlock()
	}()

	log.Printf("Watch stream connected (starting RV: %d)", requestRV)

	// Stream updates
	for {
		select {
		case <-stream.Context().Done():
			log.Println("Watch stream disconnected")
			return nil
		case gpu := <-updateCh:
			gpuRV, _ := strconv.Atoi(gpu.ResourceVersion)
			if gpuRV > requestRV {
				err := stream.Send(&pb.WatchGpusResponse{
					Type:   "MODIFIED",
					Object: devicev1alpha1.ToProto(&gpu),
				})
				if err != nil {
					return err
				}
			}
		}
	}
}

func generateGPUUUID() string {
	b := make([]byte, 16)

	_, err := rand.Read(b)
	if err != nil {
		return "GPU-static-uuid-on-error"
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("GPU-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
