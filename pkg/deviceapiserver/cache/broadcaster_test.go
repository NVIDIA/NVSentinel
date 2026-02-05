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

package cache

import (
	"sync"
	"testing"
	"time"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

func TestBroadcaster_SubscribeUnsubscribe(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 10)

	// Subscribe
	ch := b.Subscribe("sub-1")
	if ch == nil {
		t.Fatal("Subscribe returned nil channel")
	}

	if b.SubscriberCount() != 1 {
		t.Errorf("Expected 1 subscriber, got %d", b.SubscriberCount())
	}

	// Unsubscribe
	b.Unsubscribe("sub-1")

	if b.SubscriberCount() != 0 {
		t.Errorf("Expected 0 subscribers, got %d", b.SubscriberCount())
	}

	// Channel should be closed
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Channel should be closed after unsubscribe")
		}
	default:
		t.Error("Channel should be closed and readable")
	}
}

func TestBroadcaster_Notify(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 10)

	// Subscribe two subscribers
	ch1 := b.Subscribe("sub-1")
	ch2 := b.Subscribe("sub-2")

	// Send event
	gpu := &v1alpha1.Gpu{Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"}}
	event := WatchEvent{Type: EventTypeAdded, Object: gpu}
	b.Notify(event)

	// Both should receive
	select {
	case e := <-ch1:
		if e.Type != EventTypeAdded || e.Object.GetMetadata().GetName() != "gpu-0" {
			t.Errorf("Unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Error("Subscriber 1 did not receive event")
	}

	select {
	case e := <-ch2:
		if e.Type != EventTypeAdded || e.Object.GetMetadata().GetName() != "gpu-0" {
			t.Errorf("Unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Error("Subscriber 2 did not receive event")
	}
}

func TestBroadcaster_SlowSubscriberDoesNotBlock(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 2) // Small buffer

	// Subscribe - don't read from channel
	_ = b.Subscribe("slow-sub")
	fastCh := b.Subscribe("fast-sub")

	// Send more events than buffer size
	for i := 0; i < 10; i++ {
		gpu := &v1alpha1.Gpu{Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"}}
		b.Notify(WatchEvent{Type: EventTypeModified, Object: gpu})
	}

	// Fast subscriber should receive events (up to buffer)
	received := 0
	for {
		select {
		case <-fastCh:
			received++
		default:
			goto done
		}
	}
done:

	if received == 0 {
		t.Error("Fast subscriber should have received some events")
	}
	if received > 2 {
		t.Errorf("Fast subscriber received more than buffer size: %d", received)
	}
}

func TestBroadcaster_ConcurrentNotify(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 1000)

	// Subscribe
	ch := b.Subscribe("sub-1")

	// Concurrent writers
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				gpu := &v1alpha1.Gpu{Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"}}
				b.Notify(WatchEvent{Type: EventTypeModified, Object: gpu})
			}
		}(i)
	}

	// Concurrent reader
	received := 0
	done := make(chan struct{})
	go func() {
		for range ch {
			received++
		}
		close(done)
	}()

	wg.Wait()
	b.Close()
	<-done

	t.Logf("Received %d events (some may be dropped due to buffer)", received)
}

func TestBroadcaster_Close(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 10)

	ch1 := b.Subscribe("sub-1")
	ch2 := b.Subscribe("sub-2")

	b.Close()

	if b.SubscriberCount() != 0 {
		t.Errorf("Expected 0 subscribers after close, got %d", b.SubscriberCount())
	}

	// Channels should be closed
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("Channel 1 should be closed")
		}
	default:
		t.Error("Channel 1 should be readable (closed)")
	}

	select {
	case _, ok := <-ch2:
		if ok {
			t.Error("Channel 2 should be closed")
		}
	default:
		t.Error("Channel 2 should be readable (closed)")
	}
}

func TestBroadcaster_ResubscribeClosesOld(t *testing.T) {
	logger := klog.Background()
	b := NewBroadcaster(logger, 10)

	// First subscription
	ch1 := b.Subscribe("sub-1")

	// Resubscribe with same ID
	ch2 := b.Subscribe("sub-1")

	// First channel should be closed
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("First channel should be closed after resubscribe")
		}
	default:
		t.Error("First channel should be readable (closed)")
	}

	// Second channel should be active
	if b.SubscriberCount() != 1 {
		t.Errorf("Expected 1 subscriber, got %d", b.SubscriberCount())
	}

	// Send event - should only go to new channel
	gpu := &v1alpha1.Gpu{Metadata: &v1alpha1.ObjectMeta{Name: "gpu-0"}}
	b.Notify(WatchEvent{Type: EventTypeAdded, Object: gpu})

	select {
	case <-ch2:
		// Good
	case <-time.After(time.Second):
		t.Error("New channel should receive events")
	}
}
