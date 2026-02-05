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

package v1alpha1_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	"github.com/nvidia/nvsentinel/client-go/client/versioned"
	"github.com/nvidia/nvsentinel/client-go/client/versioned/scheme"
	informers "github.com/nvidia/nvsentinel/client-go/informers/externalversions"
	"github.com/nvidia/nvsentinel/client-go/nvgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

func TestClientset_EndToEnd(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()

	mock := newMockGpuServer()
	pb.RegisterGpuServiceServer(s, mock)

	go s.Serve(lis)
	defer s.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.DialContext(ctx, "bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial bufconn: %v", err)
	}
	defer conn.Close()

	config := &nvgrpc.Config{Target: "passthrough://bufconn"}
	cs, err := versioned.NewForConfigAndClient(config, conn)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	t.Run("Get", func(t *testing.T) {
		gpu, err := cs.DeviceV1alpha1().GPUs().Get(ctx, "gpu-1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if gpu.Name != "gpu-1" {
			t.Errorf("Expected gpu-1, got %s", gpu.Name)
		}
	})

	t.Run("List", func(t *testing.T) {
		list, err := cs.DeviceV1alpha1().GPUs().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}

		if len(list.Items) != 1 {
			t.Errorf("Expected 1 item in list, got %d", len(list.Items))
		}
		if list.Items[0].ResourceVersion != "7" {
			t.Errorf("Expected ResourceVersion 7, got %s", list.Items[0].ResourceVersion)
		}
	})

	t.Run("Watch flow with initial snapshot", func(t *testing.T) {
		w, err := cs.DeviceV1alpha1().GPUs().Watch(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Watch failed: %v", err)
		}
		defer w.Stop()

		// Consume the initial snapshot
		select {
		case event := <-w.ResultChan():
			if event.Type != watch.Added {
				t.Errorf("Expected initial ADDED event, got %v", event.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Timed out waiting for initial snapshot")
		}

		// Trigger event
		mock.sendEvent(&pb.WatchGpusResponse{
			Type: "MODIFIED",
			Object: &pb.Gpu{
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-1",
					ResourceVersion: "8",
				},
			},
		})

		select {
		case event := <-w.ResultChan():
			if event.Type != watch.Modified {
				t.Errorf("Expected event type MODIFIED, got %v", event.Type)
			}

			gpu := event.Object.(*devicev1alpha1.GPU)
			if gpu.ResourceVersion != "8" {
				t.Errorf("Expected ResourceVersion 8, got %s", gpu.ResourceVersion)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Timed out waiting for modified event")
		}
	})

	t.Run("Informer and Lister Sync", func(t *testing.T) {
		subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
		defer subCancel()

		factory := informers.NewSharedInformerFactory(cs, 0)

		// Informer must be instantiated BEFORE starting the factory to register it with the factory.
		gpuInformer := factory.Device().V1alpha1().GPUs()
		_ = gpuInformer.Informer()

		stopCh := make(chan struct{})
		defer close(stopCh)

		factory.Start(stopCh)

		if !cache.WaitForCacheSync(subCtx.Done(), gpuInformer.Informer().HasSynced) {
			t.Fatal("Timed out waiting for cache sync")
		}

		// Initial snapshot
		lister := gpuInformer.Lister()
		gpu, err := lister.Get("gpu-1")
		if err != nil {
			t.Fatalf("Lister failed to find gpu-1 in cache: %v", err)
		}
		if gpu.ResourceVersion != "7" {
			t.Errorf("Expected cached RV 7, got %s", gpu.ResourceVersion)
		}

		// Trigger event
		mock.sendEvent(&pb.WatchGpusResponse{
			Type: "MODIFIED",
			Object: &pb.Gpu{
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-1",
					ResourceVersion: "8",
				},
			},
		})

		err = wait.PollUntilContextTimeout(subCtx, 100*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
			updated, err := lister.Get("gpu-1")
			if err != nil {
				return false, nil
			}
			return updated.ResourceVersion == "8", nil
		})

		if err != nil {
			t.Errorf("Informer failed to update cache with new ResourceVersion: %v", err)
		}
	})

	t.Run("Controller-runtime Compatibility", func(t *testing.T) {
		factory := informers.NewSharedInformerFactory(cs, 0)
		gpuInformer := factory.Device().V1alpha1().GPUs()

		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{devicev1alpha1.SchemeGroupVersion})
		mapper.Add(devicev1alpha1.SchemeGroupVersion.WithKind("GPU"), meta.RESTScopeRoot)

		c, err := ctrlcache.New(&rest.Config{Host: "http://localhost:0"}, ctrlcache.Options{
			Scheme: scheme.Scheme,
			Mapper: mapper,
			NewInformer: func(lw cache.ListerWatcher, obj runtime.Object, resync time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
				if _, ok := obj.(*devicev1alpha1.GPU); ok {
					return gpuInformer.Informer()
				}
				return cache.NewSharedIndexInformer(lw, obj, resync, indexers)
			},
		})
		if err != nil {
			t.Fatalf("Failed to create controller-runtime cache: %v", err)
		}

		stopCh := make(chan struct{})
		defer close(stopCh)

		factory.Start(stopCh)
		go func() {
			if err := c.Start(ctx); err != nil {
				if ctx.Err() == nil {
					// Errors during Start are expected when context is cancelled during cleanup.
					t.Logf("Cache start error (may be expected): %v", err)
				}
			}
		}()

		if !c.WaitForCacheSync(ctx) {
			t.Fatal("Controller-runtime cache failed to sync")
		}

		// Initial snapshot
		var gpu devicev1alpha1.GPU
		key := client.ObjectKey{Name: "gpu-1"}
		if err := c.Get(ctx, key, &gpu); err != nil {
			t.Fatalf("Failed to read initial state from cache: %v", err)
		}
		if gpu.ResourceVersion != "7" {
			t.Errorf("Expected RV 7, got %s", gpu.ResourceVersion)
		}

		// Trigger event
		mock.sendEvent(&pb.WatchGpusResponse{
			Type: "MODIFIED",
			Object: &pb.Gpu{
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-1",
					ResourceVersion: "8",
				},
			},
		})

		err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
			var updated devicev1alpha1.GPU
			if err := c.Get(ctx, key, &updated); err != nil {
				return false, nil
			}
			return updated.ResourceVersion == "8", nil
		})

		if err != nil {
			t.Errorf("Controller-runtime cache failed to reflect gRPC event: %v", err)
		}
	})
}

