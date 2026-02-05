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
	"testing"
	"time"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	"github.com/nvidia/nvsentinel/client-go/client/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestGPUFakeClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gpu1 := &devicev1alpha1.GPU{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gpu-1",
			ResourceVersion: "100",
		},
		Spec: devicev1alpha1.GPUSpec{UUID: "GPU-1"},
	}
	client := fake.NewSimpleClientset(gpu1)

	gpu, err := client.DeviceV1alpha1().GPUs().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Errorf("Fake client failed to retrieve GPU: %v", err)
	}
	if gpu.Name != "gpu-1" {
		t.Errorf("Expected %v, got  %v", gpu1, gpu)
	}
	if gpu.ResourceVersion != "100" {
		t.Errorf("ResourceVersion mismatch: expected 100, got %s", gpu.ResourceVersion)
	}
	if gpu.Spec.UUID != "GPU-1" {
		t.Errorf("UUID mismatch: expected GPU-1, got %s", gpu.Spec.UUID)
	}

	list, err := client.DeviceV1alpha1().GPUs().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Fake client failed to list GPUs: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("Expected 1 GPU, got %d", len(list.Items))
	}

	watchRV := list.ResourceVersion
	watcher, err := client.DeviceV1alpha1().GPUs().Watch(ctx, metav1.ListOptions{
		ResourceVersion: watchRV,
	})
	if err != nil {
		t.Fatalf("Fake client failed to Watch GPUs: %v", err)
	}
	defer watcher.Stop()

	// Simulate an Event in the background
	gpu2 := &devicev1alpha1.GPU{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gpu-2",
			ResourceVersion: "101",
		},
		Spec: devicev1alpha1.GPUSpec{UUID: "GPU-2"},
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		client.Tracker().Add(gpu2)
	}()

	select {
	case event, ok := <-watcher.ResultChan():
		if !ok {
			t.Fatal("Watch channel closed prematurely")
		}
		if event.Type != watch.Added {
			t.Errorf("Expected Added event, got %v", event.Type)
		}
		obj := event.Object.(*devicev1alpha1.GPU)
		if obj.Name != "gpu-2" {
			t.Errorf("Expected gpu-2, got %s", obj.Name)
		}
		if obj.ResourceVersion != "101" {
			t.Errorf("ResourceVersion mismatch: expected 101, got %s", obj.ResourceVersion)
		}
	case <-ctx.Done():
		t.Fatal("Timed out waiting for watch event")
	}
}