// --- Mock Server Implementation ---

type mockGpuServer struct {
	pb.UnimplementedGpuServiceServer
	mu    sync.RWMutex
	gpus  map[string]*pb.Gpu
	watch chan *pb.WatchGpusResponse
}

func newMockGpuServer() *mockGpuServer {
	return &mockGpuServer{
		gpus: map[string]*pb.Gpu{
			"gpu-1": {
				Metadata: &pb.ObjectMeta{
					Name:            "gpu-1",
					ResourceVersion: "7",
				},
				Spec: &pb.GpuSpec{Uuid: "GPU-1"},
			},
		},
		watch: make(chan *pb.WatchGpusResponse, 10),
	}
}

func (m *mockGpuServer) GetGpu(ctx context.Context, req *pb.GetGpuRequest) (*pb.GetGpuResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	gpu, ok := m.gpus[req.Name]
	if !ok {
		return nil, fmt.Errorf("%s not found", req.Name)
	}
	return &pb.GetGpuResponse{Gpu: gpu}, nil
}

func (m *mockGpuServer) ListGpus(ctx context.Context, req *pb.ListGpusRequest) (*pb.ListGpusResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := &pb.GpuList{}
	for _, g := range m.gpus {
		list.Items = append(list.Items, g)
	}
	return &pb.ListGpusResponse{GpuList: list}, nil
}

func (m *mockGpuServer) WatchGpus(req *pb.WatchGpusRequest, stream pb.GpuService_WatchGpusServer) error {
	m.mu.RLock()
	// Send the initial snapshot (Current state)
	for _, g := range m.gpus {
		select {
		case <-stream.Context().Done():
			m.mu.RUnlock()
			return nil
		default:
			if err := stream.Send(&pb.WatchGpusResponse{
				Type:   "ADDED",
				Object: g,
			}); err != nil {
				m.mu.RUnlock()
				return err
			}
		}
	}
	m.mu.RUnlock()

	// Continuous watch (Live events)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-m.watch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (m *mockGpuServer) sendEvent(ev *pb.WatchGpusResponse) {
	m.watch <- ev
}
